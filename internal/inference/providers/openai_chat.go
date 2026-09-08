// openai_chat.go — OpenAI Chat Completions 协议适配器（Task 8）。
//
// Adapted from the candidate branch internal/llm/openai.go
// (BuildOpenAIPayload + UsageTracker, 基线报告 §3.2). Kept and extended:
//   - stream_options.include_usage is ALWAYS set on streaming calls (设计
//     §7.1: 适配器必须显式请求流式 usage；上游确实不提供才进入 estimated/
//     unknown 路径 — 适配器不请求而把可获得的用量记为未知是被禁止的).
//   - The forced output cap is forwarded as max_tokens (设计 §7.2: 不允许
//     无限输出).
//   - usage parsing keeps nil-vs-zero fidelity and full bucket detail
//     (cached/reasoning), instead of the candidate's int-pair semantics.

package providers

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// OpenAIChat is the adapter for openai_chat deployments (OpenAI,
// DeepSeek, Moonshot, GLM, MiniMax and other OpenAI-compatible endpoints).
type OpenAIChat struct{}

// NewOpenAIChat builds the adapter.
func NewOpenAIChat() *OpenAIChat { return &OpenAIChat{} }

// Protocol implements Adapter.
func (a *OpenAIChat) Protocol() domain.Protocol { return domain.ProtocolOpenAIChat }

// Capabilities implements Adapter. include_usage is always requested, so
// streaming usage is available whenever the upstream honors it.
func (a *OpenAIChat) Capabilities() domain.AdapterCapabilities {
	return domain.AdapterCapabilities{StreamingUsage: true, Tools: true, Reasoning: true}
}

// Inclusion implements Adapter: OpenAI's prompt_tokens already contains
// cached_tokens, and completion_tokens already contains reasoning_tokens —
// accounting subtracts/folds accordingly so nothing is double-charged
// (设计 §7.1).
func (a *OpenAIChat) Inclusion() accounting.Inclusion {
	return accounting.Inclusion{ReasoningInOutput: true, CacheReadInInput: true}
}

// passthroughKeys are the sampling parameters this adapter relays verbatim.
var passthroughKeys = []string{"temperature", "top_p", "stop", "presence_penalty", "frequency_penalty", "seed"}

// BuildPayload implements Adapter. Adapted from the candidate's
// BuildOpenAIPayload: same payload keys, plus the forced max_tokens cap,
// tool_choice and the passthrough allowlist.
func (a *OpenAIChat) BuildPayload(call *Call) ([]byte, error) {
	r := call.Request
	payload := map[string]any{
		"model":      call.Deployment.UpstreamModel,
		"messages":   r.Messages,
		"stream":     r.Stream,
		"max_tokens": call.OutputCap,
	}
	if r.Stream {
		// 显式请求流式 usage（设计 §7.1）。Providers that ignore it omit the
		// chunk; metering then falls to the estimated/unknown path — never
		// silently zero.
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(r.Tools) > 0 {
		payload["tools"] = r.Tools
	}
	if len(r.ToolChoice) > 0 {
		payload["tool_choice"] = r.ToolChoice
	}
	if r.ThinkingEnabled != nil && *r.ThinkingEnabled {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
	for _, k := range passthroughKeys {
		if v, ok := r.Passthrough[k]; ok {
			payload[k] = v
		}
	}
	return json.Marshal(payload)
}

// AuthHeaders implements Adapter.
func (a *OpenAIChat) AuthHeaders(secret []byte) map[string]string {
	if len(secret) == 0 {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + string(secret)}
}

// WrapStream implements Adapter: the upstream already speaks OpenAI SSE, so
// the body passes through untouched; the incremental tracker meters it.
func (a *OpenAIChat) WrapStream(body io.ReadCloser) *Stream {
	return &Stream{Body: body, Tap: &OpenAIUsageTracker{}}
}

// openAINonStream is the non-streaming chat.completion shape we read usage
// from. The payload is relayed to the client untouched — this struct is a
// metering view only. Choices carry the visible content so a missing usage
// object can still be ESTIMATED from what was actually served (never 0).
type openAINonStream struct {
	ID      string       `json:"id"`
	Usage   *openAIUsage `json:"usage"`
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

// DecodeNonStream implements Adapter.
func (a *OpenAIChat) DecodeNonStream(body []byte) (*NonStreamResult, error) {
	var resp openAINonStream
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("providers: decode openai chat.completion: %w", err)
	}
	out := &NonStreamResult{Payload: body, UpstreamRequestID: resp.ID}
	for _, ch := range resp.Choices {
		out.ContentBytes += int64(len(ch.Message.Content) + len(ch.Message.ReasoningContent))
	}
	if resp.Usage == nil {
		// The upstream genuinely did not report usage on a non-streaming
		// call — the estimated path handles it (设计 §7.1).
		return out, nil
	}
	out.UsageReported = true
	out.UsageRaw = rawUsageFromPayload(body, resp.Usage)
	out.Usage = domain.UsageBuckets{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}
	if resp.Usage.PromptDetails != nil {
		out.Usage.CacheReadTokens = resp.Usage.PromptDetails.CachedTokens
	}
	if resp.Usage.CompletionDetails != nil {
		out.Usage.ReasoningTokens = resp.Usage.CompletionDetails.ReasoningTokens
	}
	return out, nil
}

// rawUsageFromPayload wraps the usage sub-object of a full payload into the
// schema-versioned raw_usage document (prompts/completions are never
// retained, 设计 §7.1).
func rawUsageFromPayload(full []byte, usage *openAIUsage) json.RawMessage {
	raw, err := json.Marshal(struct {
		SchemaVersion int          `json:"schema_version"`
		Usage         *openAIUsage `json:"usage"`
	}{SchemaVersion: 1, Usage: usage})
	if err != nil {
		return json.RawMessage(`{"schema_version":1}`)
	}
	return raw
}
