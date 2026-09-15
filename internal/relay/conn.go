package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"
)

// ticketVerifier 由 *service.RelayTicketService 满足;relay 包不 import
// service,避免 service → relay → service 循环。
type ticketVerifier interface {
	Verify(token string) (userID string, exp time.Time, err error)
}

// OriginAllowed 比对 Origin 的 scheme://host 与 host 两种形态的白名单条目。
// 白名单为空 → fail-closed(已决事项 4)。解析失败的 Origin 一律拒绝。
func OriginAllowed(origin string, allowed []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, a := range allowed {
		if a == u.Host || a == u.Scheme+"://"+u.Host {
			return true
		}
	}
	return false
}

// helloProtoError 标记 hello 阶段可回 closed protocol 的失败(坏 JSON /
// 版本 / 角色 / 超大帧);超时与传输错误直接关连接、不发帧(spec §7)。
type helloProtoError struct{ msg string }

func (e *helloProtoError) Error() string { return e.msg }

// wsConn 实现 Conn,包装一条 coder/websocket 连接。
type wsConn struct {
	hub *Hub
	ws  *websocket.Conn

	userID string
	role   Role
	id     string // device_id 或 client_id
	meta   DeviceInfo

	send          chan []byte // 出站缓冲,容量 = Options.OutboundBuffer
	closing       atomic.Bool
	closeOnce     sync.Once
	closedWritten chan struct{} // writer 写出 closed 帧后关闭
	closeDone     chan struct{} // finalize 完成(ws.Close 已调用)后关闭
	cancel        context.CancelFunc

	mu      sync.Mutex
	exp     time.Time     // 当前 ticket 过期时刻
	renewed chan struct{} // cap 1:renew 成功通知 runTimers 重置定时器

	originPass  bool        // Origin 头存在且已通过 handler 层白名单(只记是否通过)
	closeReason CloseReason // 仅 initiateClose 设置(mu 保护);空 = 非主动关闭

	framesIn    atomic.Int64
	framesOut   atomic.Int64
	connectedAt time.Time
}

func (c *wsConn) UserID() string { return c.userID }
func (c *wsConn) GetRole() Role  { return c.role }

func (c *wsConn) DeviceID() string {
	if c.role == RoleDevice {
		return c.id
	}
	return ""
}

func (c *wsConn) ClientID() string {
	if c.role == RoleClient {
		return c.id
	}
	return ""
}

func (c *wsConn) DeviceMeta() DeviceInfo { return c.meta }

// HandleWS 是 /relay/ws 的入口:hello 失败限流预检 → Upgrade → 等 hello →
// 注册 → 服务循环。Origin 已在 handler 层手工校验(已决事项 4:device 不带
// Origin 头,不能用 AcceptOptions.OriginPatterns)。clientIP 必须是调用方
// 按可信代理链解析后的来源(gin c.ClientIP())——不能在本层读原始
// X-Forwarded-For:nginx 的 $proxy_add_x_forwarded_for 保留客户端自带的
// XFF,攻击者轮换首段即可绕过限流(spec §8)。全部路径保证:返回前
// 连接一定被关闭或已随服务循环终结。
func HandleWS(w http.ResponseWriter, r *http.Request, hub *Hub, tickets ticketVerifier, fails *HelloFailLimiter, clientIP string) {
	opts := hub.Options()
	// hello 失败超限的 IP 在握手阶段直接 403,不升级(spec §8)。
	if fails != nil && !fails.Allow(clientIP) {
		http.Error(w, "too many hello failures", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin 已在 handler 层手工校验
	})
	if err != nil {
		return // Upgrade 失败,连接未建立
	}
	ws.SetReadLimit(int64(opts.MaxFrameBytes) + 1024) // 余量给信封外层

	c := &wsConn{
		hub:           hub,
		ws:            ws,
		send:          make(chan []byte, opts.OutboundBuffer),
		closedWritten: make(chan struct{}),
		closeDone:     make(chan struct{}),
		renewed:       make(chan struct{}, 1),
		connectedAt:   time.Now(),
		// 走到这里 Origin 已被 handler 层放行(或不存在);只记是否通过。
		originPass: r.Header.Get("Origin") != "",
	}
	c.run(clientIP, tickets, fails)
}

