// client_surface.go — 客户端协议面的共享翻译件（Task 13）。
//
// The gateway's internal wire shape is ALWAYS the OpenAI chat.completion
// (non-stream) / chat.completion.chunk SSE (stream), regardless of upstream
// protocol (see anthropic_stream.go). The Messages / Responses client
// surfaces therefore need ONE shared incremental parser for that chunk
// shape, and per-protocol state machines that render native events
// (messages_client.go / responses_client.go).
//
// Termination semantics are uniform across surfaces: [DONE] is the ONLY
// clean end; a bare EOF / read error / injected {"error":...} chunk means
// the stream broke — the protocol translators emit their native terminal
// error event and never a fake completion marker (与 chat 面同一约定：
// 客户端在结束标记处停读，半截答案绝不当成完整答案渲染).

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// openAIStreamChunk is the internal wire chunk the client-surface
// translators consume (superset of what openai_chat/anthropic_stream emit).
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	// Error is the gateway-injected mid-stream error chunk convention
	// (chat 面 relayStream 在上游中断时注入的 {"error":...} data 块).
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// pumpEvent is one parsed line of the internal SSE stream.
type pumpEvent struct {
	chunk *openAIStreamChunk
	done  bool // [DONE] observed — the ONLY clean terminal marker
}

// chunkPump incrementally reassembles and parses `data:` lines (the relay
// buffer has no line alignment), mirroring OpenAIUsageTracker's discipline:
// overlong lines are dropped (never wedge), malformed payloads are skipped.
type chunkPump struct {
	pending []byte
}

// Feed consumes one read and returns every complete event it unblocks.
func (p *chunkPump) Feed(b []byte) []pumpEvent {
	var out []pumpEvent
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		end := len(b)
		if nl >= 0 {
			end = nl
		}
		if len(p.pending)+end > trackLineCap {
			p.pending = p.pending[:0]
			if nl < 0 {
				return out
			}
			b = b[nl+1:]
			continue
		}
		p.pending = append(p.pending, b[:end]...)
		if nl < 0 {
			return out
		}
		if ev, ok := p.scanLine(p.pending); ok {
			out = append(out, ev)
		}
		p.pending = p.pending[:0]
		b = b[nl+1:]
	}
	return out
}

func (p *chunkPump) scanLine(line []byte) (pumpEvent, bool) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return pumpEvent{}, false // event:/comment/blank lines
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		return pumpEvent{done: true}, true
	}
	if len(payload) == 0 || payload[0] != '{' {
		return pumpEvent{}, false
	}
	var chunk openAIStreamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return pumpEvent{}, false // malformed event: skip, keep scanning
	}
	return pumpEvent{chunk: &chunk}, true
}

// completionView is the metering/content view over a non-streaming internal
// chat.completion payload (the same fields openAINonStream reads, plus
// tool_calls for client-surface reconstruction).
type completionView struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Usage   *openAIUsage
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// parseCompletion decodes the internal non-stream payload for client-surface
// reconstruction. An absent choices array is an internal-shape error (the
// gateway only hands us adapter-validated payloads, so this is 500-class).
func parseCompletion(payload []byte) (*completionView, error) {
	var v completionView
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, fmt.Errorf("providers: decode internal completion: %w", err)
	}
	return &v, nil
}

// streamRenderer is one client-surface state machine: Render consumes each
// parsed internal event; BrokenEnd emits the native terminal error event
// when the stream ended WITHOUT [DONE] (bare EOF / read error). A renderer
// that already relayed an in-band {"error":...} chunk suppresses a
// duplicate in BrokenEnd.
type streamRenderer interface {
	Render(ev pumpEvent, w io.Writer) error
	BrokenEnd(w io.Writer) error
}

// translateStream runs the renderer over the internal OpenAI-shaped SSE
// body, writing the native protocol byte stream into the pipe. The source
// is always closed; the pipe propagates the render error (client gone).
// [DONE] is the only clean end — anything else routes to BrokenEnd, which
// never emits a fake completion marker (与 chat 面同一约定).
func translateStream(src io.ReadCloser, r streamRenderer) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer src.Close()
		pump := &chunkPump{}
		buf := make([]byte, 32<<10)
		terminal := false
		for {
			n, readErr := src.Read(buf)
			if n > 0 {
				for _, ev := range pump.Feed(buf[:n]) {
					if ev.done {
						terminal = true
					}
					if err := r.Render(ev, pw); err != nil {
						pw.CloseWithError(err)
						return
					}
				}
			}
			if readErr != nil {
				if !terminal {
					if err := r.BrokenEnd(pw); err != nil {
						pw.CloseWithError(err)
						return
					}
				}
				pw.CloseWithError(nil)
				return
			}
		}
	}()
	return &stackedReadCloser{r: pr, closers: []io.Closer{pr, src}}
}

// writeSSE writes one `event: <name>\ndata: <json>\n\n` frame. An empty
// event name omits the event line (OpenAI-style data-only frames).
func writeSSE(w io.Writer, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal client event: %w", err)
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// newPublicID renders the protocol's public id shape (msg_/resp_/fc_/...).
func newPublicID(prefix string) string {
	return prefix + uuid.NewString()
}

// NewPublicMessageID returns the public Messages response id (msg_...).
func NewPublicMessageID() string { return newPublicID("msg_") }

// NewPublicResponseID returns the public Responses object id (resp_...).
func NewPublicResponseID() string { return newPublicID("resp_") }

// derefOr returns *p or the fallback.
func derefOr(p *int64, fallback int64) int64 {
	if p == nil {
		return fallback
	}
	return *p
}

// strOr returns v unless empty.
func strOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
