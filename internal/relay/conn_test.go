package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// ---- 测试基建 ----

// stubTickets 是 ticketVerifier 的本地实现(ruling:relay 包测试不 import service)。
type stubTickets struct {
	mu     sync.Mutex
	next   int
	tokens map[string]stubToken
}

type stubToken struct {
	userID string
	exp    time.Time
}

func newStubTickets() *stubTickets { return &stubTickets{tokens: make(map[string]stubToken)} }

func (s *stubTickets) issue(userID string, ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	tok := fmt.Sprintf("tok-%d", s.next)
	s.tokens[tok] = stubToken{userID: userID, exp: time.Now().Add(ttl)}
	return tok
}

func (s *stubTickets) Verify(token string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[token]
	if !ok || time.Now().After(t.exp) {
		return "", time.Time{}, errors.New("invalid ticket")
	}
	return t.userID, t.exp, nil
}

// testOptions 注入短时序常量。注意:PingInterval/ReadIdleTimeout 刻意保持
// 较长——coder/websocket 的 client 只在 Read 期间回应 ping,空闲的测试
// client 无法自动 pong,短 ping 周期会把正常连接误判为 idle_timeout。
// 时序敏感断言一律从这些注入常量推导,不写死墙钟阈值。
func testOptions() Options {
	o := DefaultOptions()
	o.HelloTimeout = 300 * time.Millisecond
	o.PingInterval = 10 * time.Second
	o.ReadIdleTimeout = 30 * time.Second
	o.TicketExpiringLead = 500 * time.Millisecond
	o.RenewGrace = 500 * time.Millisecond
	return o
}

// wsTestEnv 起 httptest.Server,入口与 handler.ServeWS 同构:
// 停机 503 → Origin 白名单(调用同一个 relay.OriginAllowed)→ 移交 HandleWS。
// clientIP 固定为 testClientIP,等价于 gin 按可信链解析后的结果;原始
// X-Forwarded-For 不再参与限流键,伪造 XFF 分摊限流桶的场景由
// TestWSHelloFailSpoofedXFF 守护。
const testClientIP = "203.0.113.1"
type wsTestEnv struct {
	server  *httptest.Server
	hub     *Hub
	tickets *stubTickets
	fails   *HelloFailLimiter
}

func newWSTestEnv(t *testing.T, opts Options, failLimit int, allowedOrigins []string) *wsTestEnv {
	t.Helper()
	e := &wsTestEnv{
		hub:     NewHub(opts, nil),
		tickets: newStubTickets(),
		fails:   NewHelloFailLimiter(failLimit),
	}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.hub.ShutdownStarted() {
			http.Error(w, "relay shutting down", http.StatusServiceUnavailable)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !OriginAllowed(origin, allowedOrigins) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		HandleWS(w, r, e.hub, e.tickets, e.fails, testClientIP)
	}))
	t.Cleanup(func() {
		e.server.Close()
		e.fails.Stop()
	})
	return e
}

func (e *wsTestEnv) dial(t *testing.T, origin string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return e.dialWithHeaders(t, h)
}

func (e *wsTestEnv) dialWithHeaders(t *testing.T, h http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	dialOpts := &websocket.DialOptions{HTTPHeader: h}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.server.URL, "http")+"/relay/ws", dialOpts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// client 默认 read limit 仅 32KB,放开以便接收大 payload 帧。
	c.SetReadLimit(4 << 20)
	return c
}

func (e *wsTestEnv) dialExpectStatus(t *testing.T, origin string, wantStatus int) {
	t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	e.dialExpectStatusWithHeaders(t, h, wantStatus)
}

func (e *wsTestEnv) dialExpectStatusWithHeaders(t *testing.T, h http.Header, wantStatus int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	dialOpts := &websocket.DialOptions{HTTPHeader: h}
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.server.URL, "http")+"/relay/ws", dialOpts)
	if err == nil {
		c.CloseNow()
		t.Fatalf("expected handshake status %d, dial succeeded", wantStatus)
	}
	if resp == nil || resp.StatusCode != wantStatus {
		t.Fatalf("handshake status = %v, want %d (err=%v)", resp, wantStatus, err)
	}
}