// run 的阶段划分对应 spec §7 状态机。ip 是可信链解析后的来源,用于
// hello 失败限流与告警节流键(与 HandleWS 预检同一桶)。
func (c *wsConn) run(ip string, tickets ticketVerifier, fails *HelloFailLimiter) {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	defer cancel()
	defer c.ws.Close(websocket.StatusNormalClosure, "bye")
	opts := c.hub.Options()

	// 阶段 1:等 hello(HelloTimeout 超时 → 直接关,不发帧,WS close 1008)
	hello, err := c.readHello(ctx, opts)
	if err != nil {
		// helloProtoError = 可回 closed 的形态错;其余视为超时/传输错。
		reason := "timeout"
		var pe *helloProtoError
		if errors.As(err, &pe) {
			reason = "protocol"
			c.sendDirect(ClosedFrame(ReasonProtocol))
		}
		c.hub.metrics.helloFailed(reason)
		c.hub.warn.Logf("hello_fail:"+shortHash(ip), "hello failed reason=%s ip_hash=%s", reason, shortHash(ip))
		fails.RecordFailure(ip)
		c.ws.Close(websocket.StatusPolicyViolation, "hello failed")
		return
	}

	// 阶段 2:校验 ticket + hello 形态
	userID, exp, verr := tickets.Verify(hello.Ticket)
	if verr != nil {
		c.hub.metrics.helloFailed("auth")
		c.hub.warn.Logf("hello_fail:"+shortHash(ip), "hello failed reason=auth ip_hash=%s", shortHash(ip))
		fails.RecordFailure(ip)
		c.sendDirect(ClosedFrame(ReasonAuth))
		c.ws.Close(websocket.StatusPolicyViolation, string(ReasonAuth))
		return
	}
	if !validHelloShape(hello, opts.MaxIDLen) {
		c.hub.metrics.helloFailed("protocol")
		c.hub.warn.Logf("hello_fail:"+shortHash(ip), "hello failed reason=protocol ip_hash=%s", shortHash(ip))
		c.sendDirect(ClosedFrame(ReasonProtocol))
		c.ws.Close(websocket.StatusPolicyViolation, string(ReasonProtocol))
		return
	}
	c.userID, c.exp = userID, exp
	c.role, c.id = hello.Role, helloID(hello)
	c.meta = DeviceInfo{
		DeviceID: hello.DeviceID, DeviceName: hello.DeviceName,
		AppVersion: hello.AppVersion, ConnectedAt: c.connectedAt.Unix(),
	}

	// 阶段 3:注册(踢旧连由 Hub 完成;限额/停机拒绝 → closed)
	if _, err := c.hub.Register(c); err != nil {
		reason := ReasonProtocol
		var re *RejectError
		if errors.As(err, &re) {
			reason = re.Reason
		}
		c.sendDirect(ClosedFrame(reason))
		c.ws.Close(websocket.StatusPolicyViolation, string(reason))
		return
	}
	defer c.hub.Unregister(c)

	// 阶段 4:hello_ok(client 带全量在线 device;device 也发,表为空)
	c.sendDirect(HelloOKFrame(c.hub.OnlineDevices(c.userID), time.Now().Unix()))
	logConnect(c)

	// 阶段 5:服务循环:writer goroutine + 续期定时器 goroutine + 读循环
	go c.writer(ctx)
	go c.runTimers(ctx)

	c.readLoop(ctx, tickets)
	// readLoop 返回 = 连接终结。若主动关闭已触发,等 finalize 把
	// closed 帧 flush 并关闭底层连接后再返回(closed 帧能发尽发)。
	if c.closing.Load() {
		<-c.closeDone
	}
	// 关闭日志:主动关闭记 CloseReason;对端断开/传输失败(未走
	// initiateClose)记 "eof"(不编造 spec 之外的 reason 枚举)。
	reason := c.finalReason()
	if reason == "" {
		reason = "eof"
	}
	logClose(c, reason)
}

// readHello 在 HelloTimeout 内等首帧并做信封级校验。
// 注意:不能用 ctx 超时的 Read 实现 hello 超时——coder/websocket 在读 ctx
// 取消时会直接硬关连接(setupReadTimeout → c.close),之后的 Close(1008)
// 发不出去。改为 AfterFunc 主动 Close(1008):先写 close 帧再解除 Read 阻塞,
// 客户端可观测到 spec 要求的 WS close 1008(且不发 closed 帧)。
func (c *wsConn) readHello(ctx context.Context, opts Options) (*HelloFrame, error) {
	timer := time.AfterFunc(opts.HelloTimeout, func() {
		c.ws.Close(websocket.StatusPolicyViolation, "hello timeout")
	})
	defer timer.Stop()
	_, frame, err := c.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if len(frame) > opts.MaxFrameBytes {
		return nil, &helloProtoError{"hello frame too large"}
	}
	var h HelloFrame
	if err := json.Unmarshal(frame, &h); err != nil {
		return nil, &helloProtoError{"hello is not valid json"}
	}
	if h.Type != "hello" || h.V != ProtocolVersion ||
		(h.Role != RoleDevice && h.Role != RoleClient) {
		return nil, &helloProtoError{"invalid hello envelope"}
	}
	return &h, nil
}

