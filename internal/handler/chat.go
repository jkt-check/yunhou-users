package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// chatStreamer is the ChatService surface the handler needs. Defined as a
// local interface so handler tests can inject a hand-rolled mock without a
// real upstream.
type chatStreamer interface {
	StreamChat(ctx context.Context, userID, appID string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error)
}

// ChatHandler serves POST /chat — the JWT-authenticated, subscription-gated
// DeepSeek proxy. The upstream SSE stream is relayed verbatim to kaya; every
// non-streaming outcome (auth, validation, access, upstream handshake) is a
// normal JSON error before the stream starts.
//
// accessLog, when non-nil, receives one JSON line per request (success AND
// failure) with user_id, session_id, input messages, output text, status and
// duration — the chat audit trail. Nil disables access logging.
type ChatHandler struct {
	svc       chatStreamer
	accessLog *log.Logger
}

func NewChatHandler(svc chatStreamer, accessLog *log.Logger) *ChatHandler {
	return &ChatHandler{svc: svc, accessLog: accessLog}
}

// chatStreamBufSize is the relay chunk size. 32 KiB balances flush latency
// against syscall overhead for the SSE relay loop.
const chatStreamBufSize = 32 << 10

// chatMaxBodyBytes caps the total request body size. Legal payloads:
// messages ≤256 KiB + tools ≤32 KiB + JSON overhead ≈ 290 KiB — 320 KiB
// leaves ~30 KiB headroom while bounding MaxBytesReader allocation for
// hostile bodies (spec §5.1: 128 KiB → 320 KiB).
const chatMaxBodyBytes = 320 << 10

// chatWriteTimeout is the per-response write deadline for /chat streams,
// set via http.ResponseController. Slightly above ChatService's 5m upstream
// timeout so the stream ends by ctx cancellation, never by a write kill.
const chatWriteTimeout = 6 * time.Minute

// chatRawLogCap caps how much of the raw SSE stream is captured for the
// audit log. A typical answer is a few KiB; 256 KiB covers pathological
// outputs while bounding memory per request.
const chatRawLogCap = 256 << 10

// chatAccessEntry is one JSON line of the chat audit log.
type chatAccessEntry struct {
	TS              string              `json:"ts"`
	UserID          string              `json:"user_id"`
	AppID           string              `json:"app_id"`
	SessionID       string              `json:"session_id"`
	Status          string              `json:"status"` // "ok" | "error" | "disconnected" | "upstream_error"
	Error           string              `json:"error,omitempty"`
	MessageCount    int                 `json:"message_count"`
	ToolsCount      int                 `json:"tools_count,omitempty"`
	ThinkingEnabled bool                `json:"thinking_enabled,omitempty"`
	InputBytes      int                 `json:"input_bytes"`
	OutputBytes     int                 `json:"output_bytes"`
	DurationMS      int64               `json:"duration_ms"`
	Input           []model.ChatMessage `json:"input"`
	InputTruncated  bool                `json:"input_truncated,omitempty"`
	Output          string              `json:"output"`
	OutputTruncated bool                `json:"output_truncated,omitempty"`
}

