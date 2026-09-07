package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// TranslateAnthropicStream converts an Anthropic Messages SSE stream into
// the OpenAI chat.completion.chunk SSE shape, so the relay, the audit log
// and kaya see one protocol regardless of upstream. The translation runs in
// a goroutine feeding an io.Pipe; closing the returned reader stops the
// goroutine (pipe error) and closes the underlying upstream body.
func TranslateAnthropicStream(body io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := translateAnthropicEvents(body, pw)
		// EOF with or without message_stop ends the pipe cleanly; a read
		// error propagates so the relay reports upstream-broken, matching
		// the OpenAI passthrough path's semantics.
		pw.CloseWithError(err)
	}()
	return &stackedReadCloser{r: pr, closers: []io.Closer{pr, body}}
}

// stackedReadCloser reads from r and closes every closer (pipe reader first,
// so the translator goroutine unblocks before the upstream body closes).
type stackedReadCloser struct {
	r       io.Reader
	closers []io.Closer
}

func (s *stackedReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *stackedReadCloser) Close() error {
	var first error
	for _, c := range s.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// anthropicEvent is the superset of the Anthropic Messages streaming event
// shapes we care about; unlisted fields are ignored.
type anthropicEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func translateAnthropicEvents(body io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(body)
	// Anthropic data lines carry full JSON events; 1 MiB covers pathological
	// tool_use inputs without unbounded allocation.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	inputTokens, outputTokens := 0, 0
	stopSeen := false
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue // event:/comment/blank lines
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // unknown/malformed event — keep the stream alive
		}
		var err error
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				inputTokens = ev.Message.Usage.InputTokens
			}
			err = writeOpenAIChunk(w, map[string]any{"role": "assistant"}, "")
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.Index,
					"id":       ev.ContentBlock.ID,
					"type":     "function",
					"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
				}}}, "")
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				err = writeOpenAIChunk(w, map[string]any{"content": ev.Delta.Text}, "")
			case "thinking_delta":
				err = writeOpenAIChunk(w, map[string]any{"reasoning_content": ev.Delta.Thinking}, "")
			case "input_json_delta":
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.Index,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}}}, "")
			}
		case "message_delta":
			if ev.Usage != nil {
				outputTokens = ev.Usage.OutputTokens // cumulative — latest wins
			}
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				err = writeOpenAIChunk(w, map[string]any{}, mapAnthropicStopReason(ev.Delta.StopReason))
			}
		case "message_stop":
			stopSeen = true
			err = writeOpenAIUsageAndDone(w, inputTokens, outputTokens)
		case "error":
			// kaya parses the {"error":...} chunk convention (same shape the
			// handler injects on upstream breaks).
			err = writeRawSSE(w, map[string]any{"error": map[string]any{"message": "upstream anthropic error"}})
		}
		if err != nil {
			return err // client (pipe reader) is gone
		}
	}
	// The stream ended without message_stop (clean EOF or an upstream read
	// error). Non-zero counters mean tokens were already consumed — emit the
	// terminal usage chunk anyway so downstream metering doesn't record 0/0.
	// Zero counters mean the provider never reported usage; emitting a 0/0
	// chunk would be indistinguishable from a real zero reading, so don't.
	// NO [DONE] here: clients stop parsing at [DONE] and would render the
	// partial answer as complete (per the client contract a missing [DONE]
	// means failure) — and on the upstream-error path the handler's
	// upstream-broke error event must be the last thing the client sees.
	if !stopSeen && (inputTokens > 0 || outputTokens > 0) {
		if err := writeOpenAIUsage(w, inputTokens, outputTokens); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// writeOpenAIChunk emits one `data: {...}\n\n` chat.completion.chunk. Empty
// finish emits JSON null; non-empty emits the string.
func writeOpenAIChunk(w io.Writer, delta map[string]any, finish string) error {
	var finishReason any
	if finish != "" {
		finishReason = finish
	}
	return writeRawSSE(w, map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	})
}

// writeOpenAIUsageAndDone emits the terminal usage chunk followed by [DONE] —
// the clean message_stop ending. [DONE] marks the answer COMPLETE, so the
// abnormal-termination flush (no message_stop) uses writeOpenAIUsage instead.
func writeOpenAIUsageAndDone(w io.Writer, inputTokens, outputTokens int) error {
	if err := writeOpenAIUsage(w, inputTokens, outputTokens); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// writeOpenAIUsage emits just the usage chunk (the shape the UsageTracker and
// ExtractStreamUsage look for) — no [DONE].
func writeOpenAIUsage(w io.Writer, inputTokens, outputTokens int) error {
	return writeRawSSE(w, map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	})
}

func writeRawSSE(w io.Writer, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal translated chunk: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func mapAnthropicStopReason(r string) string {
	switch r {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}
