// messages_client.go — Anthropic Messages 客户端协议面（Task 13，
// POST /v1/messages）。
//
// 目标客户端：Claude Code 及其他 Anthropic SDK 消费方（API 版本
// 2023-06-01 子集）。本文件做三件事：
//  1. ParseAnthropicMessagesRequest: 原生 Messages 请求体 → 内部
//     ChatRequest（模型名/工具调用 ID/推理开关逐字保留；不能兼容的字段
//     显式 400，绝不静默丢弃）；
//  2. AnthropicMessageFromCompletion: 内部非流式 chat.completion →
//     Anthropic message JSON；
//  3. TranslateOpenAIToAnthropicStream: 内部 OpenAI chunk SSE → Anthropic
//     Messages SSE 事件序列（message_start → content_block_* →
//     message_delta → message_stop）。
//
// 明确的协议子集决定（能力矩阵同步记录）:
//   - 仅文本：image/document 等多模态块 400；
//   - 服务端工具（web_search/computer/bash/code_execution/mcp 等非 custom
//     type）400 —— 本网关无服务端工具执行面；
//   - 历史中的 thinking/redacted_thinking 块接受但不回放（chat 形状上游无
//     对应语义；交错 thinking 连续性不在本期范围），工具与文本块不受影响；
//   - tool_result 的 is_error 标志不转发（OpenAI chat 无对应字段），错误
//     内容文本照常传递；
//   - cache_control 提示接受但不保证生效（成本提示，非正确性语义）；
//   - metadata 接受但不消费（客户端标记，无语义效果）。
//
// 终止语义：message_stop 是唯一干净结束；上游中断（EOF 无 message_stop /
// 读错误 / 内联 error 块）→ 发 `event: error` 原生错误事件后结束，绝不
// 伪造 message_stop。

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// Messages-surface abuse bounds (与 chat 面同量级，不继承 Kaya /chat 的
// 产品限制).
const (
	messagesMaxTurns    = 512
	messagesMaxTools    = 128
	messagesMaxModelLen = 128
	messagesMaxToolsB   = 256 << 10
)

// anthropicMinThinkingBudget is Anthropic's documented floor for
// thinking.budget_tokens.
const anthropicMinThinkingBudget int64 = 1024

