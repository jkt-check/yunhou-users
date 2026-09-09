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
//   - input 中的 reasoning 项接受但不回放（chat 形状上游无对应语义；
//     OpenAI 的 encrypted_content 存储不在本期范围）；
//   - background:true 与 include 非空 400（无异步执行/附加数据面）；
//   - truncation:auto 400 —— 服务端静默裁剪上下文违反本网关"不静默丢
//     字段"原则；truncation:disabled（默认语义）接受；
//   - text.format 仅 text；json_object/json_schema 400（与 chat 面一致）；
//   - store:false 不落会话链（OpenAI 语义），引用它的 previous_response_id
//     得到 404；store 缺省为 true（与 OpenAI 一致）。
//
// 终止语义：response.completed / response.incomplete 是仅有的干净结束；
// 上游中断 → `event: error` 原生错误事件后结束，绝不伪造 completed。

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
		case "low", "medium", "high", "xhigh":
			on := true
			req.ThinkingEnabled = &on
		case "", "none", "minimal":
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
func responsesItemsToChat(items []json.RawMessage) ([]model.ChatMessage, error) {
	var out []model.ChatMessage
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
			out = append(out, model.ChatMessage{Role: role, Content: text})
		case "function_call":
			if it.CallID == "" || it.Name == "" {
				return nil, invalidMsg("input", "function_call items require call_id and name")
			}
			if it.Arguments != "" && !json.Valid([]byte(it.Arguments)) {
				return nil, invalidMsg("input", "function_call arguments must be a JSON-encoded value")
			}
			out = append(out, model.ChatMessage{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					ID: it.CallID, Type: "function",
					Function: model.ToolCallFunction{Name: it.Name, Arguments: strOr(it.Arguments, "{}")},
				}},
			})
		case "function_call_output":
			if it.CallID == "" {
				return nil, invalidMsg("input", "function_call_output items require call_id")
			}
			text, err := responsesToolOutputText(it.Output)
			if err != nil {
				return nil, err
			}
			out = append(out, model.ChatMessage{Role: "tool", ToolCallID: it.CallID, Content: text})
		case "reasoning":
			// 推理项接受但不回放（chat 形状上游无对应语义；能力矩阵明示）。
			continue
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
// counts input_tokens INCLUDING cached tokens — the opposite of the
// Anthropic surface — so cacheReadInInput==false (Anthropic-origin wire)
// ADDS the cache bucket back into input.
func ResponsesUsageShape(u *openAIUsage, cacheReadInInput bool) map[string]any {
	if u == nil {
		return nil
	}
	input := derefOr(u.PromptTokens, 0)
	output := derefOr(u.CompletionTokens, 0)
	var cached, reasoning int64
	if u.PromptDetails != nil {
		cached = derefOr(u.PromptDetails.CachedTokens, 0)
	}
	if u.CompletionDetails != nil {
		reasoning = derefOr(u.CompletionDetails.ReasoningTokens, 0)
	}
	if !cacheReadInInput {
		input += cached
	}
	usage := map[string]any{
		"input_tokens":         input,
		"input_tokens_details": map[string]any{"cached_tokens": cached},
		"output_tokens":        output,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoning,
		},
		"total_tokens": input + output,
	}
	return usage
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
		calls = append(calls, outputToolCall{c.ID, c.Function.Name, strOr(c.Function.Arguments, "{}")})
	}
	output := responsesOutputItems(ch.Message.ReasoningContent, ch.Message.Content, calls)

	resp := responseShell(responseID, publicModel, createdAt)
	if ch.FinishReason == "length" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
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

	// open item bookkeeping
	openKind       string // "" | "message" | "reasoning" | "function_call"
	openIndex      int
	openItemID     string
	nextIndex      int
	toolItemByIx   map[int]string // wire tool_calls[].index → item id
	textBuf        strings.Builder
	reasonBuf      strings.Builder
	argsBuf        strings.Builder
	openToolName   string
	openToolCallID string

	outputItems []json.RawMessage
	completed   bool
}

