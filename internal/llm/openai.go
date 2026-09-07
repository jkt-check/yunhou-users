package llm

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// BuildOpenAIPayload builds the upstream chat.completions body for an
// OpenAI-compatible provider. stream_options.include_usage asks the upstream
// to end the stream with a usage chunk so token metering works — DeepSeek,
// Moonshot, GLM and MiniMax all honor it; providers that ignore it simply
// omit the chunk and the request is metered with zero tokens (the usage row
// is still written, so spend never goes unrecorded structurally).
func BuildOpenAIPayload(upstreamModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error) {
	payload := map[string]any{
		"model":          upstreamModel,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if thinkingEnabled != nil && *thinkingEnabled {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
	return json.Marshal(payload)
}

// ExtractStreamUsage scans a captured OpenAI-format SSE stream for the
// terminal usage chunk (choices: [], usage: {...}) and returns the token
// counts. The LAST usage chunk wins so cumulative-reporting providers are
// handled correctly. ok=false when no chunk carried usage.
func ExtractStreamUsage(raw []byte) (inputTokens, outputTokens int, ok bool) {
	for _, block := range strings.Split(string(raw), "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Usage != nil {
			inputTokens, outputTokens, ok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, true
		}
	}
	return inputTokens, outputTokens, ok
}

// StreamUsage is the last usage object seen in an OpenAI-format SSE stream.
type StreamUsage struct {
	InputTokens  int
	OutputTokens int
	OK           bool // false when no chunk carried usage
}

// UsageTracker incrementally meters an OpenAI-format SSE stream as its bytes
// flow through the relay: Feed every upstream read, then Usage returns the
// last-seen usage object (LAST wins, matching ExtractStreamUsage). Unlike a
// post-hoc scan of the captured copy, it is independent of the audit-log
// capture cap and of how the relay ends — clean end, client disconnect or
// upstream break all meter every usage chunk read from upstream.
type UsageTracker struct {
	pending []byte // current unterminated line
	usage   StreamUsage
}

// usageTrackLineCap bounds the pending (unterminated) line. Usage chunks are
// a few hundred bytes; a longer line is a big content delta that cannot be a
// usage chunk worth buffering, so it is dropped and scanning resumes at the
// next newline.
const usageTrackLineCap = 64 << 10

// Feed consumes one upstream read. Lines are reassembled across reads (the
// relay buffer has no line alignment). Only complete `data:` lines containing
// `"usage"` are JSON-parsed — everything else costs a substring scan.
func (t *UsageTracker) Feed(p []byte) {
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		end := len(p)
		if nl >= 0 {
			end = nl
		}
		if len(t.pending)+end > usageTrackLineCap {
			t.pending = t.pending[:0] // overlong line: drop
			if nl < 0 {
				return
			}
			p = p[nl+1:]
			continue
		}
		t.pending = append(t.pending, p[:end]...)
		if nl < 0 {
			return
		}
		t.scanLine(t.pending)
		t.pending = t.pending[:0]
		p = p[nl+1:]
	}
}

// Usage returns the last usage object seen so far (zero value when none).
func (t *UsageTracker) Usage() StreamUsage { return t.usage }

func (t *UsageTracker) scanLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	var chunk struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) == nil && chunk.Usage != nil {
		t.usage = StreamUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			OK:           true,
		}
	}
}
