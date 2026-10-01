// openai_responses.go — OpenAI Responses API 上游协议适配器。
//
// 适用于上游只讲 Responses 协议（POST {base}/responses）的部署——OpenAI
// 官方 /v1/responses 及任何标准兼容端点（如 opencode zen）。与
// responses_client.go（客户端面）方向相反：这里把内部 chat 形状翻译成
// Responses 请求体，并把 Responses 响应/SSE 翻译回内部 chat 形状，因此
// 网关内部 wire 协议不变式（client_surface.go）对所有上游成立。
//
// 明确的翻译决定（能力矩阵同步记录）：
//   - system 轮并入 instructions（Responses 的系统提示字段），多段 \n\n 连接；
//   - assistant 轮的 reasoning_content 不回放：忠实回放需要上游签发的
//     reasoning item id / encrypted_content，内部 chat 形状不携带；伪造
//     reasoning 项会被上游 400。丢弃回放与客户端面对称（responses_client.go
//     「推理项接受但不回放」），推理模型多轮对话由上游重新推理；
//   - store:false 始终发送——网关自建会话链（httpapi/responses.go 的
//     previous_response_id 由网关侧 transcript 回放实现），不要求上游
//     存储，prompt 不落上游存储面；
//   - thinking 开启 → reasoning {effort:medium, summary:auto}（要回推理
//     文本必须显式请求 summary）；显式关闭 → {effort:none}；
//   - Responses 没有对应物的采样参数（stop/penalties/seed/top_k）显式
//     400 —— 不静默丢字段。

package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// OpenAIResponses is the adapter for openai_responses deployments (the
// OpenAI Responses API and standard-compatible endpoints).
type OpenAIResponses struct{}

// NewOpenAIResponses builds the adapter.
func NewOpenAIResponses() *OpenAIResponses { return &OpenAIResponses{} }

// Protocol implements Adapter.
func (a *OpenAIResponses) Protocol() domain.Protocol { return domain.ProtocolOpenAIResponses }

// Capabilities implements Adapter. The Responses terminal frames
// (response.completed/incomplete) always carry the full response object
// including usage, so streaming usage is available by construction.
func (a *OpenAIResponses) Capabilities() domain.AdapterCapabilities {
	return domain.AdapterCapabilities{StreamingUsage: true, Tools: true, Reasoning: true}
}

// Inclusion implements Adapter: Responses input_tokens already contains
// cached_tokens (input_tokens_details) and output_tokens already contains
// reasoning_tokens (output_tokens_details) — accounting subtracts/folds
// accordingly so nothing is double-charged (设计 §7.1).
func (a *OpenAIResponses) Inclusion() accounting.Inclusion {
	return accounting.Inclusion{ReasoningInOutput: true, CacheReadInInput: true}
}

// responsesThinkingEffort is the effort requested when the client toggles
// thinking on without a protocol-level effort knob (OpenAI's own default
// for reasoning models is medium).
const responsesThinkingEffort = "medium"

// responsesPassthroughKeys are the sampling parameters the Responses API
// shares with chat completions and this adapter relays verbatim.
var responsesPassthroughKeys = []string{"temperature", "top_p", "parallel_tool_calls"}

