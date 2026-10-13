package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ContentPart is one element of a multimodal message content array (OpenAI
// shape). Only "text" and "image_url" are accepted at unmarshal time —
// anything else is a client error surfaced as 400, never silently dropped.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries one image reference: an http(s) URL the upstream fetches
// itself (this server never fetches it — no SSRF surface), or an inline
// data:image/...;base64 URI.
type ImageURL struct {
	URL string `json:"url"`
	// Detail 是 OpenAI 视觉分档参数("low"/"high"/"auto"),透传上游;
	// Anthropic 无此概念,转换时忽略。透传是有意为之的规格决定(向前兼
	// 容):不设取值白名单,上游未来新增 detail 值无需服务端变更即可生效;
	// 非法值由上游 400 拒绝并透传为客户端错误。
	Detail string `json:"detail,omitempty"`
}

// ChatMessage is one turn of a chat conversation, in the OpenAI-compatible
// shape that the DeepSeek chat.completions API consumes. The server proxies
// these verbatim upstream — kaya owns conversation history and sends the
// full context each request (stateless proxy, no server-side sessions).
type ChatMessage struct {
	Role    string `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content string `json:"content"`
	// ContentParts is non-nil when the client sent content as a multimodal
	// array. json:"-" so default marshaling (audit logs) stays string-only
	// and never leaks base64 image data; upstream builders call
	// OpenAIWireContent / AnthropicImageBlock explicitly. Content then holds
	// the concatenated text parts so size validation and byte metrics keep
	// working unchanged.
	ContentParts []ContentPart `json:"-"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`   // assistant 轮发起的工具调用(透传上游)
	ToolCallID   string        `json:"tool_call_id,omitempty"` // role=tool 时关联的 assistant tool_call id
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

// ChatMaxImages bounds image parts per request (each is billed upstream
// vision tokens).
const ChatMaxImages = 4

// ChatMaxImageBytes bounds one image_url value in WIRE bytes — base64
// characters for data URIs (~768 KiB decoded) or URL characters. Wire bytes
// are what the body cap sees and what len() measures at validation, so there
// is no post-decode expansion window.
const ChatMaxImageBytes = 1 << 20

// ChatMaxTotalImageBytes bounds summed image_url wire bytes per request.
// 3 MiB (not 4): worst legal body = 4 MiB text + 3 MiB images + JSON/
// reasoning overhead stays under the 8 MiB chatMaxBodyBytes cap, so the
// precise validation 400 always fires before the generic body-cap rejection.
const ChatMaxTotalImageBytes = 3 << 20

// UnmarshalJSON accepts content as a plain string (unchanged legacy form), a
// multimodal parts array, or null (legal on assistant tool-call turns).
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ToolCalls        []ToolCall      `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		ReasoningContent string          `json:"reasoning_content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role, m.ToolCalls, m.ToolCallID, m.ReasoningContent = wire.Role, wire.ToolCalls, wire.ToolCallID, wire.ReasoningContent
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	if wire.Content[0] != '[' {
		return json.Unmarshal(wire.Content, &m.Content)
	}
	var parts []ContentPart
	if err := json.Unmarshal(wire.Content, &parts); err != nil {
		return err
	}
	if len(parts) == 0 {
		// "content":[] 归位为 content:""(ContentParts 保持 nil,走 legacy
		// 路径),否则空数组会让 role=tool / 带 tool_calls 的 assistant 轮以
		// ContentParts != nil 通过校验,上行 content 变成 [] 而非 ""——
		// 比 master 的 400 绑定错误放得更宽。
		parts = nil
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			sb.WriteString(p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return fmt.Errorf("image_url part requires image_url.url")
			}
		default:
			return fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	m.ContentParts = parts
	m.Content = sb.String()
	return nil
}

// HasImages reports whether the message carries at least one image part.
func (m *ChatMessage) HasImages() bool {
	for _, p := range m.ContentParts {
		if p.Type == "image_url" {
			return true
		}
	}
	return false
}

// MessagesHaveImages is the vision-gate trigger for a whole request.
func MessagesHaveImages(messages []ChatMessage) bool {
	for i := range messages {
		if messages[i].HasImages() {
			return true
		}
	}
	return false
}

// OpenAIWireContent returns the OpenAI wire form of the message content: the
// plain string, or the parts array when ContentParts is set.
func (m ChatMessage) OpenAIWireContent() any {
	if m.ContentParts == nil {
		return m.Content
	}
	parts := make([]any, 0, len(m.ContentParts))
	for _, p := range m.ContentParts {
		switch p.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			// 防御性跳过:Unmarshal 保证 image_url part 的 ImageURL 非 nil,
			// 这里保护进程内手工构造 ContentParts 的调用方。
			if p.ImageURL == nil {
				continue
			}
			img := map[string]any{"url": p.ImageURL.URL}
			if p.ImageURL.Detail != "" {
				img["detail"] = p.ImageURL.Detail
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
		}
	}
	return parts
}

// AnthropicImageBlock converts one image_url value into an Anthropic image
// block: data URIs become base64 sources (image/jpg normalized to
// image/jpeg), http(s) URLs become url sources. The media type is the
// entry-validated declared type (validated against magic bytes at the edge).
func AnthropicImageBlock(u string) map[string]any {
	if mt, data, ok := ParseDataImageURI(u); ok {
		if mt == "image/jpg" {
			mt = "image/jpeg"
		}
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mt, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
}

// AnthropicUserBlocks builds the Anthropic user-turn content blocks: one text
// block for legacy string content (byte-identical to the pre-multimodal
// shape), or one block per part for multimodal messages. Shared by both
// Anthropic adapters (internal/llm, internal/inference/providers) so their
// user-turn translation stays identical. A nil ImageURL is skipped
// defensively — Unmarshal guarantees non-nil, in-process constructors may not.
func AnthropicUserBlocks(m ChatMessage) []any {
	if m.ContentParts == nil {
		return []any{map[string]any{"type": "text", "text": m.Content}}
	}
	blocks := make([]any, 0, len(m.ContentParts))
	for _, p := range m.ContentParts {
		switch p.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			blocks = append(blocks, AnthropicImageBlock(p.ImageURL.URL))
		}
	}
	return blocks
}

// ParseDataImageURI splits "data:image/png;base64,<data>" into media type and
// payload. ok=false for any other shape (including non-base64 data URIs).
func ParseDataImageURI(u string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	i := strings.Index(rest, ";base64,")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(";base64,"):], true
}

// IsAllowedImageMediaType gates the data-URI formats we relay upstream. SVG
// is deliberately excluded (scriptable markup, not an image to models).
func IsAllowedImageMediaType(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return true
	}
	return false
}

// SniffImageMediaType identifies an image from its decoded magic bytes.
// Returns the canonical MIME type ("image/jpeg" for jpg too) or "" when the
// prefix matches no supported signature. 18 bytes cover every signature.
func SniffImageMediaType(prefix []byte) string {
	switch {
	case len(prefix) >= 8 && bytes.Equal(prefix[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(prefix) >= 3 && prefix[0] == 0xFF && prefix[1] == 0xD8 && prefix[2] == 0xFF:
		return "image/jpeg"
	case len(prefix) >= 6 && (string(prefix[:6]) == "GIF87a" || string(prefix[:6]) == "GIF89a"):
		return "image/gif"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