func writeFrame(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// expectClosedReason 读到 type=closed 且 reason 匹配的帧为止(跳过中间帧);
// 连接先断而没有 closed 帧则失败。注意 coder/websocket 在读 ctx 超时时会
// 硬关连接,因此每次 Read 都用"剩余总时长"作为 ctx,不做短轮询重试。
func expectClosedReason(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("expected closed frame with reason %q, conn closed without it (err=%v)", want, err)
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && m["type"] == "closed" {
			if m["reason"] != want {
				t.Fatalf("closed reason = %v, want %q", m["reason"], want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for closed %q", want)
		}
	}
}

// expectConnClosed 断言连接最终被服务端关闭(closed 帧能发尽发,不要求一定收到)。
func expectConnClosed(t *testing.T, c *websocket.Conn, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, _, err := c.Read(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("conn still open after %v", within)
			}
			return
		}
	}
}

// expectNoFrame 断言 d 内没有任何帧到达。
func expectNoFrame(t *testing.T, c *websocket.Conn, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err == nil {
		t.Fatalf("expected no frame, got %s", b)
	}
}

func helloDevice(t *testing.T, c *websocket.Conn, ticket, deviceID string) map[string]any {
	t.Helper()
	writeFrame(t, c, map[string]any{
		"v": 1, "type": "hello", "ticket": ticket, "role": "device",
		"device_id": deviceID, "device_name": "Test Mac", "app_version": "2.4.0",
	})
	m := readFrame(t, c)
	if m["type"] != "hello_ok" {
		t.Fatalf("expected hello_ok, got %v", m)
	}
	return m
}

func helloClient(t *testing.T, c *websocket.Conn, ticket, clientID string) map[string]any {
	t.Helper()
	writeFrame(t, c, map[string]any{
		"v": 1, "type": "hello", "ticket": ticket, "role": "client", "client_id": clientID,
	})
	m := readFrame(t, c)
	if m["type"] != "hello_ok" {
		t.Fatalf("expected hello_ok, got %v", m)
	}
	return m
}

// ---- 测试用例(spec §10.1)----

func TestWSHelloTimeout(t *testing.T) {
	opts := testOptions()
	env := newWSTestEnv(t, opts, 5, nil)
	c := env.dial(t, "")
	defer c.CloseNow()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 超时不发 closed 帧:第一次 Read 必须直接返回错误,close status 1008。
	_, b, err := c.Read(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected read error after hello timeout, got frame %s", b)
	}
	if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want %v (1008)", code, websocket.StatusPolicyViolation)
	}
	if elapsed < opts.HelloTimeout {
		t.Fatalf("closed after %v, before HelloTimeout %v", elapsed, opts.HelloTimeout)
	}
	if elapsed > 10*opts.HelloTimeout {
		t.Fatalf("closed after %v, HelloTimeout is %v", elapsed, opts.HelloTimeout)
	}
}

func TestWSHelloBadTicket(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, nil)
	c := env.dial(t, "")
	defer c.CloseNow()
	writeFrame(t, c, map[string]any{
		"v": 1, "type": "hello", "ticket": "forged", "role": "device",
		"device_id": "d1", "device_name": "Mac", "app_version": "2.4.0",
	})
	expectClosedReason(t, c, "auth")
	expectConnClosed(t, c, 2*time.Second)
}

func TestWSHelloBadJSONAndBadVersion(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, nil)

	t.Run("bad json", func(t *testing.T) {
		c := env.dial(t, "")
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
			t.Fatalf("write: %v", err)
		}
		expectClosedReason(t, c, "protocol")
	})

	t.Run("bad version", func(t *testing.T) {
		c := env.dial(t, "")
		defer c.CloseNow()
		ticket := env.tickets.issue("u1", 10*time.Second)
		writeFrame(t, c, map[string]any{
			"v": 2, "type": "hello", "ticket": ticket, "role": "device",
			"device_id": "d1", "device_name": "Mac", "app_version": "2.4.0",
		})
		expectClosedReason(t, c, "protocol")
	})

	t.Run("device missing device_id", func(t *testing.T) {
		c := env.dial(t, "")
		defer c.CloseNow()
		ticket := env.tickets.issue("u1", 10*time.Second)
		writeFrame(t, c, map[string]any{
			"v": 1, "type": "hello", "ticket": ticket, "role": "device",
			"device_name": "Mac", "app_version": "2.4.0",
		})
		expectClosedReason(t, c, "protocol")
	})
}

