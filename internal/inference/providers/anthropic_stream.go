// anthropic_stream.go — Anthropic Messages SSE → OpenAI chunk SSE 流式翻译
// (Task 8)。
//
// Adapted from the candidate branch internal/llm/anthropic_stream.go
// (TranslateAnthropicStream, 基线报告 §3.2). Semantics kept verbatim:
//   - message_delta carries cumulative usage — latest wins;
//   - abnormal termination (EOF/read error without message_stop) flushes the
//     usage seen so far as a terminal usage chunk WITHOUT [DONE], so clients
//     never render a partial answer as complete;
//   - malformed/unknown events are skipped, never wedging the stream.
//
// Task 8 extension: the translator owns an internal usage tap metering the
// RAW Anthropic events (usage + content bytes + message_stop), so metering
// survives a client disconnect mid-translation and is independent of any
// relay/capture buffer (设计: 用量解析不受内容日志截断影响；按协议结束事件
// 判断完整结束).

package providers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/yunhou/users/internal/inference/domain"
)

// TranslateAnthropicStream converts an Anthropic Messages SSE stream into
// the OpenAI chat.completion.chunk SSE shape, so the relay and clients see
// one protocol regardless of upstream. The translation runs in a goroutine
// feeding an io.Pipe; closing the returned stream's Body stops the goroutine
// (pipe error) and closes the underlying upstream body.
func TranslateAnthropicStream(body io.ReadCloser) *Stream {
	tap := &anthropicTap{}
	pr, pw := io.Pipe()
	go func() {
		err := translateAnthropicEvents(body, pw, tap)
		// EOF with or without message_stop ends the pipe cleanly; a read
		// error propagates so the relay reports upstream-broken, matching
		// the OpenAI passthrough path's semantics.
		pw.CloseWithError(err)
	}()
	return &Stream{
		Body: &stackedReadCloser{r: pr, closers: []io.Closer{pr, body}},
		Tap:  tap,
	}
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
		ID    string `json:"id"`
		Usage *struct {
			InputTokens         *int64 `json:"input_tokens"`
			OutputTokens        *int64 `json:"output_tokens"`
			CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
			CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
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
		OutputTokens *int64 `json:"output_tokens"`
	} `json:"usage"`
}

func translateAnthropicEvents(body io.Reader, w io.Writer, tap *anthropicTap) error {
	scanner := bufio.NewScanner(body)
	// Anthropic data lines carry full JSON events; 1 MiB covers pathological
	// tool_use inputs without unbounded allocation.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	var input, output, cacheRead, cacheWrite *int64
	flushUsage := func() {
		// message_start already reported input/cache; message_delta carries
		// cumulative output. The tap always holds the latest merged view.
		if input != nil || output != nil || cacheRead != nil || cacheWrite != nil {
			tap.recordUsage(input, output, cacheRead, cacheWrite,
				marshalAnthropicRawUsage(input, output, cacheRead, cacheWrite))
		}
	}
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
				input = ev.Message.Usage.InputTokens
				cacheRead = ev.Message.Usage.CacheReadTokens
				cacheWrite = ev.Message.Usage.CacheCreationTokens
				flushUsage()
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
				tap.addContent(len(ev.Delta.Text))
				err = writeOpenAIChunk(w, map[string]any{"content": ev.Delta.Text}, "")
			case "thinking_delta":
				tap.addContent(len(ev.Delta.Thinking))
				err = writeOpenAIChunk(w, map[string]any{"reasoning_content": ev.Delta.Thinking}, "")
			case "input_json_delta":
				err = writeOpenAIChunk(w, map[string]any{"tool_calls": []any{map[string]any{
					"index":    ev.Index,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}}}, "")
			}
		case "message_delta":
			if ev.Usage != nil && ev.Usage.OutputTokens != nil {
				output = ev.Usage.OutputTokens // cumulative — latest wins
				flushUsage()
			}
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				err = writeOpenAIChunk(w, map[string]any{}, mapAnthropicStopReason(ev.Delta.StopReason))
			}
		case "message_stop":
			stopSeen = true
			tap.markTerminal()
			// 评审轮2 M5：上游从未报 usage 时只发 [DONE]——缺失 usage 绝不
			// 渲染为 {"total_tokens":0}（与下方 !stopSeen 分支同一纪律）。
			if res := tap.Result(); res.SawUsage {
				err = writeOpenAIUsageAndDone(w, res.Buckets)
			} else {
				_, err = io.WriteString(w, "data: [DONE]\n\n")
			}
		case "error":
			// kaya/OpenAI clients parse the {"error":...} chunk convention
			// (the same shape the gateway injects on upstream breaks).
			err = writeRawSSE(w, map[string]any{"error": map[string]any{"message": "upstream anthropic error"}})
		}
		if err != nil {
			return err // client (pipe reader) is gone
		}
	}
	// The stream ended without message_stop (clean EOF or an upstream read
	// error). Non-nil counters mean tokens were already consumed — emit the
	// terminal usage chunk anyway so downstream metering sees them (but the
	// tap already holds them regardless of whether this write succeeds).
	// NO [DONE] here: clients stop parsing at [DONE] and would render the
	// partial answer as complete — and on the upstream-error path the
	// gateway's upstream-broke error event must be the last thing the client
	// sees.
	if !stopSeen {
		if b := tap.Result(); b.SawUsage {
			if err := writeOpenAIUsage(w, b.Buckets); err != nil {
				return err
			}
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
func writeOpenAIUsageAndDone(w io.Writer, b domain.UsageBuckets) error {
	if err := writeOpenAIUsage(w, b); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// writeOpenAIUsage emits just the usage chunk (the shape an OpenAI
// UsageTracker looks for) — no [DONE]. Nil buckets render as absent keys;
// a bucket the upstream never reported is never written as 0.
func writeOpenAIUsage(w io.Writer, b domain.UsageBuckets) error {
	usage := map[string]any{}
	var total int64
	if b.InputTokens != nil {
		usage["prompt_tokens"] = *b.InputTokens
		total += *b.InputTokens
	}
	if b.OutputTokens != nil {
		usage["completion_tokens"] = *b.OutputTokens
		total += *b.OutputTokens
	}
	if b.CacheReadTokens != nil || b.CacheWriteTokens != nil {
		details := map[string]any{}
		if b.CacheReadTokens != nil {
			details["cached_tokens"] = *b.CacheReadTokens
		}
		usage["prompt_tokens_details"] = details
	}
	if b.CacheWriteTokens != nil {
		// Anthropic-origin cache creation has no chat-completions home; the
		// extension key keeps it on the wire for the Messages surface (Task
		// 13) and is ignored by OpenAI-shape consumers.
		usage["cache_creation_input_tokens"] = *b.CacheWriteTokens
	}
	usage["total_tokens"] = total
	return writeRawSSE(w, map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{},
		"usage":   usage,
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
