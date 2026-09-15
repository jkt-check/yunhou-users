package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// RelayHandler 承载 relay 模块的 HTTP 入口。WS 升级逻辑见 serveWS(Task 5)。
type RelayHandler struct {
	svc *service.RelayService
}

func NewRelayHandler(svc *service.RelayService) *RelayHandler {
	return &RelayHandler{svc: svc}
}

// IssueTicket 处理 POST /relay/ticket:access_token(经 JWTAuth 中间件)+
// entitlement → 签发 300s relay ticket。
func (h *RelayHandler) IssueTicket(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	appID := c.GetString(middleware.ContextAppID)

	if err := h.svc.CheckAccess(c.Request.Context(), userID, appID); err != nil {
		if errors.Is(err, service.ErrRelayNoAccess) {
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "data": nil, "message": service.ErrRelayNoAccess.Error()})
			return
		}
		// 固定文案,不泄露内部错误(与 /chat 先例一致)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "data": nil, "message": "internal error"})
		return
	}

	ticket, expiresIn, err := h.svc.IssueTicket(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "data": nil, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"ticket":     ticket,
		"expires_in": expiresIn,
		"ws_url":     relayWSURL(c),
	}, "message": "ok"})
}

// relayWSURL 由请求推导 WS 地址(已决事项 7)。
func relayWSURL(c *gin.Context) string {
	scheme := "ws"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "wss"
	}
	return scheme + "://" + c.Request.Host + "/relay/ws"
}