func TestWSDeviceClientRouting(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, nil)
	ticketA := env.tickets.issue("userA", 10*time.Second)

	dev := env.dial(t, "")
	defer dev.CloseNow()
	helloDevice(t, dev, ticketA, "dev1")
	cl1 := env.dial(t, "")
	defer cl1.CloseNow()
	helloClient(t, cl1, ticketA, "c1")
	cl2 := env.dial(t, "")
	defer cl2.CloseNow()
	helloClient(t, cl2, ticketA, "c2")

	// device → 广播给全部 client,payload 逐字节一致
	payload := map[string]any{"op": "click", "x": float64(1)}
	writeFrame(t, dev, map[string]any{"v": 1, "type": "app", "payload": payload})
	wantPayload, _ := json.Marshal(payload)
	for i, cl := range []*websocket.Conn{cl1, cl2} {
		m := readFrame(t, cl)
		if m["type"] != "app" || m["from_device_id"] != "dev1" {
			t.Fatalf("client %d got %v, want app from dev1", i, m)
		}
		gotPayload, _ := json.Marshal(m["payload"])
		if string(gotPayload) != string(wantPayload) {
			t.Fatalf("client %d payload = %s, want %s", i, gotPayload, wantPayload)
		}
	}

	// client → 指定 device
	writeFrame(t, cl1, map[string]any{"v": 1, "type": "app", "target_device_id": "dev1", "payload": payload})
	m := readFrame(t, dev)
	if m["type"] != "app" || m["from_client_id"] != "c1" {
		t.Fatalf("device got %v, want app from c1", m)
	}

	// client → 不存在的 device:仅发送方收 undeliverable
	writeFrame(t, cl2, map[string]any{"v": 1, "type": "app", "target_device_id": "ghost", "payload": payload})
	m = readFrame(t, cl2)
	if m["type"] != "undeliverable" || m["target_device_id"] != "ghost" {
		t.Fatalf("got %v, want undeliverable ghost", m)
	}
	expectNoFrame(t, cl1, 300*time.Millisecond)

	// 跨 room:B 用户 client 指定 A 用户 device_id → undeliverable,不泄露存在性
	ticketB := env.tickets.issue("userB", 10*time.Second)
	clB := env.dial(t, "")
	defer clB.CloseNow()
	helloClient(t, clB, ticketB, "cB")
	writeFrame(t, clB, map[string]any{"v": 1, "type": "app", "target_device_id": "dev1", "payload": payload})
	m = readFrame(t, clB)
	if m["type"] != "undeliverable" || m["target_device_id"] != "dev1" {
		t.Fatalf("cross-room: got %v, want undeliverable dev1", m)
	}
	expectNoFrame(t, dev, 300*time.Millisecond)
}

