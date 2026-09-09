// messages.go — POST /v1/messages（Task 13，Anthropic Messages 客户端面）。
//
// 原生 Anthropic 形状：请求体、错误（{"type":"error","error":{...}}）与
// SSE 事件序列（message_start/content_block_*/message_delta/message_stop）
// 都是协议原生，不套管理 envelope，也不是 chat 面换路径——翻译规则与
// 能力子集在 providers/messages_client.go（能力矩阵见集成指南/OpenAPI）。
//
// 闸门与 Chat 完全一致：principal 解析（/v1 group 的 APIKeyAuth）→ 预占 →
// 转发 → 结算（设计 §7.2）；本文件只做协议翻译与原生错误映射。

package httpapi

import (
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
)

// MessagesHandler serves POST /v1/messages.
type MessagesHandler struct {
	Gateway *gateway.Service
}

// NewMessagesHandler builds the handler.
func NewMessagesHandler(gw *gateway.Service) *MessagesHandler {
	return &MessagesHandler{Gateway: gw}
}

// Create handles POST /v1/messages.
func (h *MessagesHandler) Create(c *gin.Context) {
	p := CallerPrincipalOf(c)
	if p == nil {
		anthropicError(c, http.StatusUnauthorized, "authentication_error",
			"missing caller principal (route mounted without auth)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, v1ChatMaxBodyBytes))
	if err != nil {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error",
			"request body too large or unreadable")
		return
	}
	req, err := providers.ParseAnthropicMessagesRequest(body)
	if err != nil {
		writeAnthropicDomainError(c, err)
		return
	}

	outcome, err := h.Gateway.ChatCompletions(c.Request.Context(), p, CallerKeyOf(c),
		domain.ProtocolAnthropicMessage, req)
	if err != nil {
		writeAnthropicDomainError(c, err)
		return
	}

	if outcome.Stream == nil {
		payload, err := providers.AnthropicMessageFromCompletion(
			outcome.Payload, outcome.ModelID, outcome.Inclusion.CacheReadInInput)
		if err != nil {
			log.Printf("v1/messages: translate completion %s: %v", outcome.RequestID, err)
			anthropicError(c, http.StatusInternalServerError, "api_error", "internal error")
			return
		}
		c.Data(http.StatusOK, "application/json", payload)
		return
	}
	h.relayStream(c, outcome)
}

// relayStream relays the translated Anthropic SSE stream. Termination:
// message_stop is the only clean end; an upstream break has ALREADY been
// rendered by the translator as a native `event: error` frame — the handler
// injects nothing more (与 chat 面同一约定，绝不伪造 message_stop).
func (h *MessagesHandler) relayStream(c *gin.Context, outcome *gateway.Outcome) {
	stream := outcome.Stream
	defer stream.Close()

	// The translator owns the wire now; it must not finalize the stream on
	// close (the handler classifies the end itself). noClose shields
	// StreamBody.Close's client-gone finalization until we Finish explicitly.
	src := noCloseBody{stream}
	translated := providers.TranslateOpenAIToAnthropicStream(
		src, providers.NewPublicMessageID(), outcome.ModelID, outcome.Inclusion.CacheReadInInput)
	defer translated.Close()

	c.Status(http.StatusOK)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")

	rc := http.NewResponseController(c.Writer)
	if err := rc.SetWriteDeadline(time.Now().Add(v1StreamWriteTimeout)); err != nil {
		log.Printf("v1/messages: set write deadline: %v", err)
	}

	buf := make([]byte, 32<<10)
	end := gateway.EndCompleted
	for {
		n, readErr := translated.Read(buf)
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
	// Settlement runs on a detached context inside Finish (设计 §7.2).
	if err := stream.Finish(end); err != nil {
		log.Printf("v1/messages: finish %s: %v", outcome.RequestID, err)
	}
}

// noCloseBody shields a *gateway.StreamBody from the translator's Close: the
// handler (not the translator) owns Finish classification.
type noCloseBody struct{ s *gateway.StreamBody }

func (n noCloseBody) Read(p []byte) (int, error) { return n.s.Read(p) }
func (n noCloseBody) Close() error               { return nil }

// anthropicError writes the Anthropic-native error shape.
func anthropicError(c *gin.Context, status int, typ, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"type":  "error",
		"error": gin.H{"type": typ, "message": message},
	})
}

// writeAnthropicDomainError maps the unified internal error codes onto the
// Anthropic error taxonomy (设计 §9.1: 统一内部错误 → 协议适配映射).
func writeAnthropicDomainError(c *gin.Context, err error) {
	var qe *domain.QuotaExceededError
	if errors.As(err, &qe) {
		message, retryAfter := quotaExceededDetails(qe)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		anthropicError(c, http.StatusTooManyRequests, "rate_limit_error", message)
		return
	}

	switch domain.CodeOf(err) {
	case domain.CodeInvalidKey:
		anthropicError(c, http.StatusUnauthorized, "authentication_error",
			"invalid, revoked or expired API key")
	case domain.CodeNotFound:
		anthropicError(c, http.StatusNotFound, "not_found_error", safeMsg(err))
	case domain.CodeInvalidInput:
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", safeMsg(err))
	case domain.CodeModelNotAllowed, domain.CodeUnpricedCapability:
		anthropicError(c, http.StatusForbidden, "permission_error", safeMsg(err))
	case domain.CodeRateLimited, domain.CodeInsufficientCapacity:
		anthropicError(c, http.StatusTooManyRequests, "rate_limit_error", safeMsg(err))
	case domain.CodeUpstreamUnavailable:
		// 529/overloaded 是 Anthropic 语义里最接近"上游暂不可用"的类别,
		// 也是 SDK/客户端有退避重试行为的类别.
		anthropicError(c, 529, "overloaded_error", safeMsg(err))
	default:
		log.Printf("v1/messages internal error: %v", err)
		anthropicError(c, http.StatusInternalServerError, "api_error", "internal error")
	}
}
