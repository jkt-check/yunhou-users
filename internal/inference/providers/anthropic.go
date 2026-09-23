// anthropic.go — Anthropic Messages 协议适配器（Task 8）。
//
// Request translation is adapted from the candidate branch
// internal/llm/anthropic.go (BuildAnthropicPayload: user/assistant
// alternation merge, tool_use/tool_result translation, illegal-shape 400s),
// extended with:
//   - non-streaming support (stream=false) and a response translator that
//     renders an OpenAI-shaped chat.completion for the client;
//   - the forced output cap as max_tokens (Anthropic REQUIRES max_tokens —
//     the gateway's forced cap guarantees one exists, 设计 §7.2);
//   - usage normalization for the Messages usage shape (input_tokens
//     excludes cache buckets; cache_read/cache_creation carried separately).

package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// AnthropicMessages is the adapter for anthropic_messages deployments
// (Anthropic API, Kimi for Coding, GLM Coding Plan, MiniMax /anthropic).
type AnthropicMessages struct{}

// NewAnthropicMessages builds the adapter.
func NewAnthropicMessages() *AnthropicMessages { return &AnthropicMessages{} }

// Protocol implements Adapter.
func (a *AnthropicMessages) Protocol() domain.Protocol { return domain.ProtocolAnthropicMessage }

// Capabilities implements Adapter. Anthropic streams report usage via
// message_start / message_delta — explicitly part of the protocol, so
// streaming usage is always requested by construction.
func (a *AnthropicMessages) Capabilities() domain.AdapterCapabilities {
	return domain.AdapterCapabilities{StreamingUsage: true, Tools: true, Reasoning: true}
}

// Inclusion implements Adapter: Anthropic's input_tokens does NOT include
// the cache buckets (they are separate fields), and output_tokens already
// contains any thinking tokens (Anthropic does not report reasoning
// separately), so nothing is re-added (设计 §7.1 重叠语义).
func (a *AnthropicMessages) Inclusion() accounting.Inclusion {
	return accounting.Inclusion{ReasoningInOutput: true}
}

// anthropicThinkingBudget must stay strictly below max_tokens (Anthropic
// rejects max_tokens <= thinking.budget_tokens; the protocol floor is
// anthropicMinThinkingBudget — messages_client.go).
const anthropicThinkingBudget int64 = 4096

// anthropicVersionHeader is the pinned Messages API version.
const anthropicVersionHeader = "2023-06-01"

