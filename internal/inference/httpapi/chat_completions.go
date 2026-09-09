// chat_completions.go — POST /v1/chat/completions（Task 8，设计 §9.1）。
//
// 首期标准入口：流式及非流式，API Key 鉴权（auth chain 在 /v1 group 挂载
// 时固定）。原生 OpenAI 响应形状与 SSE 终止事件（[DONE]），不套管理
// envelope；统一内部错误码由本文件映射为协议原生错误形状。
//
// 请求字段按能力校验（设计: 不支持的工具/模态明确报错，不能静默丢字段
// 或拼成文本；不把旧 /chat 的 20 条消息限制硬套标准客户端）。

package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/model"
)

// v1ChatMaxBodyBytes caps the standard chat-completions body. Standard
// clients legitimately exceed the Kaya /chat limits (long tool schemas,
// many turns); the engine-level 1 MiB MaxBytesReader remains the outer
// guard, and this per-route cap matches it.
const v1ChatMaxBodyBytes = 1 << 20

// v1ChatMaxMessages / v1ChatMaxTools bound collection sizes for abuse
// control WITHOUT inheriting the Kaya /chat contract (20 messages / 16
// tools are the Kaya product limits, not the standard API's, 设计 §9.1).
const (
	v1ChatMaxMessages = 512
	v1ChatMaxTools    = 128
	v1ChatMaxModelLen = 128
)

// v1StreamWriteTimeout is the per-response write deadline for /v1 SSE
// streams, set via http.ResponseController — slightly above the longest
// deployment request timeout so the stream ends by cancellation, never by
// a mid-stream write kill (same pattern as the /chat handler).
const v1StreamWriteTimeout = 11 * time.Minute

// ChatCompletionsHandler serves POST /v1/chat/completions.
type ChatCompletionsHandler struct {
	Gateway *gateway.Service
}

// NewChatCompletionsHandler builds the handler.
func NewChatCompletionsHandler(gw *gateway.Service) *ChatCompletionsHandler {
	return &ChatCompletionsHandler{Gateway: gw}
}

// ---------------------------------------------------------------------------
// Request parsing & capability validation
// ---------------------------------------------------------------------------