func TestWSPresence(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, nil)
	ticket := env.tickets.issue("u1", 10*time.Second)

	cl1 := env.dial(t, "")
	defer cl1.CloseNow()
	helloClient(t, cl1, ticket, "c1")

	dev1 := env.dial(t, "")
	helloDevice(t, dev1, ticket, "d1")

	// device 上线 → presence online=true 带 meta
	m := readFrame(t, cl1)
	if m["type"] != "presence" || m["device_id"] != "d1" || m["online"] != true {
		t.Fatalf("got %v, want presence d1 online", m)
	}
	meta, _ := m["meta"].(map[string]any)
	if meta["device_name"] != "Test Mac" || meta["app_version"] != "2.4.0" {
		t.Fatalf("presence meta = %v", meta)
	}

	// client 中途加入 → hello_ok.room_devices 含全量在线 device
	cl2 := env.dial(t, "")
	defer cl2.CloseNow()
	ok := helloClient(t, cl2, ticket, "c2")
	devs, _ := ok["room_devices"].([]any)
	if len(devs) != 1 || devs[0].(map[string]any)["device_id"] != "d1" {
		t.Fatalf("hello_ok.room_devices = %v, want [d1]", devs)
	}

	// device 正常关闭 → presence online=false
	dev1.Close(websocket.StatusNormalClosure, "bye")
	for _, cl := range []*websocket.Conn{cl1, cl2} {
		m = readFrame(t, cl)
		if m["type"] != "presence" || m["device_id"] != "d1" || m["online"] != false {
			t.Fatalf("got %v, want presence d1 offline", m)
		}
	}

	// device 杀连接 → client 在 ReadIdleTimeout 推导的上限内收到 online=false
	dev2 := env.dial(t, "")
	helloDevice(t, dev2, ticket, "d2")
	m = readFrame(t, cl1) // presence d2 online
	if m["type"] != "presence" || m["online"] != true {
		t.Fatalf("got %v, want presence d2 online", m)
	}
	dev2.CloseNow()
	m = readFrame(t, cl1) // readFrame 的 3s 上限 ≪ ReadIdleTimeout(30s)
	if m["type"] != "presence" || m["device_id"] != "d2" || m["online"] != false {
		t.Fatalf("got %v, want presence d2 offline", m)
	}
}

func TestWSTicketExpiringAndRenew(t *testing.T) {
	opts := testOptions()
	env := newWSTestEnv(t, opts, 5, nil)
	ttl := 2 * time.Second

	t.Run("expiring then successful renew", func(t *testing.T) {
		ticket := env.tickets.issue("u-renew", ttl)
		dev := env.dial(t, "")
		defer dev.CloseNow()
		start := time.Now()
		helloDevice(t, dev, ticket, "d-renew")

		// 约 ttl - TicketExpiringLead 时收到 ticket_expiring
		m := readFrame(t, dev)
		elapsed := time.Since(start)
		if m["type"] != "ticket_expiring" || m["retry_after_ms"] != float64(30000) {
			t.Fatalf("got %v, want ticket_expiring retry_after_ms=30000", m)
		}
		if elapsed < ttl-opts.TicketExpiringLead-250*time.Millisecond || elapsed > ttl+time.Second {
			t.Fatalf("ticket_expiring at %v, want ≈ %v", elapsed, ttl-opts.TicketExpiringLead)
		}

		// 合法 renew → 连接存活超过原 exp+grace。证明方式:按新 exp 重排的
		// ticket_expiring(exp-lead ≈ renew 时刻 + ttl-lead)在原 exp+grace
		// 之后才会发出,收到它即证明连接当时仍存活(client Ping 不可用于
		// 空闲断言:coder/websocket 只在 Read 期间回应 ping)。
		writeFrame(t, dev, map[string]any{"v": 1, "type": "renew", "ticket": env.tickets.issue("u-renew", ttl)})
		time.Sleep(time.Until(start.Add(ttl + opts.RenewGrace + 500*time.Millisecond)))
		m = readFrame(t, dev)
		if m["type"] != "ticket_expiring" {
			t.Fatalf("after renew + original exp+grace got %v, want second ticket_expiring", m)
		}
	})

	t.Run("forged renew", func(t *testing.T) {
		ticket := env.tickets.issue("u-forge", ttl)
		dev := env.dial(t, "")
		defer dev.CloseNow()
		helloDevice(t, dev, ticket, "d-forge")
		readFrame(t, dev) // ticket_expiring
		writeFrame(t, dev, map[string]any{"v": 1, "type": "renew", "ticket": "forged"})
		expectClosedReason(t, dev, "auth")
	})

	t.Run("renew with other user's ticket", func(t *testing.T) {
		ticket := env.tickets.issue("u-victim", ttl)
		dev := env.dial(t, "")
		defer dev.CloseNow()
		helloDevice(t, dev, ticket, "d-victim")
		readFrame(t, dev) // ticket_expiring
		// 续期 ticket 的 sub 必须等于连接 user_id
		writeFrame(t, dev, map[string]any{"v": 1, "type": "renew", "ticket": env.tickets.issue("u-attacker", ttl)})
		expectClosedReason(t, dev, "auth")
	})

	t.Run("no renew", func(t *testing.T) {
		ticket := env.tickets.issue("u-idle", ttl)
		dev := env.dial(t, "")
		defer dev.CloseNow()
		start := time.Now()
		helloDevice(t, dev, ticket, "d-idle")
		readFrame(t, dev) // ticket_expiring
		expectClosedReason(t, dev, "auth")
		if elapsed := time.Since(start); elapsed < ttl+opts.RenewGrace-250*time.Millisecond {
			t.Fatalf("closed auth at %v, before exp+grace %v", elapsed, ttl+opts.RenewGrace)
		}
	})
}