// BuildPayload implements Adapter. Adapted from the candidate's
// BuildAnthropicPayload — same alternation merge and tool translation —
// with the stream flag and the gateway-forced output cap as parameters.
func (a *AnthropicMessages) BuildPayload(call *Call) ([]byte, error) {
	r := call.Request
	maxTokens := call.OutputCap
	if maxTokens <= 0 {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"providers: anthropic dispatch requires a positive output cap (不允许无限输出)")
	}

	var systemParts []string
	var msgs []map[string]any
	for _, m := range r.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
		case "user":
			block := map[string]any{"type": "text", "text": m.Content}
			// Anthropic requires strict user/assistant alternation (400
			// otherwise): merge into the previous user turn. Text goes after
			// any tool_result blocks already there — tool_results lead.
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
				msgs[n-1]["content"] = append(msgs[n-1]["content"].([]any), block)
				continue
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{block}})
		case "assistant":
			var blocks []any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil || input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			// Anthropic rejects empty content arrays; an assistant turn with
			// neither text nor tool calls carries no information.
			if len(blocks) == 0 {
				continue
			}
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "assistant" {
				msgs[n-1]["content"] = append(msgs[n-1]["content"].([]any), blocks...)
				continue
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}
			// tool_result blocks must lead a user turn; consecutive OpenAI
			// tool messages merge into one user message.
			if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
				arr := msgs[n-1]["content"].([]any)
				insert := 0
				for insert < len(arr) {
					b, ok := arr[insert].(map[string]any)
					if !ok || b["type"] != "tool_result" {
						break
					}
					insert++
				}
				arr = append(arr, nil)
				copy(arr[insert+1:], arr[insert:])
				arr[insert] = block
				msgs[n-1]["content"] = arr
				continue
			}
			msgs = append(msgs, map[string]any{"role": "user", "content": []any{block}})
		default:
			return nil, domain.NewError(domain.CodeInvalidInput,
				fmt.Sprintf("providers: unsupported role %q for anthropic translation", m.Role))
		}
	}

	// Two client-legal shapes are hard 400s for stock Anthropic — fail
	// deliberately here, before a paid upstream round-trip buys an opaque
	// error: a system-only request translates to no messages at all, and a
	// leading assistant turn violates user-first.
	if len(msgs) == 0 {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"providers: anthropic translation: no non-system messages")
	}
	if msgs[0]["role"] != "user" {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"providers: anthropic translation: first non-system message must be a user turn")
	}

	payload := map[string]any{
		"model":      call.Deployment.UpstreamModel,
		"max_tokens": maxTokens,
		"stream":     r.Stream,
		"messages":   msgs,
	}
	if len(systemParts) > 0 {
		payload["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(r.Tools) > 0 {
		tools, err := translateAnthropicTools(r.Tools)
		if err != nil {
			return nil, err
		}
		if len(tools) > 0 {
			payload["tools"] = tools
		}
	}
	if len(r.ToolChoice) > 0 {
		tc, err := translateAnthropicToolChoice(r.ToolChoice)
		if err != nil {
			return nil, err
		}
		if tc != nil {
			payload["tool_choice"] = tc
		}
	}
	if r.ThinkingEnabled != nil && *r.ThinkingEnabled {
		budget := anthropicThinkingBudget
		if r.ThinkingBudget != nil && *r.ThinkingBudget > 0 {
			budget = *r.ThinkingBudget
		}
		// 评审轮1 I5：绝不把 max_tokens 提到客户声明的 OutputCap 之上（旧行
		// 为 maxTokens = budget*2 会让上游产出远超声明上限、按真实用量多
		// 收）。预算下调到 cap 之内；Anthropic 要求 budget_tokens >= 1024 且
		// 严格小于 max_tokens，cap 容不下合法预算时显式 400（能力错误，客
		// 户提高 max_tokens 或关闭 thinking）。
		if maxTokens <= anthropicMinThinkingBudget+1 {
			return nil, domain.NewError(domain.CodeInvalidInput,
				fmt.Sprintf("providers: anthropic thinking requires max_tokens > %d (budget floor %d); raise max_tokens or disable thinking", anthropicMinThinkingBudget+1, anthropicMinThinkingBudget))
		}
		if budget > maxTokens-1 {
			budget = maxTokens - 1
		}
		payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	// Passthrough mapping: temperature/top_p/stop map onto Anthropic's
	// names; parallel_tool_calls=false folds into tool_choice as
	// disable_parallel_tool_use; the remaining OpenAI-only knobs are
	// rejected explicitly instead of silently dropped (设计: 不能静默丢字段).
	for k, v := range r.Passthrough {
		switch k {
		case "temperature", "top_p":
			payload[k] = v
		case "top_k":
			payload["top_k"] = v
		case "stop":
			payload["stop_sequences"] = v
		case "parallel_tool_calls":
			enabled, _ := v.(bool)
			if enabled {
				continue
			}
			tc, _ := payload["tool_choice"].(map[string]any)
			if tc == nil {
				tc = map[string]any{"type": "auto"}
				payload["tool_choice"] = tc
			}
			tc["disable_parallel_tool_use"] = true
		default:
			return nil, domain.NewError(domain.CodeInvalidInput,
				"providers: parameter "+k+" is not supported by the anthropic protocol")
		}
	}
	return json.Marshal(payload)
}

// translateAnthropicTools maps the OpenAI tool envelope
// ({"type":"function","function":{...}}) onto the Anthropic shape; an
// already-flat shape is accepted too. A nameless tool would 400 the whole
// upstream request, so it is rejected here.
func translateAnthropicTools(tools []json.RawMessage) ([]any, error) {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		var tool map[string]any
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, domain.WrapError(domain.CodeInvalidInput, "providers: decode tool", err)
		}
		fn := tool
		if f, ok := tool["function"].(map[string]any); ok {
			fn = f
		}
		name, _ := fn["name"].(string)
		if name == "" {
			return nil, domain.NewError(domain.CodeInvalidInput,
				"providers: tool without a name cannot be translated to anthropic")
		}
		entry := map[string]any{
			"name":         name,
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		}
		if d, ok := fn["description"]; ok {
			entry["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			entry["input_schema"] = p
		}
		if s, ok := fn["input_schema"]; ok {
			entry["input_schema"] = s
		}
		out = append(out, entry)
	}
	return out, nil
}

