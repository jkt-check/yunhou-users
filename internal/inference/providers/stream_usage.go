// stream_usage.go — SSE 增量解析与流式计量（Task 8）。
//
// Adapted from the candidate branch's internal/llm/openai.go UsageTracker
// and extended per the design's non-negotiables:
//
//   - 用量解析不受内容日志截断影响：the tap meters the byte flow directly;
//     nothing about audit-log capture caps can lose a usage chunk.
//   - 按协议结束事件判断完整结束：Terminal() is true ONLY after the
//     protocol end marker ([DONE] for OpenAI, message_stop for Anthropic) —
//     a bare EOF is an interruption, never a completion.
//   - 缺失用量不记零：Buckets stay nil when the upstream reported nothing;
//     SawUsage=false distinguishes "no usage reported" from "reported 0".
//   - 畸形事件容错：a malformed data line never wedges the tracker — it is
//     skipped and scanning resumes at the next line.

package providers

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"

	"github.com/yunhou/users/internal/inference/domain"
)

// TapResult is the metering state of one stream at a point in time.
type TapResult struct {
	// Buckets are the RAW upstream usage totals (overlap normalization —
	// reasoning-inside-output etc. — is applied later via the adapter's
	// Inclusion and accounting.BillableBuckets). Nil bucket = not reported.
	Buckets domain.UsageBuckets
	// Raw is the last raw usage payload seen, schema-versioned for the
	// usage_records.raw_usage column.
	Raw json.RawMessage
	// SawUsage distinguishes "the upstream reported usage" from silence.
	SawUsage bool
	// Terminal is true only after the protocol end marker ([DONE] /
	// message_stop) — never on a bare EOF.
	Terminal bool
	// ContentBytes counts visible answer bytes (delta content + reasoning)
	// relayed so far — the basis of the estimate fallback when the upstream
	// never reports usage (设计 §7.1 estimated 路径).
	ContentBytes int64
}

// UsageTap is the per-attempt incremental metering surface. For byte
// passthrough protocols Feed consumes every upstream read; the Anthropic
// translator owns its tap internally (Feed is a no-op there) because it
// meters the RAW Anthropic events before translation.
type UsageTap interface {
	Feed(p []byte)
	Result() TapResult
}

// Stream is one upstream SSE response normalized to the OpenAI chunk shape:
// Body yields `data: ...` frames (translated when the upstream speaks
// Anthropic); Tap meters usage as bytes flow.
type Stream struct {
	Body io.ReadCloser
	Tap  UsageTap
}

// teeReader wraps a stream body so every Read feeds the tap before the
// caller sees the bytes — metering can never be skipped by the relay, and
// usage parsing is independent of any downstream capture cap (设计: 用量解
// 析不受内容日志截断影响).
type teeReader struct {
	body io.ReadCloser
	tap  UsageTap
}

// TeeStream wraps s.Body so reads feed s.Tap.
func (s *Stream) TeeBody() io.ReadCloser {
	return &teeReader{body: s.Body, tap: s.Tap}
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		t.tap.Feed(p[:n])
	}
	return n, err
}

func (t *teeReader) Close() error { return t.body.Close() }

// ---------------------------------------------------------------------------
// OpenAI incremental usage tracker
// ---------------------------------------------------------------------------

// trackLineCap bounds the pending (unterminated) line. Usage chunks are a
// few hundred bytes; a longer line is a big content delta that cannot be a
// usage chunk worth buffering — it is dropped and scanning resumes at the
// next newline (candidate-branch semantics, kept).
const trackLineCap = 64 << 10

// openAIUsage is the OpenAI usage object shape, including the detail
// sub-objects that carry cache/reasoning breakdowns.
type openAIUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// OpenAIUsageTracker incrementally meters an OpenAI-format SSE stream as
// its bytes flow through the relay: Feed every upstream read; Result returns
// the last-seen usage object (LAST wins, so cumulative-reporting providers
// are handled correctly). Adapted from the candidate UsageTracker and
// extended with: detail sub-objects (cached/reasoning), [DONE] terminal
// tracking and visible-content byte counting for the estimate path.
type OpenAIUsageTracker struct {
	pending []byte
	res     TapResult
}