// validHelloShape:device 必填 device_id/device_name/app_version 非空且
// ≤ maxIDLen;client 必填 client_id 非空 ≤ maxIDLen。
func validHelloShape(h *HelloFrame, maxIDLen int) bool {
	switch h.Role {
	case RoleDevice:
		return h.DeviceID != "" && len(h.DeviceID) <= maxIDLen &&
			h.DeviceName != "" && len(h.DeviceName) <= maxIDLen &&
			h.AppVersion != "" && len(h.AppVersion) <= maxIDLen
	case RoleClient:
		return h.ClientID != "" && len(h.ClientID) <= maxIDLen
	}
	return false
}

// helloID 按 role 返回 device_id 或 client_id。
func helloID(h *HelloFrame) string {
	if h.Role == RoleDevice {
		return h.DeviceID
	}
	return h.ClientID
}

// readLoop 读循环:无读超时——活性由 writer 的 ping/pong 探测保证。
// 返回即连接终结;主动关闭路径都已先触发 initiateClose。
func (c *wsConn) readLoop(ctx context.Context, tickets ticketVerifier) {
	opts := c.hub.Options()
	burst := int(opts.MaxFramesPerSec)
	if burst < 1 {
		burst = 1
	}
	limiter := rate.NewLimiter(rate.Limit(opts.MaxFramesPerSec), burst)
	for {
		_, frame, err := c.ws.Read(ctx)
		if err != nil {
			// 对端关闭(任意 CloseStatus)/ 传输错 / ctx 取消:正常断开,
			// Unregister 由 run 的 defer 触发 presence。
			return
		}
		c.framesIn.Add(1)
		if !limiter.Allow() || len(frame) > opts.MaxFrameBytes {
			c.hub.warn.Logf("rate_limit:"+UserHash(c.userID)+"/"+c.id,
				"frame limit hit user_hash=%s role=%s id=%s", UserHash(c.userID), c.role, c.id)
			c.initiateClose(ReasonProtocol)
			return
		}
		v, typ, err := PeekType(frame)
		if err != nil || v != ProtocolVersion {
			c.initiateClose(ReasonProtocol)
			return
		}
		switch typ {
		case "app":
			var af AppFrame
			if err := json.Unmarshal(frame, &af); err != nil {
				c.initiateClose(ReasonProtocol)
				return
			}
			// client 缺 target_device_id → protocol;device 带 target 字段
			// 忽略(RouteApp 对 device 角色本就不读 target);payload 为空
			// 允许——是合法 JSON 即可,不解析内容。
			if c.role == RoleClient && af.TargetDeviceID == "" {
				c.initiateClose(ReasonProtocol)
				return
			}
			c.hub.RouteApp(c, af.TargetDeviceID, af.Payload)
		case "renew":
			var rf RenewFrame
			if err := json.Unmarshal(frame, &rf); err != nil {
				c.initiateClose(ReasonProtocol)
				return
			}
			uid, exp, err := tickets.Verify(rf.Ticket)
			// 安全不变式:续期 ticket 的 sub 必须等于连接 user_id。
			if err != nil || uid != c.userID {
				c.hub.metrics.renewFailed()
				c.hub.warn.Logf("renew_fail:"+UserHash(c.userID)+"/"+c.id,
					"renew failed user_hash=%s role=%s id=%s", UserHash(c.userID), c.role, c.id)
				c.initiateClose(ReasonAuth)
				return
			}
			c.mu.Lock()
			c.exp = exp
			c.mu.Unlock()
			select {
			case c.renewed <- struct{}{}:
			default:
			}
		default: // 重复 hello 与未知 type
			c.initiateClose(ReasonProtocol)
			return
		}
	}
}

// writer 出站循环 + ping keepalive。coder/websocket 的 Ping 阻塞等 pong
// (pong 由 readLoop 的 Read 读出),连续失败即死连接,等价于 spec 的
// "读侧 90s 无帧判死";实际探测周期 ≤ PingInterval + ReadIdleTimeout/3。
func (c *wsConn) writer(ctx context.Context) {
	opts := c.hub.Options()
	ping := time.NewTicker(opts.PingInterval)
	defer ping.Stop()
	for {
		select {
		case b := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.abort()
				return
			}
			c.framesOut.Add(1)
			// 约定:closed 帧是出站最后一帧,写出后收尾交给 finalize。
			if _, typ, _ := PeekType(b); typ == "closed" {
				close(c.closedWritten)
				return
			}
		case <-ping.C:
			if c.closing.Load() {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, opts.ReadIdleTimeout/3)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.initiateClose(ReasonIdleTimeout)
			}
		case <-ctx.Done():
			return
		}
	}
}

