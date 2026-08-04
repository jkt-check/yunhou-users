package model

import "encoding/json"

// ChatMessage is one turn of a chat conversation, in the OpenAI-compatible
// shape that the DeepSeek chat.completions API consumes. The server proxies
// these verbatim upstream — kaya owns conversation history and sends the
// full context each request (stateless proxy, no server-side sessions).
type ChatMessage struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
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
// BYTES (len(), not runes — CJK content counts ~3 bytes per character). Long
// paste-in of documents should be chunked by the caller.
const ChatMaxMessageBytes = 8000

// ChatMaxSystemBytes bounds a single system message's content length. kaya's
// rendered system prompt is ~21-23 KB, well above the per-message cap, so
// system messages get their own budget that matches the client (see
// MAX_SYSTEM_BYTES in yunhou_chat.rs). This must stay in sync with the client.
const ChatMaxSystemBytes = 24576

// ChatMaxTotalBytes bounds the total request size in bytes across all
// messages. Matches the client's MAX_TOTAL_BYTES.
const ChatMaxTotalBytes = 65536

// ChatMaxSessionIDLen bounds the optional session_id field — it is only an
// audit-log grouping key, so anything longer is rejected rather than stored.
const ChatMaxSessionIDLen = 64

// ChatMaxTools bounds the number of tool definitions per request (abuse
// surface: each tool inflates the upstream prompt and costs tokens).
const ChatMaxTools = 16

// ChatMaxToolsBytes bounds the total serialized size of the tools array.
// Tool schemas are typically a few hundred bytes each (kaya ships 4 tools);
// 32 KiB covers pathological schemas while bounding memory per request.
const ChatMaxToolsBytes = 32 << 10