// Feed consumes one upstream read. Lines are reassembled across reads (the
// relay buffer has no line alignment). Only complete `data:` lines
// containing "usage", "content" or [DONE] cost more than a prefix scan.
func (t *OpenAIUsageTracker) Feed(p []byte) {
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		end := len(p)
		if nl >= 0 {
			end = nl
		}
		if len(t.pending)+end > trackLineCap {
			t.pending = t.pending[:0] // overlong line: drop, never wedge
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

// Result returns the metering state so far.
func (t *OpenAIUsageTracker) Result() TapResult { return t.res }

func (t *OpenAIUsageTracker) scanLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		t.res.Terminal = true
		return
	}
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	// Cheap gates before a full JSON parse. A content decoy containing the
	// quoted word "usage" still fails the shape check below (candidate test
	// TestUsageTracker_ContentMentioningUsageIgnored pins this).
	wantUsage := bytes.Contains(line, []byte(`"usage"`))
	wantContent := bytes.Contains(line, []byte(`"content"`))
	if !wantUsage && !wantContent {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return // malformed event: skip, keep the stream alive
	}
	for _, ch := range chunk.Choices {
		t.res.ContentBytes += int64(len(ch.Delta.Content) + len(ch.Delta.ReasoningContent))
	}
	if chunk.Usage != nil {
		t.res.SawUsage = true
		t.res.Raw = append(t.res.Raw[:0], payload...)
		t.res.Buckets = domain.UsageBuckets{
			InputTokens:      chunk.Usage.PromptTokens,
			OutputTokens:     chunk.Usage.CompletionTokens,
			CacheReadTokens:  nil,
			ReasoningTokens:  nil,
			CacheWriteTokens: nil,
		}
		if chunk.Usage.PromptDetails != nil {
			t.res.Buckets.CacheReadTokens = chunk.Usage.PromptDetails.CachedTokens
		}
		if chunk.Usage.CompletionDetails != nil {
			t.res.Buckets.ReasoningTokens = chunk.Usage.CompletionDetails.ReasoningTokens
		}
	}
}

// ---------------------------------------------------------------------------
// Anthropic stream tap (owned by the translator in anthropic_stream.go)
// ---------------------------------------------------------------------------

// anthropicTap meters the RAW Anthropic event stream inside the translator
// goroutine, so a client disconnect mid-translation can never lose usage
// already read from upstream (Task 8: 中途断开走中断语义，已读用量保留).
type anthropicTap struct {
	mu  sync.Mutex
	res TapResult
}

func (t *anthropicTap) Feed(p []byte) { /* the translator meters internally */ }

func (t *anthropicTap) Result() TapResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	res := t.res
	if t.res.Raw != nil {
		res.Raw = append(json.RawMessage(nil), t.res.Raw...)
	}
	return res
}

func (t *anthropicTap) addContent(n int) {
	t.mu.Lock()
	t.res.ContentBytes += int64(n)
	t.mu.Unlock()
}

// recordUsage stores the latest cumulative counters (message_start input +
// cache buckets; message_delta cumulative output — latest wins, matching
// Anthropic semantics). raw is the schema-versioned merged view for
// raw_usage.
func (t *anthropicTap) recordUsage(input, output, cacheRead, cacheWrite *int64, raw json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.res.SawUsage = true
	t.res.Buckets = domain.UsageBuckets{
		InputTokens:      input,
		OutputTokens:     output,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
	}
	if len(raw) > 0 {
		t.res.Raw = append(t.res.Raw[:0], raw...)
	}
}

func (t *anthropicTap) markTerminal() {
	t.mu.Lock()
	t.res.Terminal = true
	t.mu.Unlock()
}