// runTimers 续期定时器:exp-TicketExpiringLead 发一次 ticket_expiring;
// exp+RenewGrace 仍未 renew → closed auth。renew 成功后经 renewed 通道
// 按新 exp 重建两个 timer。
func (c *wsConn) runTimers(ctx context.Context) {
	opts := c.hub.Options()
	c.mu.Lock()
	exp := c.exp
	c.mu.Unlock()
	expiring := time.NewTimer(time.Until(exp.Add(-opts.TicketExpiringLead)))
	deadline := time.NewTimer(time.Until(exp.Add(opts.RenewGrace)))
	defer expiring.Stop()
	defer deadline.Stop()
	for {
		select {
		case <-expiring.C:
			c.Enqueue(TicketExpiringFrame(opts.TicketRetryAfterMS))
		case <-deadline.C:
			c.initiateClose(ReasonAuth)
			return
		case <-c.renewed:
			c.mu.Lock()
			exp = c.exp
			c.mu.Unlock()
			resetTimer(expiring, time.Until(exp.Add(-opts.TicketExpiringLead)))
			resetTimer(deadline, time.Until(exp.Add(opts.RenewGrace)))
		case <-ctx.Done():
			return
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// Enqueue 实现 Conn 接口,必须非阻塞:出站缓冲满 → 该连接 slow_consumer
// 关闭,不影响同 room 其他连接(spec §8)。Hub 发来的 closed 帧(踢连 /
// 停机)在此转入统一关闭路径。
func (c *wsConn) Enqueue(b []byte) {
	if c.closing.Load() {
		return
	}
	if _, typ, _ := PeekType(b); typ == "closed" {
		var p struct {
			Reason CloseReason `json:"reason"`
		}
		_ = json.Unmarshal(b, &p)
		if p.Reason == "" {
			p.Reason = ReasonProtocol
		}
		c.initiateClose(p.Reason)
		return
	}
	select {
	case c.send <- b:
	default:
		c.hub.metrics.slowConsumerInc()
		c.hub.warn.Logf("slow_consumer:"+UserHash(c.userID)+"/"+c.id,
			"slow consumer close user_hash=%s role=%s id=%s", UserHash(c.userID), c.role, c.id)
		c.initiateClose(ReasonSlowConsumer)
	}
}

// initiateClose 统一关闭出口(踢连/停机/协议错/慢消费者/idle/auth):
// best-effort 把 closed 帧压入出站缓冲(满则丢弃最早一帧腾位),随后由
// finalize 等 writer flush(或 2s 超时)后关闭底层连接并取消 ctx。
func (c *wsConn) initiateClose(reason CloseReason) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closeReason = reason
		c.mu.Unlock()
		c.closing.Store(true)
		frame := ClosedFrame(reason)
		select {
		case c.send <- frame:
		default:
			select {
			case <-c.send:
			default:
			}
			select {
			case c.send <- frame:
			default:
			}
		}
		go c.finalize(true, reason)
	})
}

// finalReason 返回 initiateClose 记录的关闭原因;空 = 未走主动关闭路径。
func (c *wsConn) finalReason() CloseReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeReason
}

// ConnectedSince 供 Hub 在 Unregister/踢连时观测连接存活时长。
func (c *wsConn) ConnectedSince() time.Time { return c.connectedAt }

// abort 传输层失败的硬关闭(不再尝试发 closed 帧)。
func (c *wsConn) abort() {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		go c.finalize(false, "")
	})
}

func (c *wsConn) finalize(waitFlush bool, reason CloseReason) {
	if waitFlush {
		select {
		case <-c.closedWritten:
		case <-time.After(2 * time.Second):
		}
	}
	if c.cancel != nil {
		c.cancel()
	}
	code := websocket.StatusPolicyViolation
	if reason == "" {
		code = websocket.StatusInternalError
	}
	_ = c.ws.Close(code, string(reason))
	close(c.closeDone)
}

// sendDirect 是 writer 启动前的同步写(hello 阶段与 hello_ok 用),带 2s ctx。
func (c *wsConn) sendDirect(b []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, b)
}