// chatCompletionsRequest is the accepted OpenAI Chat Completions shape.
// Unknown fields are tolerated (standard OpenAI-server behavior); fields
// that name capabilities we cannot honor are rejected explicitly.
type chatCompletionsRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		// ContentParts detects the multimodal array shape (content:[{...}]).
		Name       string           `json:"name"`
		ToolCalls  []model.ToolCall `json:"tool_calls"`
		ToolCallID string           `json:"tool_call_id"`
	} `json:"messages"`
	Stream              bool              `json:"stream"`
	MaxTokens           *int64            `json:"max_tokens"`
	MaxCompletionTokens *int64            `json:"max_completion_tokens"`
	Tools               []json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	StreamOptions       json.RawMessage   `json:"stream_options"`
	ThinkingEnabled     *bool             `json:"thinking_enabled"`
	Thinking            json.RawMessage   `json:"thinking"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	Stop                json.RawMessage   `json:"stop"`
	PresencePenalty     *float64          `json:"presence_penalty"`
	FrequencyPenalty    *float64          `json:"frequency_penalty"`
	Seed                *int64            `json:"seed"`

	// Capability fields we do not serve; any non-null value rejects the
	// request explicitly rather than silently degrading it.
	Modalities     json.RawMessage `json:"modalities"`
	Audio          json.RawMessage `json:"audio"`
	N              *int64          `json:"n"`
	ResponseFormat json.RawMessage `json:"response_format"`
	Logprobs       *bool           `json:"logprobs"`
	TopLogprobs    *int64          `json:"top_logprobs"`
	Prediction     json.RawMessage `json:"prediction"`
}

// parseChatCompletions decodes and capability-validates the body into the
// internal request. All failures are invalid_input 400s in the native shape.
func parseChatCompletions(body []byte) (*providers.ChatRequest, error) {
	var wire chatCompletionsRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&wire); err != nil {
		return nil, domain.NewError(domain.CodeInvalidInput, "invalid request body")
	}

	// Capability gates — explicit rejection, never a silent drop.
	if present(wire.Modalities) {
		return nil, invalidField("modalities", "multimodal output is not supported by this gateway")
	}
	if present(wire.Audio) {
		return nil, invalidField("audio", "audio is not supported by this gateway")
	}
	if present(wire.Prediction) {
		return nil, invalidField("prediction", "predicted output is not supported")
	}
	if wire.N != nil && *wire.N != 1 {
		return nil, invalidField("n", "only n=1 is supported")
	}
	if wire.Logprobs != nil && *wire.Logprobs || wire.TopLogprobs != nil {
		return nil, invalidField("logprobs", "logprobs are not supported")
	}
	if present(wire.ResponseFormat) {
		var rf struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(wire.ResponseFormat, &rf); err != nil || (rf.Type != "" && rf.Type != "text") {
			return nil, invalidField("response_format", "only plain text responses are supported")
		}
	}

	if wire.Model == "" {
		return nil, invalidField("model", "model is required")
	}
	if len(wire.Model) > v1ChatMaxModelLen {
		return nil, invalidField("model", "model id too long")
	}
	if len(wire.Messages) == 0 {
		return nil, invalidField("messages", "messages is required")
	}
	if len(wire.Messages) > v1ChatMaxMessages {
		return nil, invalidField("messages", "too many messages")
	}

	messages := make([]model.ChatMessage, 0, len(wire.Messages))
	for i, m := range wire.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, invalidField("messages", "invalid message role")
		}
		var cm model.ChatMessage
		cm.Role = m.Role
		cm.ToolCalls = m.ToolCalls
		cm.ToolCallID = m.ToolCallID
		if len(m.Content) > 0 && string(m.Content) != "null" {
			if m.Content[0] == '[' {
				// 模态数组形态明确报错 — 不静默丢字段、不拼成文本。
				return nil, invalidField("messages",
					"content arrays (multimodal parts) are not supported; send plain string content")
			}
			if err := json.Unmarshal(m.Content, &cm.Content); err != nil {
				return nil, invalidField("messages", "message content must be a string")
			}
		}
		// content is required for text-bearing roles; tool results and
		// assistant tool-call turns may legitimately carry empty content.
		if cm.Content == "" && m.Role != "tool" && len(cm.ToolCalls) == 0 {
			return nil, invalidField("messages", "message content is required")
		}
		if m.Role == "tool" && cm.ToolCallID == "" {
			return nil, invalidField("messages", "tool messages require tool_call_id")
		}
		_ = i
		messages = append(messages, cm)
	}

	if len(wire.Tools) > v1ChatMaxTools {
		return nil, invalidField("tools", "too many tools")
	}
	toolBytes := 0
	for _, t := range wire.Tools {
		trimmed := bytes.TrimSpace(t)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, invalidField("tools", "invalid tool definition")
		}
		toolBytes += len(trimmed)
	}
	if toolBytes > 256<<10 {
		return nil, invalidField("tools", "tools too large")
	}

	req := &providers.ChatRequest{
		Model:    wire.Model,
		Messages: messages,
		Stream:   wire.Stream,
		Tools:    wire.Tools,
	}
	if wire.MaxTokens != nil || wire.MaxCompletionTokens != nil {
		cap := wire.MaxTokens
		if wire.MaxCompletionTokens != nil {
			cap = wire.MaxCompletionTokens
		}
		if *cap <= 0 {
			return nil, invalidField("max_tokens", "max_tokens must be > 0")
		}
		req.MaxTokens = cap
	}
	if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
		req.ToolChoice = wire.ToolChoice
	}
	// thinking: accept kaya's thinking_enabled bool and DeepSeek's
	// thinking object ({"type":"enabled"}).
	if wire.ThinkingEnabled != nil {
		req.ThinkingEnabled = wire.ThinkingEnabled
	} else if present(wire.Thinking) {
		var th struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(wire.Thinking, &th); err != nil {
			return nil, invalidField("thinking", "invalid thinking parameter")
		}
		on := th.Type == "enabled"
		req.ThinkingEnabled = &on
	}
	// Sampling passthrough allowlist (adapters map or reject per protocol).
	passthrough := map[string]any{}
	if wire.Temperature != nil {
		passthrough["temperature"] = *wire.Temperature
	}
	if wire.TopP != nil {
		passthrough["top_p"] = *wire.TopP
	}
	if wire.PresencePenalty != nil {
		passthrough["presence_penalty"] = *wire.PresencePenalty
	}
	if wire.FrequencyPenalty != nil {
		passthrough["frequency_penalty"] = *wire.FrequencyPenalty
	}
	if wire.Seed != nil {
		passthrough["seed"] = *wire.Seed
	}
	if present(wire.Stop) {
		var stop any
		if err := json.Unmarshal(wire.Stop, &stop); err != nil {
			return nil, invalidField("stop", "invalid stop parameter")
		}
		passthrough["stop"] = stop
	}
	if len(passthrough) > 0 {
		req.Passthrough = passthrough
	}
	return req, nil
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func invalidField(field, msg string) error {
	return domain.NewError(domain.CodeInvalidInput, field+": "+msg)
}

// ---------------------------------------------------------------------------
// Response writing
// ---------------------------------------------------------------------------

// Create handles POST /v1/chat/completions.
func (h *ChatCompletionsHandler) Create(c *gin.Context) {
	p := CallerPrincipalOf(c)
	if p == nil {
		v1Error(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"missing caller principal (route mounted without auth)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, v1ChatMaxBodyBytes))
	if err != nil {
		v1Error(c, http.StatusBadRequest, "invalid_request_error", "invalid_input", "request body too large or unreadable")
		return
	}
	req, err := parseChatCompletions(body)
	if err != nil {
		writeV1DomainError(c, err)
		return
	}

	outcome, err := h.Gateway.ChatCompletions(c.Request.Context(), p, CallerKeyOf(c), domain.ProtocolOpenAIChat, req)
	if err != nil {
		writeV1DomainError(c, err)
		return
	}

	if outcome.Stream == nil {
		c.Data(http.StatusOK, "application/json", outcome.Payload)
		return
	}
	h.relayStream(c, outcome)
}

// relayStream forwards the upstream SSE byte stream to the client. The
// stream ends by the protocol's [DONE]; an upstream break injects an
// in-stream error event WITHOUT a [DONE] (a client that stops parsing at
// [DONE] must never render a partial answer as complete).
func (h *ChatCompletionsHandler) relayStream(c *gin.Context, outcome *gateway.Outcome) {
	stream := outcome.Stream
	defer stream.Close()

	c.Status(http.StatusOK)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	// Ask any front proxy (nginx) not to buffer this stream.
	c.Header("X-Accel-Buffering", "no")

	// The server-wide WriteTimeout is an absolute per-request deadline that
	// would hard-cut a legitimately long stream; give this response its own
	// gateway-sized write deadline (see cmd/server).
	rc := http.NewResponseController(c.Writer)
	if err := rc.SetWriteDeadline(time.Now().Add(v1StreamWriteTimeout)); err != nil {
		log.Printf("v1/chat/completions: set write deadline: %v", err)
	}

	buf := make([]byte, 32<<10)
	end := gateway.EndCompleted
	for {
		n, readErr := stream.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				end = gateway.EndClientGone
				break
			}
			c.Writer.Flush()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if !stream.Terminal() {
					end = gateway.EndUpstreamBroke
				}
			} else if c.Request.Context().Err() != nil {
				end = gateway.EndClientGone
			} else {
				end = gateway.EndUpstreamBroke
			}
			break
		}
	}
	if end == gateway.EndUpstreamBroke {
		// A clean EOF without [DONE] would make the client render the
		// partial answer as complete — inject an in-stream error instead.
		_, _ = io.WriteString(c.Writer, "data: {\"error\":{\"message\":\"upstream stream interrupted\",\"type\":\"server_error\",\"code\":\"upstream_unavailable\"}}\n\n")
		c.Writer.Flush()
	}
	// Settlement runs on a detached context inside Finish: a client
	// disconnect stops the upstream connection but never loses the usage
	// already read (设计 §7.2).
	if err := stream.Finish(end); err != nil {
		log.Printf("v1/chat/completions: finish %s: %v", outcome.RequestID, err)
	}
}

// ---------------------------------------------------------------------------
// Error mapping (统一内部错误 → 协议原生形状, 设计 §9.1)
// ---------------------------------------------------------------------------

// writeV1DomainError maps the unified internal error codes onto the native
// /v1 error shape. Quota exhaustion answers 429 with the full window detail
// and a Retry-After ONLY when every blocking constraint has a computable
// recovery instant (未知恢复不能编造倒计时).
func writeV1DomainError(c *gin.Context, err error) {
	var qe *domain.QuotaExceededError
	if errors.As(err, &qe) {
		message, retryAfter := quotaExceededDetails(qe)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		v1Error(c, http.StatusTooManyRequests, "rate_limit_error", "quota_exceeded", message)
		return
	}

	switch domain.CodeOf(err) {
	case domain.CodeInvalidKey:
		v1Error(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid, revoked or expired API key")
	case domain.CodeNotFound:
		v1Error(c, http.StatusNotFound, "invalid_request_error", "model_not_found", safeMsg(err))
	case domain.CodeInvalidInput:
		v1Error(c, http.StatusBadRequest, "invalid_request_error", "invalid_input", safeMsg(err))
	case domain.CodeModelNotAllowed:
		v1Error(c, http.StatusForbidden, "permission_error", "model_not_allowed", safeMsg(err))
	case domain.CodeRateLimited, domain.CodeInsufficientCapacity:
		v1Error(c, http.StatusTooManyRequests, "rate_limit_error", string(domain.CodeOf(err)), safeMsg(err))
	case domain.CodeUnpricedCapability:
		v1Error(c, http.StatusForbidden, "permission_error", "unpriced_capability", safeMsg(err))
	case domain.CodeUpstreamUnavailable:
		v1Error(c, http.StatusServiceUnavailable, "server_error", "upstream_unavailable", safeMsg(err))
	case domain.CodeConflict:
		v1Error(c, http.StatusConflict, "invalid_request_error", "conflict", safeMsg(err))
	default:
		log.Printf("v1 internal error: %v", err)
		v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
	}
}

// quotaExceededDetails renders the shared quota message and the Retry-After
// seconds (0 = unknown recovery — 不能编造倒计时). All three protocol
// surfaces (chat/messages/responses) render the same 429 detail.
func quotaExceededDetails(qe *domain.QuotaExceededError) (message string, retryAfterSecs int) {
	message = "quota exceeded"
	kinds := ""
	allKnown := len(qe.BlockedBy) > 0
	var reset time.Time
	for i, b := range qe.BlockedBy {
		if i > 0 {
			kinds += ","
		}
		kinds += string(b.Kind)
		if b.ResetsAt == nil {
			allKnown = false
		} else if reset.IsZero() || b.ResetsAt.After(reset) {
			reset = *b.ResetsAt
		}
	}
	if kinds != "" {
		message = "quota exceeded: blocked by " + kinds
	}
	if qe.KeyBudgetExhausted {
		message += "; key budget exhausted"
	}
	if qe.DeficitMicros != nil {
		message += "; deficit_micros=" + strconv.FormatInt(int64(*qe.DeficitMicros), 10)
	}
	if allKnown && !reset.IsZero() {
		if secs := int(time.Until(reset).Seconds()) + 1; secs > 0 {
			retryAfterSecs = secs
		}
	}
	return message, retryAfterSecs
}

// safeMsg exposes the domain error's Message (never the cause chain, which
// can carry upstream URLs/bodies).
func safeMsg(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return "request failed"
}