// TranslateOpenAIToResponsesStream converts the internal chunk SSE stream
// into Responses API SSE. The returned state is safe to read after the
// relay drains the stream to EOF.
func TranslateOpenAIToResponsesStream(body io.ReadCloser, responseID, model string, createdAt int64, cacheReadInInput bool) (io.ReadCloser, *ResponsesStreamState) {
	st := &ResponsesStreamState{
		responseID: responseID, model: model, createdAt: createdAt,
		cacheReadInInput: cacheReadInInput,
		openIndex:        -1, toolItemByIx: map[int]string{},
	}
	return translateStream(body, st), st
}

// Completed reports whether response.completed/incomplete was emitted
// (chain persistence only happens for completed turns).
func (s *ResponsesStreamState) Completed() bool { return s.completed }

// FinalOutputItems returns the assembled canonical output items (reasoning
// excluded — 回放语义不接受推理项), valid after EOF.
func (s *ResponsesStreamState) FinalOutputItems() []json.RawMessage {
	return s.outputItems
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
			if err := s.openItem(w, "reasoning", ""); err != nil {
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
			if err := s.openItem(w, "message", ""); err != nil {
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
// response.incomplete on length truncation) carrying the assembled output
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
	if s.finish == "length" {
		event = "response.incomplete"
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	} else {
		resp["status"] = "completed"
	}
	resp["output"] = json.RawMessage(mustJSON(s.outputItems))
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

// openItem opens the next output item of the given kind when not already
// open (interleaving opens a NEW item; kind continuation reuses the open one).
func (s *ResponsesStreamState) openItem(w io.Writer, kind string, toolCallID string) error {
	if s.openKind == kind && (kind != "function_call") {
		return nil
	}
	if err := s.closeOpenItem(w); err != nil {
		return err
	}
	s.openKind = kind
	s.openIndex = s.nextIndex
	s.nextIndex++
	s.textBuf.Reset()
	s.reasonBuf.Reset()
	s.argsBuf.Reset()
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
	case "function_call":
		s.openItemID = newPublicID("fc_")
		s.openToolCallID = toolCallID
		return s.emit(w, "response.output_item.added", map[string]any{
			"output_index": s.openIndex,
			"item": map[string]any{
				"type": "function_call", "id": s.openItemID, "call_id": toolCallID,
				"name": s.openToolName, "arguments": "", "status": "in_progress",
			},
		})
	}
	return nil
}

// renderToolDelta maps one wire tool_calls[] delta: the first fragment of a
// call (id present) opens a function_call item; argument fragments stream as
// function_call_arguments.delta (call_id 逐字保留).
func (s *ResponsesStreamState) renderToolDelta(w io.Writer, ix int, id, name, args string) error {
	itemID, open := s.toolItemByIx[ix]
	if !open {
		s.openToolName = name
		if err := s.openItem(w, "function_call", id); err != nil {
			return err
		}
		itemID = s.openItemID
		s.toolItemByIx[ix] = itemID
	}
	if args == "" {
		return nil
	}
	s.argsBuf.WriteString(args)
	return s.emit(w, "response.function_call_arguments.delta", map[string]any{
		"item_id": itemID, "output_index": s.openIndex, "delta": args,
	})
}

// closeOpenItem emits the *.done events of the open item and appends its
// canonical form to the assembled output (reasoning items join the response
// output but are excluded from chain transcripts at persistence time).
func (s *ResponsesStreamState) closeOpenItem(w io.Writer) error {
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
		s.outputItems = append(s.outputItems, b)
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
	case "function_call":
		args := s.argsBuf.String()
		if args == "" {
			args = "{}"
		}
		if err := s.emit(w, "response.function_call_arguments.done", map[string]any{
			"item_id": itemID, "output_index": ix, "arguments": args,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "function_call", "id": itemID, "call_id": s.openToolCallID,
			"name": s.openToolName, "arguments": args, "status": "completed",
		}
		if err := s.emit(w, "response.output_item.done", map[string]any{
			"output_index": ix, "item": item,
		}); err != nil {
			return err
		}
		b, _ := json.Marshal(item)
		s.outputItems = append(s.outputItems, b)
	}
	return nil
}