func TestWSReplaceKick(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, nil)
	ticket := env.tickets.issue("u1", 10*time.Second)

	dev1 := env.dial(t, "")
	defer dev1.CloseNow()
	helloDevice(t, dev1, ticket, "d1")
	cl := env.dial(t, "")
	defer cl.CloseNow()
	helloClient(t, cl, ticket, "c1")

	// 同 device_id 双连 → 旧连收 closed replaced;client 同时收到新连的
	// presence online(Hub 对 device 注册一律广播)。
	dev2 := env.dial(t, "")
	defer dev2.CloseNow()
	helloDevice(t, dev2, ticket, "d1")
	expectClosedReason(t, dev1, "replaced")
	m := readFrame(t, cl)
	if m["type"] != "presence" || m["device_id"] != "d1" || m["online"] != true {
		t.Fatalf("client got %v, want presence d1 online (re-register)", m)
	}

	// 旧连 flush 后关闭且不再收帧:新连广播 app,旧连读不到(连接已关)
	writeFrame(t, dev2, map[string]any{"v": 1, "type": "app", "payload": map[string]any{"op": "ping"}})
	m = readFrame(t, cl)
	if m["type"] != "app" || m["from_device_id"] != "d1" {
		t.Fatalf("client got %v, want app from d1", m)
	}
	expectConnClosed(t, dev1, 2*time.Second)
}

func TestWSSlowConsumer(t *testing.T) {
	opts := testOptions()
	opts.OutboundBuffer = 8
	env := newWSTestEnv(t, opts, 5, nil)
	ticket := env.tickets.issue("u1", 30*time.Second)

	dev := env.dial(t, "")
	defer dev.CloseNow()
	helloDevice(t, dev, ticket, "d1")
	slow := env.dial(t, "")
	defer slow.CloseNow()
	helloClient(t, slow, ticket, "c-slow") // 此后不再读
	fast := env.dial(t, "")
	defer fast.CloseNow()
	helloClient(t, fast, ticket, "c-fast")

	// slow_consumer 关闭由 TCP 反压触发:只有内核发送缓冲填满、writer
	// 阻塞、出站 channel 溢出,Enqueue 才会走 slow_consumer 关闭路径。
	// 内核缓冲因平台/调参差异巨大(macOS  loopback 数百 KB,Linux
	// loopback 自调节默认上限 4MB,CI 可能更高),固定小体量帧在某些
	// 环境根本填不满 —— 原 64KB×20=1.28MB 版本在 CI(Linux loopback)
	// 被内核全部吃进,writer 从不阻塞,断言超时失败。改为 fast 后台
	// 排空 + 设备端持续发送,总量 256KB×128=32MB 超过任何常见
	// loopback 自调节上限;单帧 255KB+信封 < MaxFrameBytes(读循环按整
	// 帧判定,1024 余量只兜底 WS 库硬掐),不触发帧上限关闭。
	big := map[string]any{"data": strings.Repeat("x", 255<<10)}
	fastErr := make(chan error, 1)
	go func() {
		// fast 后台排空:既防止 fast 自己的出站缓冲(同为 8)被灌满
		// 而被误伤为 slow_consumer,也验证正常 client 不受影响。
		deadline := time.Now().Add(30 * time.Second)
		for {
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			_, _, err := fast.Read(ctx)
			cancel()
			if err != nil {
				fastErr <- err
				return
			}
		}
	}()
	for i := 0; i < 128; i++ {
		writeFrame(t, dev, map[string]any{"v": 1, "type": "app", "payload": big})
	}
	expectConnClosed(t, slow, 10*time.Second)
	// fast 的 drain goroutine 只能在测试收尾 CloseNow 后才报错退出;
	// 提前退出 = fast 连接被慢消费者株连。
	select {
	case err := <-fastErr:
		t.Fatalf("fast client conn broke while slow consumer was dropped: %v", err)
	default:
	}
}

