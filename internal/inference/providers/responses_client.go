// responses_client.go — OpenAI Responses 客户端协议面（Task 13，
// POST /v1/responses）。
//
// 目标客户端：Codex 及其他 Responses API 消费方。三件事：
//  1. ParseResponsesRequest: 原生 Responses 请求体 → 内部 ChatRequest +
//     规范 input items（后者原样进入会话链 transcript，供
//     previous_response_id 接续回放）；
//  2. ResponsesFromCompletion: 内部非流式 chat.completion → Responses
//     响应对象（同时产出本轮输出 items 供链路持久化）；
//  3. TranslateOpenAIToResponsesStream: 内部 chunk SSE → Responses SSE
//     事件序列（response.created → output_item/content_part/delta →
//     response.completed|incomplete；中断 → error 事件，无 completed）。
//
// 明确的协议子集决定（能力矩阵同步记录）:
//   - 仅文本：input_image/input_file 等多模态 part 400；
//   - 服务端工具调用项（web_search_call/file_search_call/computer_call/
//     code_interpreter_call/mcp_* 等）与 item_reference 400 —— 本网关无
//     服务端工具执行面，也不实现 OpenAI 的 stored-items 引用；
//   - input 中的 reasoning 项 400（T8：无法忠实回放 ——本网关不实现
//     OpenAI 的加密推理存储，include=reasoning.encrypted_content 早已被
//     拒绝；旧行为是「接受后丢弃」，那是兼容性修正）；
//   - background:true 与 include 非空 400（无异步执行/附加数据面）；
//   - truncation:auto 400 —— 服务端静默裁剪上下文违反本网关"不静默丢
//     字段"原则；truncation:disabled（默认语义）接受；
//   - text.format 仅 text；json_object/json_schema 400（与 chat 面一致）；
//   - store:false 不落会话链（OpenAI 语义），引用它的 previous_response_id
//     得到 404；store 缺省为 true（与 OpenAI 一致）。
//   - reasoning.effort 的档位逐字保留（T8：low/medium/high/xhigh 原样
//     下传，不再一律折成 medium）。
//
// 终止语义：response.completed / response.incomplete 是仅有的干净结束；
// 上游中断 → `event: error` 原生错误事件后结束，绝不伪造 completed。

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/yunhou/users/internal/model"
)

// Responses-surface abuse bounds（与 chat/messages 面同量级）.
const (
	responsesMaxItems    = 512
	responsesMaxTools    = 128
	responsesMaxModelLen = 128
	responsesMaxToolsB   = 256 << 10
)

// ResponsesInput is the parse result: the internal request plus the
// protocol-surface metadata the handler needs for chaining.
type ResponsesInput struct {
	Request *ChatRequest
	// Instructions is the request's system-prompt field (the handler
	// re-assembles messages when a chain replay prepends transcript items).
	Instructions string
	// InputItems are the request's canonical NEW input items (verbatim JSON),
	// the transcript increment for chain persistence.
	InputItems []json.RawMessage
	// PreviousResponseID references a stored chain row (empty = new chain).
	PreviousResponseID string
	// Store mirrors OpenAI semantics: false = the response is NOT referenceable
	// by later previous_response_id calls.
	Store bool
}