// BuildPayload implements Adapter: internal ChatRequest → Responses request
// body (input items + instructions + max_output_tokens).
func (a *OpenAIResponses) BuildPayload(call *Call) ([]byte, error) {
	r := call.Request
	if call.OutputCap <= 0 {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"providers: openai_responses dispatch requires a positive output cap (不允许无限输出)")
	}

	var instructions []string
	input := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		switch m.Role {
		case "system":
			if m.Content != "" {
				instructions = append(instructions, m.Content)
			}
		case "user":
			input = append(input, map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": m.Content}},
			})
		case "assistant":
			// reasoning_content 不回放（见文件头：缺 item id / encrypted_content，
			// 伪造 reasoning 项会被上游 400；上游重新推理）。
			if m.Content != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": m.Content}},
				})
			}
			for _, tc := range m.ToolCalls {
				// 客户端合法但上游必 400 的形状在付费往返之前本地失败
				//（同 anthropic.go 的 deliberate-fail 原则）。
				if tc.ID == "" {
					return nil, domain.NewError(domain.CodeInvalidInput,
						"providers: assistant tool_call without an id cannot be translated to openai_responses (function_call requires call_id)")
				}
				input = append(input, map[string]any{
					"type": "function_call", "call_id": tc.ID,
					"name": tc.Function.Name, "arguments": strOr(tc.Function.Arguments, "{}"),
				})
			}
		case "tool":
			if m.ToolCallID == "" {
				return nil, domain.NewError(domain.CodeInvalidInput,
					"providers: tool message without tool_call_id cannot be translated to openai_responses (function_call_output requires call_id)")
			}
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content,
			})
		default:
			return nil, domain.NewError(domain.CodeInvalidInput,
				fmt.Sprintf("providers: unsupported role %q for openai_responses translation", m.Role))
		}
	}
	// 纯 system 请求翻译为空 input，上游必 400 —— 本地显式失败（同
	// anthropic.go 的 no-non-system-messages 守卫）。
	if len(input) == 0 {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"providers: openai_responses translation: no non-system messages")
	}

	payload := map[string]any{
		"model":             call.Deployment.UpstreamModel,
		"input":             input,
		"stream":            r.Stream,
		"max_output_tokens": call.OutputCap,
		// 网关自建会话链，不要求上游存储（prompt 不落上游存储面）。
		"store": false,
	}
	if len(instructions) > 0 {
		payload["instructions"] = strings.Join(instructions, "\n\n")
	}
	if len(r.Tools) > 0 {
		tools, err := responsesUpstreamTools(r.Tools)
		if err != nil {
			return nil, err
		}
		payload["tools"] = tools
	}
	if len(r.ToolChoice) > 0 {
		tc, err := responsesUpstreamToolChoice(r.ToolChoice)
		if err != nil {
			return nil, err
		}
		payload["tool_choice"] = tc
	}
	if r.ThinkingEnabled != nil {
		if *r.ThinkingEnabled {
			// summary:auto 是拿回推理文本的必要条件（无加密存储，摘要即所
			// 得；responses_client.go 的 reasoning 项映射同一口径）。
			payload["reasoning"] = map[string]any{"effort": responsesThinkingEffort, "summary": "auto"}
		} else {
			payload["reasoning"] = map[string]any{"effort": "none"}
		}
	}
	for _, k := range responsesPassthroughKeys {
		if v, ok := r.Passthrough[k]; ok {
			payload[k] = v
		}
	}
	for k := range r.Passthrough {
		known := false
		for _, allowed := range responsesPassthroughKeys {
			if k == allowed {
				known = true
				break
			}
		}
		if !known {
			return nil, domain.NewError(domain.CodeInvalidInput,
				"providers: parameter "+k+" is not supported by the openai_responses protocol")
		}
	}
	return json.Marshal(payload)
}

// responsesUpstreamTools maps the internal OpenAI tool envelope
// ({"type":"function","function":{...}}) onto the Responses flattened
// function shape — the inverse of responses_client.go's
// responsesToolsToOpenAI. An already-flat shape is accepted too. A nameless
// tool would 400 the whole upstream request, so it is rejected here.
//
// 字段转发是有意的白名单（name/description/parameters/strict）：扁平化即
// 字段边界，function 内的未知键随扁平化丢弃——与 responsesPassthroughKeys
// 的「未知即 400」相反，因为工具 schema 键是定义数据而非行为开关（与
// translateAnthropicTools 同一口径）。
func responsesUpstreamTools(tools []json.RawMessage) ([]any, error) {
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
				"providers: tool without a name cannot be translated to openai_responses")
		}
		entry := map[string]any{"type": "function", "name": name}
		if d, ok := fn["description"]; ok {
			entry["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			entry["parameters"] = p
		}
		if s, ok := fn["strict"]; ok {
			entry["strict"] = s
		}
		out = append(out, entry)
	}
	return out, nil
}

