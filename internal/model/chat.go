package model

import "encoding/json"

// ChatMessage is one turn of a chat conversation, in the OpenAI-compatible
// shape that the DeepSeek chat.completions API consumes. The server proxies
// these verbatim upstream — kaya owns conversation history and sends the
// full context each request (stateless proxy, no server-side sessions).
type ChatMessage struct {
	Role       string     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 轮发起的工具调用(透传上游)
	ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时关联的 assistant tool_call id
	// ReasoningContent 是 thinking 模式下 assistant 轮的推理内容(透传上游)。
	// DeepSeek 要求:带 tool_calls 的 assistant 轮在后续请求中必须原样回传
	// reasoning_content,否则上游 400 拒绝。omitempty 保证非 thinking 会话
	// 的上行 payload 与之前逐字节一致(向后兼容)。其大小有意不计入消息条
	// 数预算(长推理链是合法输入,按内容预算拒绝会误伤),由请求体总上限
	// chatMaxBodyBytes 兜底——与 tool_calls 的处理方式一致。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall is the OpenAI Chat Completions assistant tool_call shape. The
// server proxies it verbatim upstream — it never parses the contents, only
// bounds message body size (see validateChatMessages). Keeping it structured
// (instead of flattening to text) is what lets the model emit native
// tool_calls instead of mimicking a text "[tool call: ...]" annotation in
// history (which broke tool calling on the built-in model).
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the function payload of a tool_call. Arguments is a
// JSON-encoded string (the OpenAI streaming-protocol shape), not an object —
// matching what kaya serializes on the client side.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatRequest is the POST /chat request body. `stream` is intentionally not
// exposed — the endpoint always streams (SSE) so kaya gets a typewriter
// experience without an opt-in knob to misconfigure.
//
// SessionID is optional and opaque: kaya passes its own conversation/session
// identifier so chat access logs can group requests per session. The server
// only validates length and echoes it into the audit log — it never uses it
// for state.
// Tools / ThinkingEnabled are relayed verbatim upstream (see
// ChatService.StreamChat). They are optional: a client that only wants plain
// chat omits them and the upstream payload is byte-identical to the
// pre-tool-proxy shape.
type ChatRequest struct {
	Messages  []ChatMessage `json:"messages"`
	SessionID string        `json:"session_id"`
	// Model is the logical built-in model id (see GET /chat/models). Empty
	// selects the server-configured default — pre-multi-model clients never
	// send it and keep working unchanged. When the inference-gateway
	// migration switch is on (INFERENCE_KAYA_CHAT_GATEWAY), the facade
	// honors it as the public model id (empty = KAYA_CHAT_MODEL).
	Model string `json:"model,omitempty"`
	// Tools is the OpenAI-compatible function/tool schema list, relayed
	// verbatim to the upstream DeepSeek chat.completions `tools` field.
	// The server treats it as opaque JSON — it never parses tool contents,
	// only bounds total size (ChatMaxToolsBytes) and count (ChatMaxTools)
	// so a hostile client can't push a multi-MB schema through the proxy.
	// Omitted when the client only wants plain chat.
	Tools []json.RawMessage `json:"tools,omitempty"`
	// ThinkingEnabled relays kaya's reasoning toggle to the upstream
	// DeepSeek `thinking: {"type": "enabled"}` parameter. Omitted (nil)
	// when the client didn't ask for thinking mode.
	ThinkingEnabled *bool `json:"thinking_enabled,omitempty"`
}

// ChatMaxMessages bounds the number of turns per request (abuse surface:
// each request proxies to a paid upstream). Messages beyond this are
// rejected with 400 before any upstream spend.
const ChatMaxMessages = 20

// ChatMaxMessageBytes bounds a single non-system message's content length in
// BYTES (len(), not runes — CJK content counts ~3 bytes per character).
// R6: raised 32768 → 4 MiB so a single message can carry a near-1M-token
// context (~4 bytes/token English worst case), matching the inference
// catalog's context_tokens=1048576. The server cap is deliberately ABOVE the
// kaya client's MAX_MESSAGE_BYTES (still 32768): the client truncates to its
// own budget before sending, so a more permissive server never rejects
// client-shaped payloads — the client's constants are raised by a separate
// kaya release, until which the effective ceiling stays client-side.
const ChatMaxMessageBytes = 4 << 20

// ChatMaxSystemBytes bounds a single system message's content length. kaya's
// rendered system prompt is ~21-24 KB (commander role section, 2026-09-25:
// en/Windows worst form measured 7 bytes below the old 24576 cap, and the
// trusted-root path is interpolated ~21x so longer home paths overflow at
// runtime), so system messages get their own budget that matches the client
// (see MAX_SYSTEM_BYTES in yunhou_chat.rs). This must stay in sync with the
// client. Deploy server-first: bumping the client before this lands makes
// built-in chat hard-fail with 400 "message content too long".
const ChatMaxSystemBytes = 32768

// ChatMaxTotalBytes bounds the total request size in bytes across all
// messages. R6: raised 262144 → 4 MiB (same 1M-token rationale as
// ChatMaxMessageBytes; the invariant total ≥ per-message is preserved so a
// single full-context message is not rejected by the aggregate budget).
// Server ≥ client MAX_TOTAL_BYTES (262144) — see ChatMaxMessageBytes.
const ChatMaxTotalBytes = 4 << 20

// ChatMaxSessionIDLen bounds the optional session_id field — it is only an
// audit-log grouping key, so anything longer is rejected rather than stored.
const ChatMaxSessionIDLen = 64

// ChatMaxModelLen bounds the optional model id — it must match a catalog
// entry, and catalog ids are themselves capped at 64 chars (llm.Validate).
const ChatMaxModelLen = 64

// ChatMaxTools bounds the number of tool definitions per request (abuse
// surface: each tool inflates the upstream prompt and costs tokens).
const ChatMaxTools = 16

// ChatMaxToolsBytes bounds the total serialized size of the tools array.
// Tool schemas are typically a few hundred bytes each (kaya ships 4 tools);
// 32 KiB covers pathological schemas while bounding memory per request.
const ChatMaxToolsBytes = 32 << 10