// ParseAnthropicMessagesRequest decodes and capability-validates one
// Messages API body into the internal ChatRequest. Every rejection is a
// domain.CodeInvalidInput carrying the offending field — the handler maps it
// to the native 400 shape.
func ParseAnthropicMessagesRequest(body []byte) (*ChatRequest, error) {
	var wire struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		System        json.RawMessage   `json:"system"`
		MaxTokens     *int64            `json:"max_tokens"`
		Stream        bool              `json:"stream"`
		Tools         []json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage   `json:"tool_choice"`
		Thinking      json.RawMessage   `json:"thinking"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		TopK          *int64            `json:"top_k"`
		StopSequences []string          `json:"stop_sequences"`
		Metadata      json.RawMessage   `json:"metadata"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&wire); err != nil {
		return nil, invalidMsg("body", "invalid request body")
	}
	if wire.Model == "" {
		return nil, invalidMsg("model", "model is required")
	}
	if len(wire.Model) > messagesMaxModelLen {
		return nil, invalidMsg("model", "model id too long")
	}
	// Anthropic REQUIRES max_tokens — a request without it is malformed.
	if wire.MaxTokens == nil || *wire.MaxTokens <= 0 {
		return nil, invalidMsg("max_tokens", "max_tokens is required and must be > 0")
	}
	if len(wire.Messages) == 0 {
		return nil, invalidMsg("messages", "messages is required")
	}
	if len(wire.Messages) > messagesMaxTurns {
		return nil, invalidMsg("messages", "too many messages")
	}

	messages := make([]model.ChatMessage, 0, len(wire.Messages)+1)
	// system: string or array of text blocks (cache_control hints accepted,
	// not honored — cost hint only).
	if sys, err := anthropicSystemText(wire.System); err != nil {
		return nil, err
	} else if sys != "" {
		messages = append(messages, model.ChatMessage{Role: "system", Content: sys})
	}
	for _, m := range wire.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, invalidMsg("messages", "role must be user or assistant")
		}
		converted, err := anthropicTurnToChat(m.Role, m.Content)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	// Anthropic-illegal shapes fail at the surface (same deliberate-400 policy
	// as the upstream adapter): a system-only request has no conversation.
	if n := nonSystemCount(messages); n == 0 {
		return nil, invalidMsg("messages", "at least one user or assistant message is required")
	}

	req := &ChatRequest{Model: wire.Model, Messages: messages, Stream: wire.Stream,
		MaxTokens: wire.MaxTokens}

	if len(wire.Tools) > messagesMaxTools {
		return nil, invalidMsg("tools", "too many tools")
	}
	tools, err := anthropicToolsToOpenAI(wire.Tools)
	if err != nil {
		return nil, err
	}
	req.Tools = tools
	parallel := true
	if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
		tc, disParallel, err := anthropicToolChoiceToOpenAI(wire.ToolChoice)
		if err != nil {
			return nil, err
		}
		req.ToolChoice = tc
		if disParallel {
			parallel = false
		}
	}
	passthrough := map[string]any{}
	if !parallel {
		passthrough["parallel_tool_calls"] = false
	}
	if wire.Temperature != nil {
		passthrough["temperature"] = *wire.Temperature
	}
	if wire.TopP != nil {
		passthrough["top_p"] = *wire.TopP
	}
	if wire.TopK != nil {
		if *wire.TopK <= 0 {
			return nil, invalidMsg("top_k", "top_k must be > 0")
		}
		passthrough["top_k"] = *wire.TopK
	}
	if len(wire.StopSequences) > 0 {
		for _, s := range wire.StopSequences {
			if s == "" {
				return nil, invalidMsg("stop_sequences", "empty stop sequence")
			}
		}
		passthrough["stop"] = wire.StopSequences
	}
	if len(passthrough) > 0 {
		req.Passthrough = passthrough
	}
	if len(wire.Thinking) > 0 && string(wire.Thinking) != "null" {
		var th struct {
			Type         string `json:"type"`
			BudgetTokens *int64 `json:"budget_tokens"`
		}
		if err := json.Unmarshal(wire.Thinking, &th); err != nil {
			return nil, invalidMsg("thinking", "invalid thinking parameter")
		}
		switch th.Type {
		case "enabled":
			on := true
			req.ThinkingEnabled = &on
			if th.BudgetTokens != nil {
				if *th.BudgetTokens < anthropicMinThinkingBudget {
					return nil, invalidMsg("thinking", "budget_tokens must be >= 1024")
				}
				if *th.BudgetTokens >= *wire.MaxTokens {
					return nil, invalidMsg("thinking",
						"budget_tokens must be less than max_tokens")
				}
				req.ThinkingBudget = th.BudgetTokens
			}
		case "disabled":
			off := false
			req.ThinkingEnabled = &off
		default:
			return nil, invalidMsg("thinking", "thinking.type must be enabled or disabled")
		}
	}
	if len(wire.Metadata) > 0 && string(wire.Metadata) != "null" {
		// metadata is a client-side tag (echo-only upstream); validate the
		// shape, then it is intentionally not consumed (capability matrix).
		var meta struct {
			UserID *string `json:"user_id"`
		}
		if err := json.Unmarshal(wire.Metadata, &meta); err != nil {
			return nil, invalidMsg("metadata", "metadata must be an object")
		}
	}
	return req, nil
}

func invalidMsg(field, msg string) error {
	return domain.NewError(domain.CodeInvalidInput, field+": "+msg)
}

func nonSystemCount(ms []model.ChatMessage) int {
	n := 0
	for _, m := range ms {
		if m.Role != "system" {
			n++
		}
	}
	return n
}