// translateAnthropicToolChoice maps OpenAI's tool_choice onto Anthropic's:
// "auto"→auto, "required"→any, "none"→none, {"type":"function","function":
// {"name":X}}→{"type":"tool","name":X}.
func translateAnthropicToolChoice(raw json.RawMessage) (any, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto", "none":
			return map[string]any{"type": asString}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		default:
			return nil, domain.NewError(domain.CodeInvalidInput,
				"providers: unsupported tool_choice "+asString)
		}
	}
	var asObj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &asObj); err != nil {
		return nil, domain.WrapError(domain.CodeInvalidInput, "providers: decode tool_choice", err)
	}
	if asObj.Type == "function" && asObj.Function.Name != "" {
		return map[string]any{"type": "tool", "name": asObj.Function.Name}, nil
	}
	return nil, domain.NewError(domain.CodeInvalidInput,
		"providers: unsupported tool_choice shape for anthropic")
}

// AuthHeaders implements Adapter.
func (a *AnthropicMessages) AuthHeaders(secret []byte) map[string]string {
	h := map[string]string{"anthropic-version": anthropicVersionHeader}
	if len(secret) > 0 {
		h["x-api-key"] = string(secret)
	}
	return h
}

// WrapStream implements Adapter (see anthropic_stream.go).
func (a *AnthropicMessages) WrapStream(body io.ReadCloser) *Stream {
	return TranslateAnthropicStream(body)
}

// ---------------------------------------------------------------------------
// Non-streaming response translation
// ---------------------------------------------------------------------------

// anthropicResponse is the non-streaming Messages API response shape.
type anthropicResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content blocks: text | thinking | tool_use.
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      *struct {
		InputTokens          *int64 `json:"input_tokens"`
		OutputTokens         *int64 `json:"output_tokens"`
		CacheReadInputTokens *int64 `json:"cache_read_input_tokens"`
		CacheCreationTokens  *int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// DecodeNonStream implements Adapter: the Anthropic message is translated
// into an OpenAI-shaped chat.completion so the client surface stays one
// protocol (same normalization the stream translator applies).
func (a *AnthropicMessages) DecodeNonStream(body []byte) (*NonStreamResult, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("providers: decode anthropic message: %w", err)
	}
	if resp.Type != "" && resp.Type != "message" {
		return nil, fmt.Errorf("providers: unexpected anthropic response type %q", resp.Type)
	}

	var text strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":     resp.ID,
		"object": "chat.completion",
		"model":  resp.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapAnthropicStopReason(resp.StopReason),
		}},
	}
	result := &NonStreamResult{UpstreamRequestID: resp.ID}
	// Visible answer size feeds the estimate path when usage is absent —
	// same 口径 as the streaming tap (content + reasoning bytes).
	result.ContentBytes = int64(text.Len() + reasoning.Len())
	if resp.Usage != nil {
		u := resp.Usage
		usage := map[string]any{
			"prompt_tokens":     derefInt64(u.InputTokens),
			"completion_tokens": derefInt64(u.OutputTokens),
			"total_tokens":      derefInt64(u.InputTokens) + derefInt64(u.OutputTokens),
		}
		// Cache buckets ride the wire so the client surfaces (Task 13) can
		// render them natively: cached_tokens in prompt_tokens_details
		// (OpenAI-conventional), cache creation via the extension key
		// (OpenAI consumers ignore unknown keys).
		if u.CacheReadInputTokens != nil {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *u.CacheReadInputTokens}
		}
		if u.CacheCreationTokens != nil {
			usage["cache_creation_input_tokens"] = *u.CacheCreationTokens
		}
		out["usage"] = usage
		result.UsageReported = true
		result.Usage = domain.UsageBuckets{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationTokens,
		}
		result.UsageRaw = marshalAnthropicRawUsage(u.InputTokens, u.OutputTokens,
			u.CacheReadInputTokens, u.CacheCreationTokens)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("providers: marshal translated completion: %w", err)
	}
	result.Payload = payload
	return result, nil
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// marshalAnthropicRawUsage renders the schema-versioned raw_usage document
// for an Anthropic usage fact (field names keep the upstream names).
func marshalAnthropicRawUsage(input, output, cacheRead, cacheWrite *int64) json.RawMessage {
	doc := map[string]any{"schema_version": 1, "usage": map[string]any{}}
	u := doc["usage"].(map[string]any)
	if input != nil {
		u["input_tokens"] = *input
	}
	if output != nil {
		u["output_tokens"] = *output
	}
	if cacheRead != nil {
		u["cache_read_input_tokens"] = *cacheRead
	}
	if cacheWrite != nil {
		u["cache_creation_input_tokens"] = *cacheWrite
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{"schema_version":1}`)
	}
	return raw
}

// mapAnthropicStopReason maps Anthropic stop reasons onto OpenAI finish
// reasons (candidate-branch mapping, kept).
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