// ParseResponsesRequest decodes and capability-validates one Responses API
// body. Every rejection is a domain.CodeInvalidInput carrying the field.
func ParseResponsesRequest(body []byte) (*ResponsesInput, error) {
	var wire struct {
		Model              string            `json:"model"`
		Input              json.RawMessage   `json:"input"`
		Instructions       string            `json:"instructions"`
		MaxOutputTokens    *int64            `json:"max_output_tokens"`
		Stream             bool              `json:"stream"`
		Store              *bool             `json:"store"`
		PreviousResponseID string            `json:"previous_response_id"`
		Tools              []json.RawMessage `json:"tools"`
		ToolChoice         json.RawMessage   `json:"tool_choice"`
		ParallelToolCalls  *bool             `json:"parallel_tool_calls"`
		Temperature        *float64          `json:"temperature"`
		TopP               *float64          `json:"top_p"`
		Reasoning          json.RawMessage   `json:"reasoning"`
		Text               json.RawMessage   `json:"text"`
		Include            json.RawMessage   `json:"include"`
		Background         *bool             `json:"background"`
		Truncation         string            `json:"truncation"`
		MaxToolCalls       *int64            `json:"max_tool_calls"`
		Metadata           json.RawMessage   `json:"metadata"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&wire); err != nil {
		return nil, invalidMsg("body", "invalid request body")
	}
	if wire.Model == "" {
		return nil, invalidMsg("model", "model is required")
	}
	if len(wire.Model) > responsesMaxModelLen {
		return nil, invalidMsg("model", "model id too long")
	}
	if len(wire.Input) == 0 || string(wire.Input) == "null" {
		return nil, invalidMsg("input", "input is required")
	}
	if wire.Background != nil && *wire.Background {
		return nil, invalidMsg("background", "background execution is not supported")
	}
	if len(wire.Include) > 0 && string(wire.Include) != "null" {
		var inc []string
		if err := json.Unmarshal(wire.Include, &inc); err != nil {
			return nil, invalidMsg("include", "include must be an array")
		}
		if len(inc) > 0 {
			return nil, invalidMsg("include",
				"additional includes (encrypted reasoning, file outputs, ...) are not supported")
		}
	}
	if wire.Truncation != "" && wire.Truncation != "disabled" {
		return nil, invalidMsg("truncation",
			"server-side truncation is not supported (不静默裁剪上下文); omit or set disabled")
	}
	if wire.MaxToolCalls != nil {
		return nil, invalidMsg("max_tool_calls", "max_tool_calls is not supported")
	}
	if len(wire.Text) > 0 && string(wire.Text) != "null" {
		var txt struct {
			Format *struct {
				Type string `json:"type"`
			} `json:"format"`
		}
		if err := json.Unmarshal(wire.Text, &txt); err != nil {
			return nil, invalidMsg("text", "invalid text parameter")
		}
		if txt.Format != nil && txt.Format.Type != "" && txt.Format.Type != "text" {
			return nil, invalidMsg("text", "only text.format=text is supported")
		}
	}
	if len(wire.Metadata) > 0 && string(wire.Metadata) != "null" {
		var meta map[string]any
		if err := json.Unmarshal(wire.Metadata, &meta); err != nil {
			return nil, invalidMsg("metadata", "metadata must be an object")
		}
	}

	items, err := responsesInputItems(wire.Input)
	if err != nil {
		return nil, err
	}
	if len(items) > responsesMaxItems {
		return nil, invalidMsg("input", "too many input items")
	}
	messages, err := responsesItemsToChat(items)
	if err != nil {
		return nil, err
	}
	if wire.Instructions != "" {
		messages = append([]model.ChatMessage{{Role: "system", Content: wire.Instructions}}, messages...)
	}
	if n := nonSystemCount(messages); n == 0 && wire.PreviousResponseID == "" {
		return nil, invalidMsg("input", "at least one message or tool output item is required")
	}

	req := &ChatRequest{Model: wire.Model, Messages: messages, Stream: wire.Stream}
	if wire.MaxOutputTokens != nil {
		if *wire.MaxOutputTokens <= 0 {
			return nil, invalidMsg("max_output_tokens", "max_output_tokens must be > 0")
		}
		req.MaxTokens = wire.MaxOutputTokens
	}
	if len(wire.Tools) > responsesMaxTools {
		return nil, invalidMsg("tools", "too many tools")
	}
	tools, err := responsesToolsToOpenAI(wire.Tools)
	if err != nil {
		return nil, err
	}
	req.Tools = tools
	if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
		tc, err := responsesToolChoiceToOpenAI(wire.ToolChoice)
		if err != nil {
			return nil, err
		}
		req.ToolChoice = tc
	}
	passthrough := map[string]any{}
	if wire.ParallelToolCalls != nil {
		passthrough["parallel_tool_calls"] = *wire.ParallelToolCalls
	}
	if wire.Temperature != nil {
		passthrough["temperature"] = *wire.Temperature
	}
	if wire.TopP != nil {
		passthrough["top_p"] = *wire.TopP
	}
	if len(passthrough) > 0 {
		req.Passthrough = passthrough
	}
	if len(wire.Reasoning) > 0 && string(wire.Reasoning) != "null" {
		var rs struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(wire.Reasoning, &rs); err != nil {
			return nil, invalidMsg("reasoning", "invalid reasoning parameter")
		}
		switch rs.Effort {
		case "low", "medium", "high", "xhigh", "minimal":
			// T8: 档位逐字保留 —— 不再一律折成「开/关」，也不把 minimal
			// 折成「关闭」（minimal 是最低**推理档**，不是无推理；OpenAI 的
			// 语义如此）。上游不支持该档位时由能力路由在出站前明确拒绝。
			on := true
			req.ThinkingEnabled = &on
			req.ReasoningEffort = rs.Effort
		case "", "none":
			if rs.Effort != "" {
				off := false
				req.ThinkingEnabled = &off
			}
		default:
			return nil, invalidMsg("reasoning", "unsupported reasoning.effort: "+rs.Effort)
		}
		// reasoning.summary 是展示提示：上游给推理文本时本面以 summary_text
		// 形式回吐（无加密存储），不接受即视为关闭。
	}
	store := true
	if wire.Store != nil {
		store = *wire.Store
	}
	return &ResponsesInput{
		Request:            req,
		Instructions:       wire.Instructions,
		InputItems:         items,
		PreviousResponseID: wire.PreviousResponseID,
		Store:              store,
	}, nil
}

// ResponsesReplayToChat converts a COMBINED canonical item list (stored
// transcript ++ new input) into internal chat messages for a chained call.
// The caller prepends the instructions/system turn itself.
func ResponsesReplayToChat(items []json.RawMessage) ([]model.ChatMessage, error) {
	return responsesItemsToChat(items)
}

// responsesInputItems normalizes the input field (string | item array) into
// the canonical item list kept for transcript persistence (verbatim JSON).
func responsesInputItems(raw json.RawMessage) ([]json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, invalidMsg("input", "input must not be empty")
		}
		item, _ := json.Marshal(map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": s}},
		})
		return []json.RawMessage{item}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, invalidMsg("input", "input must be a string or an array of items")
	}
	if len(items) == 0 {
		return nil, invalidMsg("input", "input must not be empty")
	}
	for _, it := range items {
		if !json.Valid(it) {
			return nil, invalidMsg("input", "invalid input item")
		}
	}
	return items, nil
}

// responsesItemView is the parsed shape of one input item.
type responsesItemView struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// function_call_output
	Output json.RawMessage `json:"output"`
}

// responsesItemsToChat converts canonical Responses items into internal chat
// messages. function_call.call_id is preserved VERBATIM as the chat
// tool_call id (多轮工具调用 ID 保留).
//
// 轮次边界（评审修复）：message item 永远自成一条 ChatMessage —— 连续的
// assistant 文本轮不得无分隔拼成一条（"Hello"+"World" → "HelloWorld" 会
// 丢掉轮次边界）。只有 function_call 需要并入前一个 assistant 轮（Chat 的
// tool_calls 挂在同一条 assistant 消息上），否则新建 assistant 轮。
//
// 未决调用（评审修复）：一次 function_call 在对应的 function_call_output
// 之前一直「未决」—— Chat 的 content 永远排在 tool_calls 之前，未决调用
// 之后的 assistant 文本无法保序，必须拒绝；空的 assistant 轮不成其为文本，
// 也不能把未决状态清掉（否则空轮就成了绕道）。
func responsesItemsToChat(items []json.RawMessage) ([]model.ChatMessage, error) {
	var out []model.ChatMessage
	// Chat 的 tool_calls 挂在 assistant 消息上：调用前的文本与连续的并行调用
	// 归入同一个 assistant 轮；调用之后的文本无法保序（Chat 的 content 永远
	// 排在 tool_calls 之前），只能拒绝。
	toolCallPending := false
	for _, raw := range items {
		var it responsesItemView
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, invalidMsg("input", "invalid input item")
		}
		switch it.Type {
		case "", "message":
			role := it.Role
			if role == "developer" {
				role = "system"
			}
			switch role {
			case "system", "user", "assistant":
			default:
				return nil, invalidMsg("input", "message item role must be user, assistant, system or developer")
			}
			text, err := responsesMessageText(role, it.Content)
			if err != nil {
				return nil, err
			}
			switch role {
			case "assistant":
				if text != "" {
					if toolCallPending {
						return nil, invalidMsg("input",
							"assistant text after function_call cannot preserve item order in a Chat turn; send tool outputs before the next assistant message")
					}
					toolCallPending = false
				}
				// 空 assistant 轮自成一条 ChatMessage，但**不**清掉未决标记。
			default:
				// user / system 消息终结未决调用。
				toolCallPending = false
			}
			out = append(out, model.ChatMessage{Role: role, Content: text})
		case "function_call":
			if it.CallID == "" || it.Name == "" {
				return nil, invalidMsg("input", "function_call items require call_id and name")
			}
			if !json.Valid([]byte(it.Arguments)) {
				return nil, invalidMsg("input", "function_call arguments must be a JSON-encoded value")
			}
			call := model.ToolCall{
				ID: it.CallID, Type: "function",
				Function: model.ToolCallFunction{Name: it.Name, Arguments: it.Arguments},
			}
			if len(out) > 0 && out[len(out)-1].Role == "assistant" {
				out[len(out)-1].ToolCalls = append(out[len(out)-1].ToolCalls, call)
			} else {
				out = append(out, model.ChatMessage{Role: "assistant", ToolCalls: []model.ToolCall{call}})
			}
			toolCallPending = true
		case "function_call_output":
			if it.CallID == "" {
				return nil, invalidMsg("input", "function_call_output items require call_id")
			}
			text, err := responsesToolOutputText(it.Output)
			if err != nil {
				return nil, err
			}
			out = append(out, model.ChatMessage{Role: "tool", ToolCallID: it.CallID, Content: text})
			toolCallPending = false
		case "reasoning":
			// T8: 推理项**无法忠实回放**（本网关不实现 OpenAI 的加密推理
			// 存储：`include` 里的 reasoning.encrypted_content 早已被拒绝，
			// reasoning 项本身也没有可回放的加密载荷）。旧行为是「接受后
			// 丢弃」—— 客户端看不到自己回传的内容被吞掉。现在明确 4xx。
			return nil, invalidMsg("input",
				"reasoning items cannot be replayed by this gateway (no lossless encrypted reasoning replay)")
		case "item_reference":
			return nil, invalidMsg("input",
				"item_reference is not supported (stored-items API 不在本期范围)")
		default:
			return nil, invalidMsg("input", "unsupported input item type: "+it.Type)
		}
	}
	return out, nil
}

// responsesMessageText normalizes a message item's content (string | part
// array). input_text/output_text carry text; multimodal parts are rejected
// explicitly.
func responsesMessageText(role string, content json.RawMessage) (string, error) {
	if len(content) == 0 || string(content) == "null" {
		return "", invalidMsg("input", "message item content is required")
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", invalidMsg("input", "message content must be a string or content parts")
	}
	if len(parts) == 0 {
		return "", invalidMsg("input", "message content parts must not be empty")
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "input_text":
			if role == "assistant" {
				return "", invalidMsg("input", "assistant messages use output_text parts")
			}
			texts = append(texts, p.Text)
		case "output_text":
			texts = append(texts, p.Text)
		case "input_image", "input_file":
			return "", invalidMsg("input",
				p.Type+" parts are not supported by this gateway (text-only surface)")
		default:
			return "", invalidMsg("input", "unsupported content part type: "+p.Type)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// responsesToolOutputText normalizes function_call_output.output (string |
// output_text part array).
func responsesToolOutputText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", invalidMsg("input", "function_call_output output must be a string or output_text parts")
	}
	var texts []string
	for _, p := range parts {
		if p.Type != "output_text" && p.Type != "input_text" {
			return "", invalidMsg("input", "unsupported function_call_output part type: "+p.Type)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}

// responsesToolsToOpenAI converts Responses tool definitions (flattened
// function shape) into the internal OpenAI tool envelope. Non-function tool
// types (web_search, code_interpreter, mcp, local_shell, ...) are rejected
// explicitly — no server-side tool execution surface exists here.
func responsesToolsToOpenAI(raw []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(raw))
	total := 0
	for _, t := range raw {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		}
		if err := json.Unmarshal(t, &tool); err != nil {
			return nil, invalidMsg("tools", "invalid tool definition")
		}
		if tool.Type != "function" {
			return nil, invalidMsg("tools",
				"tool type "+tool.Type+" is not supported by this gateway")
		}
		if tool.Name == "" {
			return nil, invalidMsg("tools", "tool name is required")
		}
		fn := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			fn["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 && string(tool.Parameters) != "null" {
			fn["parameters"] = json.RawMessage(tool.Parameters)
		}
		if tool.Strict != nil {
			fn["strict"] = *tool.Strict
		}
		b, err := json.Marshal(map[string]any{"type": "function", "function": fn})
		if err != nil {
			return nil, invalidMsg("tools", "invalid tool schema")
		}
		total += len(b)
		out = append(out, b)
	}
	if total > responsesMaxToolsB {
		return nil, invalidMsg("tools", "tools too large")
	}
	return out, nil
}

// responsesToolChoiceToOpenAI maps the Responses tool_choice onto the
// internal OpenAI shape.
func responsesToolChoiceToOpenAI(raw json.RawMessage) (json.RawMessage, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch asString {
		case "auto", "none", "required":
			return json.Marshal(asString)
		default:
			return nil, invalidMsg("tool_choice", "unsupported tool_choice: "+asString)
		}
	}
	var asObj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &asObj); err != nil {
		return nil, invalidMsg("tool_choice", "invalid tool_choice")
	}
	if asObj.Type == "function" && asObj.Name != "" {
		return json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": asObj.Name}})
	}
	return nil, invalidMsg("tool_choice", "unsupported tool_choice shape")
}

// ---------------------------------------------------------------------------
// Response object assembly (非流式 & 流式终止帧共用)
// ---------------------------------------------------------------------------

// ResponsesUsageShape renders the Responses usage object. The Responses API
// counts input_tokens INCLUDING cache reads and creation — the opposite of
// the Anthropic surface — so cacheReadInInput==false (Anthropic-origin wire)
// adds both cache buckets back into input.
//
// input_tokens/output_tokens/total_tokens 与两个 details 对象都是 Responses
// usage 的**必填**字段：未知计数在此补零（已报值、显式零、已知 detail 值
// 原样保留）。total 优先用上游已报的值；没有可用上报值时由两个计数相加
// 推导（推导失败/溢出则落回零）。
func ResponsesUsageShape(u *openAIUsage, cacheReadInInput bool) map[string]any {
	if u == nil {
		return nil
	}
	input := u.PromptTokens
	output := u.CompletionTokens
	var cached, reasoning *int64
	if u.PromptDetails != nil {
		cached = u.PromptDetails.CachedTokens
	}
	if u.CompletionDetails != nil {
		reasoning = u.CompletionDetails.ReasoningTokens
	}
	total := u.TotalTokens
	if !cacheReadInInput {
		input = addUsageDetails(input, cached, u.CacheCreationTokens)
		total = nil // The wire total excludes Anthropic cache buckets.
	}
	if total == nil {
		// 必填计数归零后两个操作数都是已知数字 → total 可由两者相加得出；
		// 溢出/非法时推导失败 → 落回零。
		wireInput, wireOutput := usageCounter(input), usageCounter(output)
		total = addUsageDetails(&wireInput, &wireOutput)
	}
	return map[string]any{
		"input_tokens":          usageCounter(input),
		"output_tokens":         usageCounter(output),
		"total_tokens":          usageCounter(total),
		"input_tokens_details":  map[string]any{"cached_tokens": usageCounter(cached)},
		"output_tokens_details": map[string]any{"reasoning_tokens": usageCounter(reasoning)},
	}
}

// outputToolCall is one completed function call for output assembly.
type outputToolCall struct{ ID, Name, Arguments string }

// responsesOutputItems builds the canonical output items of one completed
// response from content parts (reasoning 项进入响应对象但不进会话链
// transcript —— 回放语义不接受推理项).
func responsesOutputItems(reasoning, text string, toolCalls []outputToolCall) []json.RawMessage {
	var out []json.RawMessage
	if reasoning != "" {
		b, _ := json.Marshal(map[string]any{
			"type": "reasoning", "id": newPublicID("rs_"),
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
		out = append(out, b)
	}
	if text != "" {
		b, _ := json.Marshal(map[string]any{
			"type": "message", "id": newPublicID("msg_"), "status": "completed",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		})
		out = append(out, b)
	}
	for _, tc := range toolCalls {
		b, _ := json.Marshal(map[string]any{
			"type": "function_call", "id": newPublicID("fc_"),
			"call_id": tc.ID, "name": tc.Name, "arguments": tc.Arguments,
			"status": "completed",
		})
		out = append(out, b)
	}
	return out
}

// responseShell is the shared response-object skeleton (status/output/usage
// filled by the caller).
func responseShell(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id": id, "object": "response", "created_at": createdAt,
		"model": model, "output": []any{},
		"parallel_tool_calls": true, "tool_choice": "auto",
	}
}

// ResponsesFromCompletion renders the non-streaming Responses API body and
// the canonical output items (for chain persistence). The response id is
// generated by the HANDLER (it owns the public id before streaming starts).
func ResponsesFromCompletion(payload []byte, responseID, publicModel string, createdAt int64, cacheReadInInput bool) ([]byte, []json.RawMessage, error) {
	v, err := parseCompletion(payload)
	if err != nil {
		return nil, nil, err
	}
	if len(v.Choices) == 0 {
		return nil, nil, fmt.Errorf("providers: internal completion has no choices")
	}
	ch := v.Choices[0]

	var calls []outputToolCall
	for _, c := range ch.Message.ToolCalls {
		calls = append(calls, outputToolCall{c.ID, c.Function.Name, c.Function.Arguments})
	}
	output := responsesOutputItems(ch.Message.ReasoningContent, ch.Message.Content, calls)

	resp := responseShell(responseID, publicModel, createdAt)
	if ch.FinishReason == "length" || ch.FinishReason == "content_filter" {
		reason := "max_output_tokens"
		if ch.FinishReason == "content_filter" {
			reason = "content_filter"
		}
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": reason}
	} else {
		resp["status"] = "completed"
	}
	resp["output"] = json.RawMessage(mustJSON(output))
	if u := ResponsesUsageShape(v.Usage, cacheReadInInput); u != nil {
		resp["usage"] = u
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("providers: marshal responses body: %w", err)
	}
	// Transcript excludes reasoning items (回放语义不接受推理项).
	var transcriptItems []json.RawMessage
	for _, it := range output {
		if !bytes.Contains(it, []byte(`"type":"reasoning"`)) {
			transcriptItems = append(transcriptItems, it)
		}
	}
	return body, transcriptItems, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// ---------------------------------------------------------------------------
// Streaming translation (内部 chunk SSE → Responses SSE)
// ---------------------------------------------------------------------------

// ResponsesStreamState is the renderer behind TranslateOpenAIToResponsesStream.
// After the relay reaches EOF the handler reads FinalOutputItems/Completed
// for chain persistence (pipe close establishes happens-before).
type ResponsesStreamState struct {
	responseID       string
	model            string
	createdAt        int64
	cacheReadInInput bool

	seq          int
	started      bool
	finish       string
	usage        *openAIUsage
	errorRelayed bool

	// open textual-item bookkeeping（message/reasoning 互斥，kind 切换即换 item）
	openKind   string // "" | "message" | "reasoning"
	openIndex  int
	openItemID string
	nextIndex  int
	textBuf    strings.Builder
	reasonBuf  strings.Builder
	// 评审轮2 I3：function_call item 按 wire tool_calls[].index 独立维护
	// （各自 id/name/args 缓冲），OpenAI 允许 parallel tool calls 交错——
	// 一个 call 的 open 绝不得关闭另一个 call 的 item。
	toolItems map[int]*responsesToolItem

	// Both slices are indexed by output_index, not by item closing order.
	// outputItems leaves reasoning slots nil; FinalOutputItems filters them
	// without moving earlier calls behind later text in the transcript.
	outputItems   []json.RawMessage
	responseItems []json.RawMessage
	completed     bool
}

// responsesToolItem is one in-flight function_call item keyed by the wire
// tool_calls[].index.
type responsesToolItem struct {
	itemID      string
	outputIndex int
	callID      string
	name        string
	argsBuf     strings.Builder
	open        bool
}

// TranslateOpenAIToResponsesStream converts the internal chunk SSE stream
// into Responses API SSE. The returned state is safe to read after the
// relay drains the stream to EOF.
func TranslateOpenAIToResponsesStream(body io.ReadCloser, responseID, model string, createdAt int64, cacheReadInInput bool) (io.ReadCloser, *ResponsesStreamState) {
	st := &ResponsesStreamState{
		responseID: responseID, model: model, createdAt: createdAt,
		cacheReadInInput: cacheReadInInput,
		openIndex:        -1, toolItems: map[int]*responsesToolItem{},
	}
	return translateStream(body, st), st
}

// Completed reports whether response.completed/incomplete was emitted
// (chain persistence only happens for completed turns).
func (s *ResponsesStreamState) Completed() bool { return s.completed }

// FinalOutputItems returns the assembled canonical output items (reasoning
// excluded — 回放语义不接受推理项), valid after EOF.
func (s *ResponsesStreamState) FinalOutputItems() []json.RawMessage {
	var items []json.RawMessage
	for _, item := range s.outputItems {
		if item != nil {
			items = append(items, item)
		}
	}
	return items
}

func (s *ResponsesStreamState) recordOutputItem(index int, raw json.RawMessage, replay bool) {
	for len(s.responseItems) <= index {
		s.responseItems = append(s.responseItems, nil)
		s.outputItems = append(s.outputItems, nil)
	}
	s.responseItems[index] = raw
	if replay {
		s.outputItems[index] = raw
	}
}

func (s *ResponsesStreamState) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *ResponsesStreamState) emit(w io.Writer, event string, v map[string]any) error {
	v["type"] = event
	v["sequence_number"] = s.nextSeq()
	return writeSSE(w, event, v)
}

// Render implements streamRenderer.
func (s *ResponsesStreamState) Render(ev pumpEvent, w io.Writer) error {
	if s.errorRelayed {
		return nil
	}
	if ev.done {
		return s.finishClean(w)
	}
	chunk := ev.chunk
	if chunk == nil {
		return nil
	}
	if chunk.Error != nil {
		s.errorRelayed = true
		return s.emitError(w, chunk.Error.Code, chunk.Error.Message)
	}
	if chunk.hasRefusal() {
		s.errorRelayed = true
		return s.emitError(w, "server_error", errChatRefusal.Error())
	}
	if !s.started {
		if err := s.emitCreated(w); err != nil {
			return err
		}
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	for _, choice := range chunk.Choices {
		d := choice.Delta
		if d.ReasoningContent != "" {
			if err := s.openTextualItem(w, "reasoning"); err != nil {
				return err
			}
			s.reasonBuf.WriteString(d.ReasoningContent)
			if err := s.emit(w, "response.reasoning_summary_text.delta", map[string]any{
				"item_id": s.openItemID, "output_index": s.openIndex, "summary_index": 0,
				"delta": d.ReasoningContent,
			}); err != nil {
				return err
			}
		}
		if d.Content != "" {
			if err := s.openTextualItem(w, "message"); err != nil {
				return err
			}
			s.textBuf.WriteString(d.Content)
			if err := s.emit(w, "response.output_text.delta", map[string]any{
				"item_id": s.openItemID, "output_index": s.openIndex, "content_index": 0,
				"delta": d.Content,
			}); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := s.renderToolDelta(w, tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finish = *choice.FinishReason
			if err := s.closeOpenItem(w); err != nil {
				return err
			}
		}
	}
	return nil
}

// finishClean closes the stream with response.completed (or
// response.incomplete on truncation/filtering) carrying the assembled output
// and usage.
func (s *ResponsesStreamState) finishClean(w io.Writer) error {
	if !s.started {
		if err := s.emitCreated(w); err != nil {
			return err
		}
	}
	if err := s.closeOpenItem(w); err != nil {
		return err
	}
	resp := responseShell(s.responseID, s.model, s.createdAt)
	event := "response.completed"
	if s.finish == "length" || s.finish == "content_filter" {
		reason := "max_output_tokens"
		if s.finish == "content_filter" {
			reason = "content_filter"
		}
		event = "response.incomplete"
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": reason}
	} else {
		resp["status"] = "completed"
	}
	resp["output"] = json.RawMessage(mustJSON(s.responseItems))
	if u := ResponsesUsageShape(s.usage, s.cacheReadInInput); u != nil {
		resp["usage"] = u
	}
	s.completed = true
	return s.emit(w, event, map[string]any{"response": resp})
}

// BrokenEnd implements streamRenderer: no [DONE] — the terminal frame is the
// native error event, never a fabricated response.completed.
func (s *ResponsesStreamState) BrokenEnd(w io.Writer) error {
	if s.errorRelayed {
		return nil
	}
	if !s.started {
		if err := s.emitCreated(w); err != nil {
			return err
		}
	}
	return s.emitError(w, "server_error", "upstream stream interrupted")
}

func (s *ResponsesStreamState) emitError(w io.Writer, code, message string) error {
	if code == "" {
		code = "server_error"
	}
	return s.emit(w, "error", map[string]any{"code": code, "message": message})
}

func (s *ResponsesStreamState) emitCreated(w io.Writer) error {
	s.started = true
	resp := responseShell(s.responseID, s.model, s.createdAt)
	resp["status"] = "in_progress"
	if err := s.emit(w, "response.created", map[string]any{"response": resp}); err != nil {
		return err
	}
	return s.emit(w, "response.in_progress", map[string]any{"response": resp})
}

// openTextualItem opens the next message/reasoning output item when not
// already open (kind 切换关闭当前文本/推理 item 并开新 item；function_call
// item 不受影响——它们按 wire index 独立维护).
func (s *ResponsesStreamState) openTextualItem(w io.Writer, kind string) error {
	if s.openKind == kind {
		return nil
	}
	if err := s.closeTextualItem(w); err != nil {
		return err
	}
	s.openKind = kind
	s.openIndex = s.nextIndex
	s.nextIndex++
	s.textBuf.Reset()
	s.reasonBuf.Reset()
	switch kind {
	case "message":
		s.openItemID = newPublicID("msg_")
		if err := s.emit(w, "response.output_item.added", map[string]any{
			"output_index": s.openIndex,
			"item": map[string]any{
				"type": "message", "id": s.openItemID, "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		}); err != nil {
			return err
		}
		return s.emit(w, "response.content_part.added", map[string]any{
			"item_id": s.openItemID, "output_index": s.openIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	case "reasoning":
		s.openItemID = newPublicID("rs_")
		if err := s.emit(w, "response.output_item.added", map[string]any{
			"output_index": s.openIndex,
			"item":         map[string]any{"type": "reasoning", "id": s.openItemID, "summary": []any{}},
		}); err != nil {
			return err
		}
		return s.emit(w, "response.reasoning_summary_part.added", map[string]any{
			"item_id": s.openItemID, "output_index": s.openIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	return nil
}

// renderToolDelta maps one wire tool_calls[] delta: the first fragment of a
// call opens ITS OWN function_call item keyed by the wire index; argument
// fragments stream as function_call_arguments.delta (call_id 逐字保留).
// 评审轮2 I3：交错 parallel tool calls 下，开 ix1 的 item 不得关闭 ix0 的
// item，迟到的 ix0 delta 仍落在 ix0 自己的 item_id/output_index/argsBuf 上。
func (s *ResponsesStreamState) renderToolDelta(w io.Writer, ix int, id, name, args string) error {
	ti, ok := s.toolItems[ix]
	if !ok {
		// 新 wire index：只收尾开启中的文本/推理 item，绝不动其他
		// function_call item。
		if err := s.closeTextualItem(w); err != nil {
			return err
		}
		ti = &responsesToolItem{
			itemID: newPublicID("fc_"), outputIndex: s.nextIndex,
			callID: id, name: name, open: true,
		}
		s.nextIndex++
		s.toolItems[ix] = ti
		if err := s.emit(w, "response.output_item.added", map[string]any{
			"output_index": ti.outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": ti.itemID, "call_id": id,
				"name": name, "arguments": "", "status": "in_progress",
			},
		}); err != nil {
			return err
		}
	}
	if args == "" {
		return nil
	}
	ti.argsBuf.WriteString(args)
	return s.emit(w, "response.function_call_arguments.delta", map[string]any{
		"item_id": ti.itemID, "output_index": ti.outputIndex, "delta": args,
	})
}

// closeOpenItem closes every open item — the textual item plus each
// in-flight function_call item — in output_index order (canonical output 数
// 组位置序与事件序一致).
func (s *ResponsesStreamState) closeOpenItem(w io.Writer) error {
	type closable struct {
		index int
		tool  *responsesToolItem // nil = the textual item
	}
	var list []closable
	if s.openKind != "" {
		list = append(list, closable{index: s.openIndex})
	}
	for _, ti := range s.toolItems {
		if ti.open {
			list = append(list, closable{index: ti.outputIndex, tool: ti})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].index < list[j].index })
	for _, c := range list {
		var err error
		if c.tool != nil {
			err = s.closeToolItem(w, c.tool)
		} else {
			err = s.closeTextualItem(w)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// closeToolItem emits the *.done events of one function_call item and
// appends its canonical form to the assembled output.
func (s *ResponsesStreamState) closeToolItem(w io.Writer, ti *responsesToolItem) error {
	if !ti.open {
		return nil
	}
	ti.open = false
	args := ti.argsBuf.String()
	if err := s.emit(w, "response.function_call_arguments.done", map[string]any{
		"item_id": ti.itemID, "output_index": ti.outputIndex, "arguments": args,
	}); err != nil {
		return err
	}
	item := map[string]any{
		"type": "function_call", "id": ti.itemID, "call_id": ti.callID,
		"name": ti.name, "arguments": args, "status": "completed",
	}
	if err := s.emit(w, "response.output_item.done", map[string]any{
		"output_index": ti.outputIndex, "item": item,
	}); err != nil {
		return err
	}
	b, _ := json.Marshal(item)
	s.recordOutputItem(ti.outputIndex, b, true)
	return nil
}

// closeTextualItem emits the *.done events of the open message/reasoning
// item and appends its canonical form to the assembled output: reasoning
// items join only the response output (responseItems — 终结帧与事件流自
// 洽), message items additionally join the chain transcript (outputItems;
// 回放语义不接受推理项).
func (s *ResponsesStreamState) closeTextualItem(w io.Writer) error {
	if s.openKind == "" {
		return nil
	}
	kind := s.openKind
	itemID := s.openItemID
	ix := s.openIndex
	s.openKind = ""
	s.openItemID = ""
	s.openIndex = -1

	switch kind {
	case "message":
		text := s.textBuf.String()
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if err := s.emit(w, "response.output_text.done", map[string]any{
			"item_id": itemID, "output_index": ix, "content_index": 0, "text": text,
		}); err != nil {
			return err
		}
		if err := s.emit(w, "response.content_part.done", map[string]any{
			"item_id": itemID, "output_index": ix, "content_index": 0, "part": part,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "message", "id": itemID, "status": "completed",
			"role": "assistant", "content": []any{part},
		}
		if err := s.emit(w, "response.output_item.done", map[string]any{
			"output_index": ix, "item": item,
		}); err != nil {
			return err
		}
		b, _ := json.Marshal(item)
		s.recordOutputItem(ix, b, true)
	case "reasoning":
		reason := s.reasonBuf.String()
		if err := s.emit(w, "response.reasoning_summary_text.done", map[string]any{
			"item_id": itemID, "output_index": ix, "summary_index": 0, "text": reason,
		}); err != nil {
			return err
		}
		if err := s.emit(w, "response.reasoning_summary_part.done", map[string]any{
			"item_id": itemID, "output_index": ix, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": reason},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "reasoning", "id": itemID,
			"summary": []any{map[string]any{"type": "summary_text", "text": reason}},
		}
		// 推理项进响应对象（客户端展示），不进会话链 transcript。
		if err := s.emit(w, "response.output_item.done", map[string]any{
			"output_index": ix, "item": item,
		}); err != nil {
			return err
		}
		b, _ := json.Marshal(item)
		s.recordOutputItem(ix, b, false)
	}
	return nil
}