// StreamChat handles POST /chat.
func (h *ChatHandler) StreamChat(c *gin.Context) {
	started := time.Now()
	userID := c.GetString(middleware.ContextUserID)
	appID := c.GetString(middleware.ContextAppID)

	// 请求体总大小上限(滥用面):tools 字段加入后 body 面略增,320 KiB
	// 覆盖 messages(≤256 KiB)+ tools(≤32 KiB)的合法组合,超限拒绝。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, chatMaxBodyBytes)

	var req model.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.logAccess(started, userID, appID, req, "error", "invalid request body", "")
		writeChatError(c, http.StatusBadRequest, err.Error())
		return
	}
	if msg := validateChatMessages(req.Messages); msg != "" {
		h.logAccess(started, userID, appID, req, "error", msg, "")
		writeChatError(c, http.StatusBadRequest, msg)
		return
	}
	if len(req.SessionID) > model.ChatMaxSessionIDLen {
		h.logAccess(started, userID, appID, req, "error", "session_id too long", "")
		writeChatError(c, http.StatusBadRequest, "session_id too long")
		return
	}
	if msg := validateChatTools(req.Tools); msg != "" {
		h.logAccess(started, userID, appID, req, "error", msg, "")
		writeChatError(c, http.StatusBadRequest, msg)
		return
	}

	resp, err := h.svc.StreamChat(c.Request.Context(), userID, appID, req.Messages, req.Tools, req.ThinkingEnabled)
	if err != nil {
		status, msg := chatErrorMapping(err)
		h.logAccess(started, userID, appID, req, "error", msg, "")
		writeChatError(c, status, msg)
		return
	}
	defer resp.Body.Close()

	c.Status(http.StatusOK)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	// Disable nginx response buffering for this stream (honored when nginx
	// sits in front, see deploy/nginx.conf) — without it SSE chunks are
	// held until the proxy buffer fills and the typewriter UX is lost.
	c.Header("X-Accel-Buffering", "no")

	// The global http.Server.WriteTimeout (25s) is an absolute deadline set
	// once per request — it would hard-cut a stream that legitimately runs
	// longer. Give this response its own, chat-sized write deadline instead:
	// slightly above chatUpstreamTimeout (5m) so the stream ends by timeout
	// cancellation, never by a mid-stream write kill. SetWriteDeadline only
	// fails when a wrapper hides the underlying connection — log it, because
	// then the 25s WriteTimeout silently comes back into force.
	rc := http.NewResponseController(c.Writer)
	if err := rc.SetWriteDeadline(time.Now().Add(chatWriteTimeout)); err != nil {
		log.Printf("chat: set write deadline: %v", err)
	}

	raw, result := relayChatSSE(c.Writer, resp.Body)
	if result == chatRelayUpstreamBroke {
		// Upstream died mid-stream (no [DONE] relayed): a clean EOF here
		// would make kaya render the partial answer as complete. Inject an
		// in-stream error event (kaya parses the error key) before ending
		// the response. Bytes already relayed are untouched; a write error
		// means the client is also gone, which changes nothing.
		_, _ = io.WriteString(c.Writer, chatUpstreamBrokeEvent)
		c.Writer.Flush()
	}
	output := extractChatOutput(raw)
	status := "ok"
	errMsg := ""
	switch result {
	case chatRelayClientGone:
		// Client disconnected mid-stream — the captured output is a partial
		// answer; record it as such so the audit trail can't be mistaken
		// for a completed exchange.
		status = "disconnected"
	case chatRelayUpstreamBroke:
		// Upstream broke mid-stream (connection error or the 5m upstream
		// timeout) — also a partial answer, but the cause is on the
		// DeepSeek side, which an operator wants to distinguish from a
		// user closing their tab.
		status = "upstream_error"
		errMsg = "upstream stream interrupted"
	}
	h.logAccess(started, userID, appID, req, status, errMsg, output)
}

// chatErrorMapping converts a StreamChat error into (HTTP status, safe
// client message). Internal details (upstream URL, upstream body) are logged
// server-side by the caller's error branches, never sent to the client.
func chatErrorMapping(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrChatNotEnabled):
		return http.StatusNotFound, service.ErrChatNotEnabled.Error()
	case errors.Is(err, service.ErrChatNoAccess):
		return http.StatusForbidden, service.ErrChatNoAccess.Error()
	case errors.Is(err, service.ErrChatRateLimited):
		log.Printf("chat: upstream rate limited: %v", err)
		return http.StatusTooManyRequests, service.ErrChatRateLimited.Error()
	case errors.Is(err, service.ErrChatUpstreamRejected):
		// Upstream 4xx (≠429) — permanent; retrying the same request will
		// fail again, so the message must not invite a retry.
		log.Printf("chat: upstream rejected: %v", err)
		return http.StatusBadGateway, service.ErrChatUpstreamRejected.Error()
	case errors.Is(err, service.ErrChatUpstreamError):
		log.Printf("chat: upstream error: %v", err)
		return http.StatusBadGateway, service.ErrChatUpstreamError.Error()
	default:
		log.Printf("chat: internal error: %v", err)
		return http.StatusInternalServerError, "internal error"
	}
}