func TestWSConnectionCaps(t *testing.T) {
	opts := testOptions()
	opts.MaxDevicesPerUser = 2
	env := newWSTestEnv(t, opts, 5, nil)
	ticket := env.tickets.issue("u1", 10*time.Second)

	d1 := env.dial(t, "")
	defer d1.CloseNow()
	helloDevice(t, d1, ticket, "d1")
	d2 := env.dial(t, "")
	defer d2.CloseNow()
	helloDevice(t, d2, ticket, "d2")

	// 第 3 个 device → closed protocol
	d3 := env.dial(t, "")
	defer d3.CloseNow()
	writeFrame(t, d3, map[string]any{
		"v": 1, "type": "hello", "ticket": ticket, "role": "device",
		"device_id": "d3", "device_name": "Mac", "app_version": "2.4.0",
	})
	expectClosedReason(t, d3, "protocol")
}

func TestWSHelloFailIPLimit(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 3, nil)
	// 同一来源 IP 连续 3 次坏 ticket hello(每次消耗 1 令牌,burst=3)
	for i := 0; i < 3; i++ {
		c := env.dial(t, "")
		writeFrame(t, c, map[string]any{
			"v": 1, "type": "hello", "ticket": "forged", "role": "device",
			"device_id": fmt.Sprintf("d%d", i), "device_name": "Mac", "app_version": "2.4.0",
		})
		expectClosedReason(t, c, "auth")
		c.CloseNow()
	}
	// 第 4 次握手直接 403,不升级
	env.dialExpectStatus(t, "", http.StatusForbidden)
}

// TestWSHelloFailSpoofedXFF:nginx 的 $proxy_add_x_forwarded_for 保留客户端
// 自带 XFF,若限流键取自原始 XFF 首段,攻击者轮换首段即可分摊限流桶。
// 修复后限流键来自调用方按可信链解析的 clientIP(测试基建固定
// testClientIP),伪造不同的 XFF 仍共享同一桶。
func TestWSHelloFailSpoofedXFF(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 3, nil)
	// 3 次坏 ticket hello,每次带不同的伪造 X-Forwarded-For。
	for i := 0; i < 3; i++ {
		h := http.Header{"X-Forwarded-For": []string{fmt.Sprintf("198.51.100.%d", i+1)}}
		c := env.dialWithHeaders(t, h)
		writeFrame(t, c, map[string]any{
			"v": 1, "type": "hello", "ticket": "forged", "role": "device",
			"device_id": fmt.Sprintf("d%d", i), "device_name": "Mac", "app_version": "2.4.0",
		})
		expectClosedReason(t, c, "auth")
		c.CloseNow()
	}
	// 第 4 次换一个全新的伪造 XFF,仍命中同一解析 IP 的桶 → 403。
	h := http.Header{"X-Forwarded-For": []string{"198.51.100.99"}}
	env.dialExpectStatusWithHeaders(t, h, http.StatusForbidden)
}

func TestWSOriginCheck(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, []string{"https://www.yunhouai.com"})
	ticket := env.tickets.issue("u1", 10*time.Second)

	// 白名单内 Origin → 握手成功
	c := env.dial(t, "https://www.yunhouai.com")
	helloClient(t, c, ticket, "c-web")
	c.CloseNow()

	// 白名单外 Origin → 403
	env.dialExpectStatus(t, "https://evil.com", http.StatusForbidden)

	// 不带 Origin → 成功(device 路径)
	c2 := env.dial(t, "")
	helloDevice(t, c2, ticket, "d-native")
	c2.CloseNow()

	// 白名单为空 = fail-closed:拒绝一切带 Origin 的握手,不带 Origin 仍放行
	env2 := newWSTestEnv(t, testOptions(), 5, nil)
	env2.dialExpectStatus(t, "https://www.yunhouai.com", http.StatusForbidden)
	c3 := env2.dial(t, "")
	helloDevice(t, c3, env2.tickets.issue("u2", 10*time.Second), "d-native-2")
	c3.CloseNow()
}

