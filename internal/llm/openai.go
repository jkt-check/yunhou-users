package llm

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// openaiDefaultMaxTokens 是 max_tokens 缺省值（评审安全补丁：无上限时恶意
// 订阅者可开超长生成流造成不受控上游成本；与 inference 网关设计 §7.2
// 「不允许无限输出」一致）。与 anthropicDefaultMaxTokens 保持同值。
const openaiDefaultMaxTokens = 8192

// BuildOpenAIPayload builds the upstream chat.completions body for an
// OpenAI-compatible provider. stream_options.include_usage asks the upstream
// to end the stream with a usage chunk so token metering works — DeepSeek,
// Moonshot, GLM and MiniMax all honor it; providers that ignore it simply
// omit the chunk and the request is metered with zero tokens (the usage row
// is still written, so spend never goes unrecorded structurally).
//
// max_tokens 是强制输出上限（评审安全补丁）：调用方传入硬上限；<=0 时回落
// openaiDefaultMaxTokens，绝不省略该字段。legacy /chat 的客户端请求不携带
// max_tokens（model.ChatRequest 无此字段），故不存在客户端值封顶问题。
// 兼容性假设：当前接入的 OpenAI 兼容 provider（DeepSeek/Moonshot/GLM/
// MiniMax）都接受 max_tokens——未来若接入只认 max_completion_tokens 的
// reasoning 系 provider，需要在此分流字段名。
func BuildOpenAIPayload(upstreamModel string, maxTokens int, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) ([]byte, error) {
	if maxTokens <= 0 {
		maxTokens = openaiDefaultMaxTokens
	}
	payload := map[string]any{
		"model":          upstreamModel,
		"messages":       messages,
		"stream":         true,
		"max_tokens":     maxTokens,
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
//
// This is the batch-shaped twin of UsageTracker, kept for tests — production
// metering does NOT use it (a bounded capture can miss the usage chunk);
// the relay meters via UsageTracker as bytes flow through.
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