// logAccess appends one chatAccessEntry line to the audit log, if enabled.
// Output text is truncated for the log so a single pathological reply can't
// balloon the file. OutputBytes always reflects the REAL output length
// (before truncation) so cost analysis isn't skewed by the log cap. On
// error lines the input is truncated too: validation-failed requests carry
// unvalidated (potentially near-32 KiB per message) content that would
// otherwise be mirrored into the log in full.
func (h *ChatHandler) logAccess(started time.Time, userID, appID string, req model.ChatRequest, status, errMsg, output string) {
	if h.accessLog == nil {
		return
	}
	realBytes := len(output)
	output, truncated := truncateChatOutput(output)
	input := req.Messages
	inputTruncated := false
	if status == "error" {
		input, inputTruncated = truncateChatInput(req.Messages)
	}
	entry := chatAccessEntry{
		TS:              started.Format(time.RFC3339),
		UserID:          userID,
		AppID:           appID,
		SessionID:       req.SessionID,
		Status:          status,
		Error:           errMsg,
		MessageCount:    len(req.Messages),
		ToolsCount:      len(req.Tools),
		ThinkingEnabled: req.ThinkingEnabled != nil && *req.ThinkingEnabled,
		InputBytes:      chatTotalBytes(req.Messages),
		OutputBytes:     realBytes,
		DurationMS:      time.Since(started).Milliseconds(),
		Input:           input,
		InputTruncated:  inputTruncated,
		Output:          output,
		OutputTruncated: truncated,
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return // log.Logger swallows write errors; don't fail the request for auditing
	}
	h.accessLog.Println(string(b))
}

// chatTotalBytes sums message content lengths in bytes (len() — CJK content
// counts ~3 bytes per rune; the field names say bytes, not chars).
func chatTotalBytes(messages []model.ChatMessage) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
	}
	return total
}

// chatOutputLogCap bounds the output text stored in one audit line.
const chatOutputLogCap = 64 << 10

// chatErrInputLogCap bounds each message's content in an ERROR audit line.
// Error lines log input that failed validation — i.e. content whose size is
// precisely what validation rejected — so it needs its own, smaller cap.
const chatErrInputLogCap = 1 << 10

// truncateChatOutput cuts s at chatOutputLogCap on a UTF-8 rune boundary.
func truncateChatOutput(s string) (string, bool) {
	return truncateUTF8(s, chatOutputLogCap)
}

// truncateChatInput caps every message's content at chatErrInputLogCap for
// error-path audit lines. Returns the (possibly copied) slice and whether
// any content was cut.
func truncateChatInput(messages []model.ChatMessage) ([]model.ChatMessage, bool) {
	truncated := false
	out := messages
	for i, m := range messages {
		if len(m.Content) > chatErrInputLogCap {
			if !truncated {
				// First cut: copy the slice so the caller's request is untouched.
				out = make([]model.ChatMessage, len(messages))
				copy(out, messages[:i])
			}
			cut, _ := truncateUTF8(m.Content, chatErrInputLogCap)
			out[i] = model.ChatMessage{Role: m.Role, Content: cut}
			truncated = true
		} else if truncated {
			out[i] = m
		}
	}
	return out, truncated
}

// truncateUTF8 cuts s at cap bytes on a UTF-8 rune boundary — a byte-slice
// cut could split a multi-byte character, which json.Marshal would silently
// replace with U+FFFD and corrupt the logged text.
func truncateUTF8(s string, cap int) (string, bool) {
	if len(s) <= cap {
		return s, false
	}
	cut := cap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// chatRelayResult reports how an SSE relay ended, so the audit trail can
// distinguish a completed answer from a client disconnect and from an
// upstream break (connection error or the 5m upstream timeout) — the last
// is the one an operator needs to notice.
type chatRelayResult int

const (
	chatRelayOK            chatRelayResult = iota // clean end of stream
	chatRelayClientGone                           // write to the client failed (disconnect)
	chatRelayUpstreamBroke                        // upstream read failed mid-stream
)

// chatUpstreamBrokeEvent is the SSE event injected client-side when the
// upstream stream breaks mid-answer, so kaya sees an explicit error instead
// of a [DONE]-less clean EOF it would render as a completed answer.
const chatUpstreamBrokeEvent = "data: {\"error\":{\"message\":\"upstream stream interrupted\"}}\n\n"

// flushWriter is the slice of gin.ResponseWriter the relay needs, declared
// so tests can drive the relay with hand-rolled fakes.
type flushWriter interface {
	io.Writer
	http.Flusher
}

// relayChatSSE streams the upstream body to the client, flushing after every
// chunk, and captures up to chatRawLogCap bytes of the raw SSE stream for the
// audit log. The result reports how the relay ended (see chatRelayResult).
func relayChatSSE(w flushWriter, body io.Reader) ([]byte, chatRelayResult) {
	buf := make([]byte, chatStreamBufSize)
	var captured bytes.Buffer
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return captured.Bytes(), chatRelayClientGone // client gone — stop relaying
			}
			w.Flush()
			if captured.Len() < chatRawLogCap {
				remaining := chatRawLogCap - captured.Len()
				if n >= remaining {
					captured.Write(buf[:remaining])
				} else {
					captured.Write(buf[:n])
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return captured.Bytes(), chatRelayOK // clean end of stream
			}
			return captured.Bytes(), chatRelayUpstreamBroke // upstream broke mid-stream
		}
	}
}