// responsesUpstreamToolChoice maps the internal OpenAI tool_choice onto the
// Responses shape — the inverse of responses_client.go's
// responsesToolChoiceToOpenAI: strings pass through, {"type":"function",
// "function":{"name":X}} flattens to {"type":"function","name":X}.
func responsesUpstreamToolChoice(raw json.RawMessage) (any, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto", "none", "required":
			return asString, nil
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
		return map[string]any{"type": "function", "name": asObj.Function.Name}, nil
	}
	return nil, domain.NewError(domain.CodeInvalidInput,
		"providers: unsupported tool_choice shape for openai_responses")
}

// AuthHeaders implements Adapter.
func (a *OpenAIResponses) AuthHeaders(secret []byte) map[string]string {
	if len(secret) == 0 {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + string(secret)}
}

// WrapStream implements Adapter (see openai_responses_stream.go).
func (a *OpenAIResponses) WrapStream(body io.ReadCloser) *Stream {
	return TranslateResponsesStream(body)
}

// ---------------------------------------------------------------------------
// Shared Responses object shapes (non-stream decode & stream translator)
// ---------------------------------------------------------------------------

// responsesUsage is the Responses API usage object. input_tokens INCLUDES
// cached tokens; output_tokens INCLUDES reasoning tokens (the adapter's
// Inclusion declares both overlaps).
type responsesUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
	InputDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// responsesOutputItem is the superset of the output item shapes we surface:
// message (output_text parts), reasoning (summary parts), function_call.
// Server-side tool items (web_search_call etc.) carry no client-surface
// meaning and are ignored, same discipline as the Anthropic decode.
type responsesOutputItem struct {
	Type string `json:"type"`
	// message
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// reasoning
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesObjectView is the response-object projection shared by the
// non-streaming decode and the terminal streaming frames.
type responsesObjectView struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage"`
}

// usageBuckets normalizes a Responses usage object into raw buckets (nil
// preserved; overlap normalization is accounting's job via Inclusion).
func (u *responsesUsage) usageBuckets() domain.UsageBuckets {
	b := domain.UsageBuckets{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputDetails != nil {
		b.CacheReadTokens = u.InputDetails.CachedTokens
	}
	if u.OutputDetails != nil {
		b.ReasoningTokens = u.OutputDetails.ReasoningTokens
	}
	return b
}

// openAIWireUsage renders the OpenAI-shaped usage map embedded into the
// translated internal payload, so the client surfaces (Task 13) can render
// cache/reasoning natively (cached_tokens in prompt_tokens_details,
// reasoning_tokens in completion_tokens_details). nil 桶在此渲成 0 是有意
// 的：nil-vs-zero 纪律约束的是计量桶（UsageBuckets），wire 渲染与
// anthropic.go 的 DecodeNonStream 同口径。
func (u *responsesUsage) openAIWireUsage() map[string]any {
	usage := map[string]any{
		"prompt_tokens":     derefInt64(u.InputTokens),
		"completion_tokens": derefInt64(u.OutputTokens),
		"total_tokens":      derefInt64(u.InputTokens) + derefInt64(u.OutputTokens),
	}
	if u.InputDetails != nil && u.InputDetails.CachedTokens != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *u.InputDetails.CachedTokens}
	}
	if u.OutputDetails != nil && u.OutputDetails.ReasoningTokens != nil {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": *u.OutputDetails.ReasoningTokens}
	}
	return usage
}