func TestWSOversizeFrame(t *testing.T) {
	opts := testOptions()
	opts.MaxFrameBytes = 1024
	env := newWSTestEnv(t, opts, 5, nil)
	ticket := env.tickets.issue("u1", 10*time.Second)

	dev := env.dial(t, "")
	defer dev.CloseNow()
	helloDevice(t, dev, ticket, "d1")
	cl := env.dial(t, "")
	defer cl.CloseNow()
	helloClient(t, cl, ticket, "c1")

	// 帧 > MaxFrameBytes 但 < MaxFrameBytes+1024(SetReadLimit 余量内,
	// 保证服务端来得及发 closed protocol 而不是被 read limit 直接掐断)
	writeFrame(t, dev, map[string]any{
		"v": 1, "type": "app", "payload": map[string]any{"data": strings.Repeat("x", 1500)},
	})
	expectClosedReason(t, dev, "protocol")
}

func TestWSFrameRateLimit(t *testing.T) {
	opts := testOptions()
	opts.MaxFramesPerSec = 5
	env := newWSTestEnv(t, opts, 5, nil)
	ticket := env.tickets.issue("u1", 10*time.Second)

	dev := env.dial(t, "")
	defer dev.CloseNow()
	helloDevice(t, dev, ticket, "d1")

	// burst=5 秒内灌 20 帧 → 超速率上限 closed protocol
	for i := 0; i < 20; i++ {
		writeFrame(t, dev, map[string]any{"v": 1, "type": "app", "payload": map[string]any{"i": i}})
	}
	expectClosedReason(t, dev, "protocol")
}

func TestWSGracefulShutdown(t *testing.T) {
	env := newWSTestEnv(t, testOptions(), 5, nil)
	ticket := env.tickets.issue("u1", 10*time.Second)

	dev := env.dial(t, "")
	defer dev.CloseNow()
	helloDevice(t, dev, ticket, "d1")
	cl := env.dial(t, "")
	defer cl.CloseNow()
	helloClient(t, cl, ticket, "c1")

	if env.hub.ShutdownStarted() {
		t.Fatal("ShutdownStarted before Shutdown")
	}
	env.hub.Shutdown(5 * time.Second)
	if !env.hub.ShutdownStarted() {
		t.Fatal("ShutdownStarted false after Shutdown")
	}

	// 停机帧序(spec §5.3):client 必须先收到 device 的 presence offline,
	// 然后才是自己的 closed shutdown。旧实现顺序相反,closed 帧同步置
	// closing 后 presence 被静默丢弃——本断言守护修复后的行为。
	m := readFrame(t, cl)
	if m["type"] != "presence" || m["device_id"] != "d1" || m["online"] != false {
		t.Fatalf("first shutdown frame on client = %v, want presence offline for d1", m)
	}
	// device 与 client 都收到 closed shutdown;flush 由 conn 层 finalize
	// 等 closedWritten 保证,不依赖墙钟 sleep。
	expectClosedReason(t, dev, "shutdown")
	expectClosedReason(t, cl, "shutdown")
	expectConnClosed(t, dev, 2*time.Second)
	expectConnClosed(t, cl, 2*time.Second)

	// 停机后新握手在 handler 层直接 503,不升级
	env.dialExpectStatus(t, "", http.StatusServiceUnavailable)
}

// TestWSHelloFailNilLimiter:fails 限流器为 nil(关闭失败限流)时,
// hello 失败路径不得 panic(RecordFailure 全部判空),连接按常规
// closed auth 关闭。
func TestWSHelloFailNilLimiter(t *testing.T) {
	hub := NewHub(testOptions(), nil)
	tickets := newStubTickets()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandleWS(w, r, hub, tickets, nil, testClientIP)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/relay/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	writeFrame(t, c, map[string]any{
		"v": 1, "type": "hello", "ticket": "forged", "role": "device",
		"device_id": "d1", "device_name": "Mac", "app_version": "2.4.0",
	})
	expectClosedReason(t, c, "auth")
	expectConnClosed(t, c, 2*time.Second)
}