// extractChatOutput parses an OpenAI-compatible SSE stream and concatenates
// the delta content — the text the user actually saw. Non-standard events
// are skipped; if nothing parses (e.g. an error event stream), the raw
// payload is returned as a fallback so the log still shows something.
func extractChatOutput(raw []byte) string {
	var sb strings.Builder
	sawData := false
	parsed := 0
	for _, event := range strings.Split(string(raw), "\n\n") {
		line := strings.TrimSpace(event)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		sawData = true
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
			parsed++
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	// Fall back to the raw payload when no data: event could be parsed at
	// all (e.g. a CRLF-separated or otherwise non-standard stream would
	// otherwise silently log an empty output).
	if !sawData || parsed == 0 {
		return string(raw)
	}
	return sb.String()
}

// validateChatMessages enforces the abuse bounds from model/chat.go. Returns
// an empty string when valid, otherwise a human-readable reason.
func validateChatMessages(messages []model.ChatMessage) string {
	if len(messages) == 0 {
		return "messages is required"
	}
	if len(messages) > model.ChatMaxMessages {
		return "too many messages"
	}
	total := 0
	for _, m := range messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return "invalid message role"
		}
		// content is required for text-bearing roles. Two cases are exempt
		// (DeepSeek accepts empty content there): role=tool — a tool result
		// that may legitimately be empty — and assistant turns that carry
		// tool_calls (a pure tool-call turn, content empty by OpenAI
		// convention). Everything else with empty content is rejected.
		if m.Content == "" && m.Role != "tool" && len(m.ToolCalls) == 0 {
			return "message content is required"
		}
		// System messages get their own budget (ChatMaxSystemBytes, synced
		// with kaya's MAX_SYSTEM_BYTES). kaya's rendered system prompt is
		// ~21-23 KB and is truncated client-side to that budget.
		limit := model.ChatMaxMessageBytes
		if m.Role == "system" {
			limit = model.ChatMaxSystemBytes
		}
		if len(m.Content) > limit {
			return "message content too long"
		}
		total += len(m.Content)
		if total > model.ChatMaxTotalBytes {
			return "total message content too long"
		}
	}
	return ""
}

// validateChatTools enforces the abuse bounds on the optional tools array.
// Returns an empty string when valid, otherwise a human-readable reason.
// The tools themselves are opaque JSON — the server never parses them.
func validateChatTools(tools []json.RawMessage) string {
	if len(tools) == 0 {
		return ""
	}
	if len(tools) > model.ChatMaxTools {
		return "too many tools"
	}
	total := 0
	for _, t := range tools {
		// 元素必须是 JSON 对象(工具 schema):`null`/`123`/`"str"`/`[]` 等
		// 合法但非对象的值会透传上游 → 上游 400 → 客户端得到语义模糊的
		// 502。json.Valid + 首字符检查:ShouldBindJSON 已保证 body 合法,
		// json.Valid 对已解析元素恒 true,作为解析路径变化的深层防御;
		// 首字符 `{` 是实际生效的对象性检查。
		if !json.Valid(t) {
			return "invalid tool definition"
		}
		trimmed := bytes.TrimSpace(t)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return "invalid tool definition"
		}
		total += len(trimmed)
		if total > model.ChatMaxToolsBytes {
			return "tools too large"
		}
	}
	return ""
}

// writeChatError emits the standard {"code","data","message"} error shape.
func writeChatError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"code": status, "data": nil, "message": message})
}
