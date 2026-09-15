package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/relay"
	"github.com/yunhou/users/internal/service"
)

// RelayHandler 承载 relay 模块的 HTTP 入口。WS 升级逻辑见 ServeWS。
type RelayHandler struct {
	svc            *service.RelayService
	hub            *relay.Hub
	tickets        *service.RelayTicketService
	fails          *relay.HelloFailLimiter
	allowedOrigins []string
}

func NewRelayHandler(svc *service.RelayService) *RelayHandler {
	return &RelayHandler{svc: svc}
}

// SetHub 注入 WS 侧依赖(main.go 在 relay 启用时调用)。
// hello 失败限流器(5 次/min/IP,spec §8)由 handler 持有并传入 relay 包。
func (h *RelayHandler) SetHub(hub *relay.Hub, tickets *service.RelayTicketService, allowedOrigins []string) {
	h.hub = hub
	h.tickets = tickets
	h.fails = relay.NewHelloFailLimiter(5)
	h.allowedOrigins = allowedOrigins
}

// ServeWS 处理 GET /relay/ws:停机 503 → Origin 校验(防 CSWSH)→ 移交 relay 包。
func (h *RelayHandler) ServeWS(c *gin.Context) {
	if h.hub == nil || h.hub.ShutdownStarted() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "relay shutting down"})
		return
	}
	if origin := c.GetHeader("Origin"); origin != "" && !relay.OriginAllowed(origin, h.allowedOrigins) {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "origin not allowed"})
		return
	}
	// c.ClientIP() 按 TrustedProxies 可信链解析(忽略不可信代理带来的
	// 伪造 XFF);hello 失败限流与告警节流键以此为准,防 XFF 轮换绕过。
	relay.HandleWS(c.Writer, c.Request, h.hub, h.tickets, h.fails, c.ClientIP())
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