// anthropicSystemText normalizes the system field (string | text block
// array) into one string.
func anthropicSystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", invalidMsg("system", "system must be a string or an array of text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", invalidMsg("system", "only text blocks are supported in system")
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// anthropicTurnToChat converts one Anthropic message (content string or
// block array) into internal chat messages. One Anthropic turn can expand
// into several chat messages: tool_result blocks become tool-role messages
// (leading, matching the upstream adapter's merge-back semantics), text
// stays on the user/assistant turn, tool_use blocks become assistant
// tool_calls — with the tool_use id preserved VERBATIM as the chat
// tool_call id (多轮工具调用 ID 保留).
func anthropicTurnToChat(role string, content json.RawMessage) ([]model.ChatMessage, error) {
	if len(content) == 0 || string(content) == "null" {
		return nil, invalidMsg("messages", "message content is required")
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		if asString == "" {
			return nil, invalidMsg("messages", "message content must not be empty")
		}
		return []model.ChatMessage{{Role: role, Content: asString}}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		// text / thinking
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
		// tool_use
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		// tool_result
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   *bool           `json:"is_error"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, invalidMsg("messages", "content must be a string or an array of content blocks")
	}
	if len(blocks) == 0 {
		return nil, invalidMsg("messages", "content blocks must not be empty")
	}

	var out []model.ChatMessage
	var textParts []string
	var toolCalls []model.ToolCall
	flushTurn := func() {
		if len(textParts) == 0 && len(toolCalls) == 0 {
			return
		}
		cm := model.ChatMessage{Role: role, Content: strings.Join(textParts, "\n"), ToolCalls: toolCalls}
		out = append(out, cm)
		textParts, toolCalls = nil, nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking", "redacted_thinking":
			// 历史推理块不回放（chat 形状上游无对应语义；能力矩阵明示）。
			// 接受而不报错：Anthropic 客户端总在多轮回传它们。
			continue
		case "tool_use":
			if role != "assistant" {
				return nil, invalidMsg("messages", "tool_use blocks are only valid in assistant messages")
			}
			if b.ID == "" || b.Name == "" {
				return nil, invalidMsg("messages", "tool_use blocks require id and name")
			}
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				if !json.Valid(b.Input) {
					return nil, invalidMsg("messages", "tool_use input must be a JSON value")
				}
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, model.ToolCall{
				ID: b.ID, Type: "function",
				Function: model.ToolCallFunction{Name: b.Name, Arguments: args},
			})
		case "tool_result":
			if role != "user" {
				return nil, invalidMsg("messages", "tool_result blocks are only valid in user messages")
			}
			if b.ToolUseID == "" {
				return nil, invalidMsg("messages", "tool_result blocks require tool_use_id")
			}
			text, err := anthropicToolResultText(b.Content)
			if err != nil {
				return nil, err
			}
			// tool_result 块引出独立的 tool 角色消息（先行），与上游
			// Anthropic 适配器把 tool 消息合并回 user 轮前导块的语义对称。
			// is_error 标志不转发（无对应字段），错误文本内容照常传递。
			flushTurn()
			out = append(out, model.ChatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
		case "image", "document":
			return nil, invalidMsg("messages",
				b.Type+" content blocks are not supported by this gateway (text-only surface)")
		default:
			return nil, invalidMsg("messages", "unsupported content block type: "+b.Type)
		}
	}
	flushTurn()
	if len(out) == 0 {
		// e.g. an assistant turn of only thinking blocks — carries no
		// replayable information (thinking replay is out of scope).
		return nil, invalidMsg("messages", "message carries no replayable content")
	}
	return out, nil
}

// anthropicToolResultText normalizes tool_result content (string | text
// block array) into one string.
func anthropicToolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", invalidMsg("messages", "tool_result content must be a string or text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", invalidMsg("messages", "only text content is supported in tool_result")
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// anthropicToolsToOpenAI converts Anthropic custom-tool definitions into the
// internal OpenAI tool shape. Server-side tool types (web_search, computer,
// bash, code_execution, mcp, ...) are rejected explicitly — this gateway has
// no server-tool execution surface.
func anthropicToolsToOpenAI(raw []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(raw))
	total := 0
	for _, t := range raw {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(t, &tool); err != nil {
			return nil, invalidMsg("tools", "invalid tool definition")
		}
		if tool.Type != "" && tool.Type != "custom" {
			return nil, invalidMsg("tools",
				"server-side tool type "+tool.Type+" is not supported by this gateway")
		}
		if tool.Name == "" {
			return nil, invalidMsg("tools", "tool name is required")
		}
		schema := tool.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		entry := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       tool.Name,
				"parameters": json.RawMessage(schema),
			},
		}
		if tool.Description != "" {
			entry["function"].(map[string]any)["description"] = tool.Description
		}
		b, err := json.Marshal(entry)
		if err != nil {
			return nil, invalidMsg("tools", "invalid tool schema")
		}
		total += len(b)
		out = append(out, b)
	}
	if total > messagesMaxToolsB {
		return nil, invalidMsg("tools", "tools too large")
	}
	return out, nil
}

// anthropicToolChoiceToOpenAI maps Anthropic tool_choice onto the internal
// OpenAI shape; disable_parallel_tool_use maps to parallel_tool_calls=false
// (the upstream adapters fold it back per protocol).
func anthropicToolChoiceToOpenAI(raw json.RawMessage) (json.RawMessage, bool, error) {
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, false, invalidMsg("tool_choice", "invalid tool_choice")
	}
	var out any
	switch tc.Type {
	case "auto":
		out = "auto"
	case "none":
		out = "none"
	case "any":
		out = "required"
	case "tool":
		if tc.Name == "" {
			return nil, false, invalidMsg("tool_choice", "tool_choice.type=tool requires name")
		}
		out = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	default:
		return nil, false, invalidMsg("tool_choice", "unsupported tool_choice type: "+tc.Type)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false, invalidMsg("tool_choice", "invalid tool_choice")
	}
	return b, tc.DisableParallelToolUse, nil
}

// ---------------------------------------------------------------------------
// Non-streaming response translation (内部 chat.completion → Messages 响应)
// ---------------------------------------------------------------------------

// AnthropicMessageFromCompletion renders the Anthropic Messages response
// body from the internal non-stream payload. cacheReadInInput tells whether
// the wire's prompt_tokens already contains the cached_tokens bucket (the
// winning upstream adapter's Inclusion) — Anthropic usage semantics EXCLUDE
// cache reads from input_tokens, so they are subtracted only when included.
func AnthropicMessageFromCompletion(payload []byte, publicModel string, cacheReadInInput bool) ([]byte, error) {
	v, err := parseCompletion(payload)
	if err != nil {
		return nil, err
	}
	if len(v.Choices) == 0 {
		return nil, fmt.Errorf("providers: internal completion has no choices")
	}
	ch := v.Choices[0]

	id := v.ID
	if !strings.HasPrefix(id, "msg_") {
		id = newPublicID("msg_")
	}
	var content []any
	if ch.Message.ReasoningContent != "" {
		// 非 Anthropic 原生上游无签名可给；signature 为空串（能力矩阵明示：
		// 展示正常，回传历史按 Anthropic 规则被丢弃——本面本就不回放）。
		content = append(content, map[string]any{
			"type": "thinking", "thinking": ch.Message.ReasoningContent, "signature": "",
		})
	}
	if ch.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		var input any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil || input == nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}

	out := map[string]any{
		"id": id, "type": "message", "role": "assistant",
		"model": publicModel, "content": content,
		"stop_reason":   chatFinishToAnthropicStop(ch.FinishReason),
		"stop_sequence": nil,
	}
	if v.Usage != nil {
		out["usage"] = anthropicUsageShape(v.Usage, cacheReadInInput)
	}
	return json.Marshal(out)
}

// anthropicUsageShape maps the wire usage onto the Messages usage object:
// input_tokens EXCLUDES cache reads (subtracted only when the wire included
// them); output_tokens includes thinking (Anthropic 口径).
func anthropicUsageShape(u *openAIUsage, cacheReadInInput bool) map[string]any {
	input := derefOr(u.PromptTokens, 0)
	var cached int64
	if u.PromptDetails != nil {
		cached = derefOr(u.PromptDetails.CachedTokens, 0)
	}
	if cacheReadInInput {
		input -= cached
		if input < 0 {
			input = 0
		}
	}
	usage := map[string]any{
		"input_tokens":  input,
		"output_tokens": derefOr(u.CompletionTokens, 0),
	}
	if cached > 0 {
		usage["cache_read_input_tokens"] = cached
	}
	if u.CacheCreationTokens != nil && *u.CacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = *u.CacheCreationTokens
	}
	return usage
}

// chatFinishToAnthropicStop maps the internal finish reasons onto Anthropic
// stop reasons (mapAnthropicStopReason 的逆向).
func chatFinishToAnthropicStop(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// ---------------------------------------------------------------------------
// Streaming translation (内部 chunk SSE → Messages SSE)
// ---------------------------------------------------------------------------

// anthropicStreamRenderer renders the internal OpenAI chunk stream as
// Anthropic Messages SSE events.
//
// Block bookkeeping: text and thinking blocks each occupy one content-block
// index; every tool call gets its own index keyed from the wire's
// tool_calls[].index (工具调用 ID 与参数片段逐字转发，跨块不重排).
// 评审轮2 I4：tool_use 块按 wire index 保持开启——OpenAI 允许 parallel tool
// calls 交错，迟到的 delta 必须仍落在自己的 block 上（Anthropic SDK 按
// index 归集 content_block_*；对已 stop 的块再发 delta 会被拒绝）。
type anthropicStreamRenderer struct {
	messageID        string
	model            string
	cacheReadInInput bool

	started       bool
	openTextBlock int // currently open text/thinking block index (-1 = none)
	openTextKind  string
	nextBlock     int
	toolBlocks    map[int]*anthropicToolBlock // wire tool_calls[].index → block
	finish        string
	usageEmitted  bool
	errorRelayed  bool
}

// anthropicToolBlock is one in-flight tool_use content block keyed by the
// wire tool_calls[].index.
type anthropicToolBlock struct {
	index int
	open  bool
}

// TranslateOpenAIToAnthropicStream converts the internal chunk SSE stream
// into Anthropic Messages SSE. messageID/model are the public identities
// (上游模型名不外泄).
func TranslateOpenAIToAnthropicStream(body io.ReadCloser, messageID, model string, cacheReadInInput bool) io.ReadCloser {
	return translateStream(body, &anthropicStreamRenderer{
		messageID: messageID, model: model, cacheReadInInput: cacheReadInInput,
		openTextBlock: -1, toolBlocks: map[int]*anthropicToolBlock{},
	})
}

func (r *anthropicStreamRenderer) Render(ev pumpEvent, w io.Writer) error {
	if ev.done {
		// [DONE]: close out anything still open. A stream that legitimately
		// ended always emitted its finish chunk first; a bare [DONE] without
		// one still closes the protocol stream honestly (no fake content).
		if !r.started {
			if err := r.emitStart(w); err != nil {
				return err
			}
		}
		if err := r.closeOpenBlock(w); err != nil {
			return err
		}
		if !r.usageEmitted {
			if err := r.emitMessageDelta(w, nil); err != nil {
				return err
			}
		}
		return writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	}
	chunk := ev.chunk
	if chunk == nil {
		return nil
	}
	if chunk.Error != nil {
		r.errorRelayed = true
		if !r.started {
			if err := r.emitStart(w); err != nil {
				return err
			}
		}
		return writeSSE(w, "error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": chunk.Error.Message},
		})
	}
	if !r.started {
		if err := r.emitStart(w); err != nil {
			return err
		}
	}
	// Terminal usage chunk (choices empty, usage set): message_delta carries
	// the REAL totals — message_start's usage is a protocol-required
	// placeholder (input is only known at stream end on chat-shaped
	// upstreams; 能力矩阵明示).
	if chunk.Usage != nil {
		r.usageEmitted = true
		return r.emitMessageDelta(w, chunk.Usage)
	}
	for _, choice := range chunk.Choices {
		d := choice.Delta
		if d.ReasoningContent != "" {
			if err := r.openTextualBlock(w, "thinking"); err != nil {
				return err
			}
			if err := writeSSE(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": r.openTextBlock,
				"delta": map[string]any{"type": "thinking_delta", "thinking": d.ReasoningContent},
			}); err != nil {
				return err
			}
		}
		if d.Content != "" {
			if err := r.openTextualBlock(w, "text"); err != nil {
				return err
			}
			if err := writeSSE(w, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": r.openTextBlock,
				"delta": map[string]any{"type": "text_delta", "text": d.Content},
			}); err != nil {
				return err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := r.renderToolCallDelta(w, tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
				return err
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			r.finish = *choice.FinishReason
			if err := r.closeOpenBlock(w); err != nil {
				return err
			}
		}
	}
	return nil
}

// BrokenEnd implements streamRenderer: the stream ended without [DONE] —
// emit the native error event and NO message_stop (半截答案不得渲染为完整).
func (r *anthropicStreamRenderer) BrokenEnd(w io.Writer) error {
	if r.errorRelayed {
		return nil // in-band error chunk already relayed
	}
	if !r.started {
		if err := r.emitStart(w); err != nil {
			return err
		}
	}
	return writeSSE(w, "error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": "upstream stream interrupted"},
	})
}

// emitStart writes message_start. usage is the protocol-required placeholder
// (0/0); the REAL usage arrives on the terminal message_delta (the current
// Anthropic SDK's MessageDeltaUsage carries input_tokens as well).
func (r *anthropicStreamRenderer) emitStart(w io.Writer) error {
	r.started = true
	r.openTextBlock = -1
	return writeSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": r.messageID, "type": "message", "role": "assistant",
			"model": r.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// openTextualBlock opens a text/thinking block when not already open
// (kind 切换关闭当前文本块；tool_use 块不受影响——它们按 wire index 独立
// 保持开启).
func (r *anthropicStreamRenderer) openTextualBlock(w io.Writer, kind string) error {
	if r.openTextKind == kind && r.openTextBlock >= 0 {
		return nil
	}
	if err := r.closeTextBlock(w); err != nil {
		return err
	}
	r.openTextBlock = r.nextBlock
	r.nextBlock++
	r.openTextKind = kind
	block := map[string]any{"type": kind}
	switch kind {
	case "text":
		block["text"] = ""
	case "thinking":
		block["thinking"] = ""
		block["signature"] = ""
	}
	return writeSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": r.openTextBlock, "content_block": block,
	})
}

// renderToolCallDelta maps one wire tool_calls[] delta onto tool_use block
// events. The FIRST delta of a call (carrying id+name) opens ITS OWN block
// keyed by the wire index; argument fragments become input_json_delta
// partial_json (逐字). 评审轮2 I4：交错 parallel tool calls 下，开 ix1 的
// 块不得关闭 ix0 的块——只为新块收尾开启中的文本/思考块。
func (r *anthropicStreamRenderer) renderToolCallDelta(w io.Writer, ix int, id, name, args string) error {
	tb, ok := r.toolBlocks[ix]
	if !ok {
		if err := r.closeTextBlock(w); err != nil {
			return err
		}
		tb = &anthropicToolBlock{index: r.nextBlock, open: true}
		r.nextBlock++
		r.toolBlocks[ix] = tb
		if err := writeSSE(w, "content_block_start", map[string]any{
			"type": "content_block_start", "index": tb.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
			},
		}); err != nil {
			return err
		}
	}
	if args == "" {
		return nil
	}
	return writeSSE(w, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tb.index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
}

// closeOpenBlock closes every open block — the text/thinking block plus
// each in-flight tool_use block — in block-index order.
func (r *anthropicStreamRenderer) closeOpenBlock(w io.Writer) error {
	var indexes []int
	if r.openTextBlock >= 0 {
		indexes = append(indexes, r.openTextBlock)
	}
	for _, tb := range r.toolBlocks {
		if tb.open {
			indexes = append(indexes, tb.index)
		}
	}
	sort.Ints(indexes)
	for _, ix := range indexes {
		if err := r.closeBlock(w, ix); err != nil {
			return err
		}
	}
	return nil
}

// closeTextBlock closes the open text/thinking block only.
func (r *anthropicStreamRenderer) closeTextBlock(w io.Writer) error {
	if r.openTextBlock < 0 {
		return nil
	}
	return r.closeBlock(w, r.openTextBlock)
}

// closeBlock emits content_block_stop for one block and marks it closed.
func (r *anthropicStreamRenderer) closeBlock(w io.Writer, ix int) error {
	if r.openTextBlock == ix {
		r.openTextBlock = -1
		r.openTextKind = ""
	}
	for _, tb := range r.toolBlocks {
		if tb.index == ix {
			tb.open = false
		}
	}
	return writeSSE(w, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": ix,
	})
}

// emitMessageDelta writes the terminal message_delta: mapped stop_reason
// plus (when known) the real usage totals.
func (r *anthropicStreamRenderer) emitMessageDelta(w io.Writer, usage *openAIUsage) error {
	stop := chatFinishToAnthropicStop(r.finish)
	msg := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
	}
	if usage != nil {
		u := anthropicUsageShape(usage, r.cacheReadInInput)
		// message_delta usage: output_tokens official; input_tokens carried
		// for current SDK versions (MessageDeltaUsage.input_tokens).
		msg["usage"] = u
	}
	return writeSSE(w, "message_delta", msg)
}
