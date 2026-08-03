package model

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
type ChatRequest struct {
	Messages  []ChatMessage `json:"messages"`
	SessionID string        `json:"session_id"`
}

// ChatMaxMessages bounds the number of turns per request (abuse surface:
// each request proxies to a paid upstream). Messages beyond this are
// rejected with 400 before any upstream spend.
const ChatMaxMessages = 20

// ChatMaxMessageBytes bounds a single message's content length in BYTES
// (len(), not runes — CJK content counts ~3 bytes per character). Long
// paste-in of documents should be chunked by the caller.
const ChatMaxMessageBytes = 8000

// ChatMaxTotalBytes bounds the total request size in bytes across all
// messages.
const ChatMaxTotalBytes = 32000

// ChatMaxSessionIDLen bounds the optional session_id field — it is only an
// audit-log grouping key, so anything longer is rejected rather than stored.
const ChatMaxSessionIDLen = 64