// marshalResponsesRawUsage renders the schema-versioned raw_usage document
// for a Responses usage fact (field names keep the upstream names).
func marshalResponsesRawUsage(u *responsesUsage) json.RawMessage {
	doc := map[string]any{"schema_version": 1, "usage": map[string]any{}}
	w := doc["usage"].(map[string]any)
	if u.InputTokens != nil {
		w["input_tokens"] = *u.InputTokens
	}
	if u.OutputTokens != nil {
		w["output_tokens"] = *u.OutputTokens
	}
	if u.InputDetails != nil && u.InputDetails.CachedTokens != nil {
		w["input_tokens_details"] = map[string]any{"cached_tokens": *u.InputDetails.CachedTokens}
	}
	if u.OutputDetails != nil && u.OutputDetails.ReasoningTokens != nil {
		w["output_tokens_details"] = map[string]any{"reasoning_tokens": *u.OutputDetails.ReasoningTokens}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{"schema_version":1}`)
	}
	return raw
}

// responsesFinishReason maps the Responses terminal status onto the internal
// OpenAI finish reason. max_output_tokens truncation → length (the client
// surfaces render it back as response.incomplete); content_filter passes
// through under its chat.completions name (审核拦截绝不像正常结束); a
// completed turn that emitted function calls → tool_calls.
func responsesFinishReason(status, incompleteReason string, hasToolCalls bool) string {
	if status == "incomplete" {
		switch incompleteReason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		default:
			return "stop"
		}
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// collectResponseOutput folds the output items of a completed response into
// the internal message parts (visible text, reasoning text, tool calls).
func collectResponseOutput(output []responsesOutputItem) (text, reasoning string, toolCalls []any) {
	var textBuf, reasonBuf strings.Builder
	for _, it := range output {
		switch it.Type {
		case "message":
			for _, p := range it.Content {
				if p.Type == "output_text" {
					textBuf.WriteString(p.Text)
				}
			}
		case "reasoning":
			for _, p := range it.Summary {
				if p.Type == "summary_text" {
					reasonBuf.WriteString(p.Text)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id": it.CallID, "type": "function",
				"function": map[string]any{"name": it.Name, "arguments": strOr(it.Arguments, "{}")},
			})
		}
	}
	return textBuf.String(), reasonBuf.String(), toolCalls
}

// ---------------------------------------------------------------------------
// Non-streaming response translation
// ---------------------------------------------------------------------------

// DecodeNonStream implements Adapter: the Responses object is translated
// into an OpenAI-shaped chat.completion so the client surface stays one
// protocol (same normalization the stream translator applies).
func (a *OpenAIResponses) DecodeNonStream(body []byte) (*NonStreamResult, error) {
	var resp struct {
		Object string `json:"object"`
		responsesObjectView
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("providers: decode openai response object: %w", err)
	}
	if resp.Object != "" && resp.Object != "response" {
		return nil, fmt.Errorf("providers: unexpected openai_responses payload object %q", resp.Object)
	}
	if resp.Status == "failed" {
		msg := "upstream response failed"
		if resp.Error != nil && resp.Error.Message != "" {
			msg = resp.Error.Message
		}
		return nil, fmt.Errorf("providers: openai_responses upstream failed: %s", msg)
	}

	text, reasoning, toolCalls := collectResponseOutput(resp.Output)
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	message := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
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
			"finish_reason": responsesFinishReason(resp.Status, incompleteReason, len(toolCalls) > 0),
		}},
	}
	result := &NonStreamResult{UpstreamRequestID: resp.ID}
	// Visible answer size feeds the estimate path when usage is absent —
	// same 口径 as the streaming tap (content + reasoning bytes).
	result.ContentBytes = int64(len(text) + len(reasoning))
	if resp.Usage != nil {
		out["usage"] = resp.Usage.openAIWireUsage()
		result.UsageReported = true
		result.Usage = resp.Usage.usageBuckets()
		result.UsageRaw = marshalResponsesRawUsage(resp.Usage)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("providers: marshal translated completion: %w", err)
	}
	result.Payload = payload
	return result, nil
}
