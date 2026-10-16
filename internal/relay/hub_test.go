package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeConn struct {
	userID   string
	role     Role
	deviceID string
	clientID string
	meta     DeviceInfo

	mu     sync.Mutex
	frames [][]byte
}

func (f *fakeConn) UserID() string         { return f.userID }
func (f *fakeConn) GetRole() Role          { return f.role }
func (f *fakeConn) DeviceID() string       { return f.deviceID }
func (f *fakeConn) ClientID() string       { return f.clientID }
func (f *fakeConn) DeviceMeta() DeviceInfo { return f.meta }
func (f *fakeConn) Enqueue(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	f.frames = append(f.frames, cp)
}
func (f *fakeConn) decoded(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.frames[i], &m); err != nil {
		t.Fatalf("frame %d: %v", i, err)
	}
	return m
}
func (f *fakeConn) frameCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.frames) }

func newDeviceConn(userID, deviceID, name string) *fakeConn {
	return &fakeConn{
		userID:   userID,
		role:     RoleDevice,
		deviceID: deviceID,
		meta: DeviceInfo{
			DeviceID:    deviceID,
			DeviceName:  name,
			AppVersion:  "1.0.0",
			ConnectedAt: time.Now().Unix(),
		},
	}
}

func newClientConn(userID, clientID string) *fakeConn {
	return &fakeConn{userID: userID, role: RoleClient, clientID: clientID}
}

func mustRegister(t *testing.T, h *Hub, c Conn) Conn {
	t.Helper()
	kicked, err := h.Register(c)
	if err != nil {
		t.Fatalf("Register(%s/%s): %v", c.GetRole(), c.UserID(), err)
	}
	return kicked
}

func TestHubRegisterKickSameDeviceID(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	d1 := newDeviceConn("u1", "dev-1", "old")
	d2 := newDeviceConn("u1", "dev-1", "new")

	if kicked := mustRegister(t, h, d1); kicked != nil {
		t.Fatalf("first Register kicked = %v, want nil", kicked)
	}
	kicked := mustRegister(t, h, d2)
	if kicked != d1 {
		t.Fatalf("second Register kicked = %p, want first conn %p", kicked, d1)
	}
	if d1.frameCount() != 1 {
		t.Fatalf("kicked conn frames = %d, want 1", d1.frameCount())
	}
	m := d1.decoded(t, 0)
	if m["type"] != "closed" || m["reason"] != string(ReasonReplaced) {
		t.Fatalf("kicked frame = %v, want closed/replaced", m)
	}
	// 新连在位
	devs := h.OnlineDevices("u1")
	if len(devs) != 1 || devs[0].DeviceName != "new" {
		t.Fatalf("OnlineDevices = %+v, want only new conn", devs)
	}
}

func TestHubDevicePresenceBroadcast(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	c1 := newClientConn("u1", "cli-1")
	c2 := newClientConn("u1", "cli-2")
	mustRegister(t, h, c1)
	mustRegister(t, h, c2)

	d := newDeviceConn("u1", "dev-1", "phone")
	mustRegister(t, h, d)

	for _, c := range []*fakeConn{c1, c2} {
		if c.frameCount() != 1 {
			t.Fatalf("client frames after device online = %d, want 1", c.frameCount())
		}
		m := c.decoded(t, 0)
		if m["type"] != "presence" || m["device_id"] != "dev-1" || m["online"] != true {
			t.Fatalf("presence online frame = %v", m)
		}
		meta, ok := m["meta"].(map[string]any)
		if !ok {
			t.Fatalf("presence online missing meta: %v", m)
		}
		if meta["device_name"] != "phone" || meta["app_version"] != "1.0.0" {
			t.Fatalf("presence meta = %v, want device_name=phone app_version=1.0.0", meta)
		}
	}

	h.Unregister(d)
	for _, c := range []*fakeConn{c1, c2} {
		if c.frameCount() != 2 {
			t.Fatalf("client frames after device offline = %d, want 2", c.frameCount())
		}
		m := c.decoded(t, 1)
		if m["type"] != "presence" || m["device_id"] != "dev-1" || m["online"] != false {
			t.Fatalf("presence offline frame = %v", m)
		}
		if _, hasMeta := m["meta"]; hasMeta {
			t.Fatalf("presence offline must not carry meta: %v", m)
		}
	}

	// client 上下线不广播
	h.Unregister(c1)
	if c2.frameCount() != 2 {
		t.Fatalf("client unregister broadcast frames = %d, want 2 (no broadcast)", c2.frameCount())
	}
}

func TestHubHelloOKDeviceList(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	mustRegister(t, h, newDeviceConn("u1", "dev-b", "b"))
	mustRegister(t, h, newDeviceConn("u1", "dev-a", "a"))

	devs := h.OnlineDevices("u1")
	if len(devs) != 2 {
		t.Fatalf("OnlineDevices len = %d, want 2", len(devs))
	}
	if devs[0].DeviceID != "dev-a" || devs[1].DeviceID != "dev-b" {
		t.Fatalf("OnlineDevices not sorted by device_id: %+v", devs)
	}
	if devs[0].DeviceName != "a" || devs[0].AppVersion != "1.0.0" || devs[0].ConnectedAt == 0 {
		t.Fatalf("OnlineDevices[0] meta wrong: %+v", devs[0])
	}
	if got := h.OnlineDevices("no-such-user"); len(got) != 0 {
		t.Fatalf("OnlineDevices(unknown) = %+v, want empty", got)
	}

	// hello_ok 帧数据源即 OnlineDevices
	frame := HelloOKFrame(devs, 1234)
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "hello_ok" || m["server_time"].(float64) != 1234 {
		t.Fatalf("hello_ok frame = %v", m)
	}
	if got := len(m["room_devices"].([]any)); got != 2 {
		t.Fatalf("hello_ok room_devices len = %d, want 2", got)
	}
}

func TestHubRouteDeviceBroadcast(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	d := newDeviceConn("u1", "dev-1", "phone")
	mustRegister(t, h, d)
	c1 := newClientConn("u1", "cli-1")
	c2 := newClientConn("u1", "cli-2")
	mustRegister(t, h, c1)
	mustRegister(t, h, c2)
	other := newClientConn("u2", "cli-9")
	mustRegister(t, h, other)

	payload := json.RawMessage(`{"a":1}`)
	h.RouteApp(d, "", payload)

	want := AppFromDeviceFrame("dev-1", payload)
	for _, c := range []*fakeConn{c1, c2} {
		if c.frameCount() != 1 {
			t.Fatalf("client frames = %d, want 1", c.frameCount())
		}
		c.mu.Lock()
		got := string(c.frames[0])
		c.mu.Unlock()
		if got != string(want) {
			t.Fatalf("client frame = %s, want %s", got, want)
		}
		m := c.decoded(t, 0)
		if m["type"] != "app" || m["from_device_id"] != "dev-1" {
			t.Fatalf("app frame = %v", m)
		}
		pm, err := json.Marshal(m["payload"])
		if err != nil || string(pm) != `{"a":1}` {
			t.Fatalf("payload = %s, err=%v", pm, err)
		}
	}
	if other.frameCount() != 0 {
		t.Fatalf("cross-room client received %d frames, want 0", other.frameCount())
	}
}

func TestHubRouteClientToDevice(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	d1 := newDeviceConn("u1", "dev-1", "one")
	d2 := newDeviceConn("u1", "dev-2", "two")
	mustRegister(t, h, d1)
	mustRegister(t, h, d2)
	c := newClientConn("u1", "cli-1")
	mustRegister(t, h, c)

	// presence 扩投 device(改动 3)后的基线:d1 收到 d2 的上线 presence,
	// d2 收到 d1 的入会快照,各 1 帧;app 帧在此之后到达。
	if d1.frameCount() != 1 || d2.frameCount() != 1 {
		t.Fatalf("presence baseline: d1=%d d2=%d, want 1/1", d1.frameCount(), d2.frameCount())
	}

	payload := json.RawMessage(`{"cmd":"sync"}`)
	h.RouteApp(c, "dev-1", payload)

	if d1.frameCount() != 2 {
		t.Fatalf("target device frames = %d, want 2 (presence + app)", d1.frameCount())
	}
	m := d1.decoded(t, 1)
	if m["type"] != "app" || m["from_client_id"] != "cli-1" {
		t.Fatalf("app frame = %v, want from_client_id=cli-1", m)
	}
	pm, err := json.Marshal(m["payload"])
	if err != nil || string(pm) != `{"cmd":"sync"}` {
		t.Fatalf("payload = %s, err=%v", pm, err)
	}
	if d2.frameCount() != 1 {
		t.Fatalf("other device received %d frames, want 1 (仅入会快照,不收 app)", d2.frameCount())
	}
}

func TestHubRouteUndeliverable(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	c := newClientConn("u1", "cli-1")
	mustRegister(t, h, c)

	// room 内不存在目标
	h.RouteApp(c, "dev-x", json.RawMessage(`{"a":1}`))
	if c.frameCount() != 1 {
		t.Fatalf("sender frames = %d, want 1", c.frameCount())
	}
	m := c.decoded(t, 0)
	if m["type"] != "undeliverable" || m["target_device_id"] != "dev-x" {
		t.Fatalf("undeliverable frame = %v", m)
	}

	// 跨 room:目标存在于别人的 room,也不可达且不泄露
	devB := newDeviceConn("u2", "dev-b", "b")
	mustRegister(t, h, devB)
	h.RouteApp(c, "dev-b", json.RawMessage(`{"a":1}`))
	if c.frameCount() != 2 {
		t.Fatalf("sender frames after cross-room route = %d, want 2", c.frameCount())
	}
	m = c.decoded(t, 1)
	if m["type"] != "undeliverable" || m["target_device_id"] != "dev-b" {
		t.Fatalf("cross-room undeliverable frame = %v", m)
	}
	if devB.frameCount() != 0 {
		t.Fatalf("cross-room device received %d frames, want 0 (existence must not leak)", devB.frameCount())
	}
}

func TestHubConnectionCaps(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxDevicesPerUser = 2
	opts.MaxClientsPerUser = 2
	h := NewHub(opts, nil)

	mustRegister(t, h, newDeviceConn("u1", "dev-1", "1"))
	mustRegister(t, h, newDeviceConn("u1", "dev-2", "2"))
	_, err := h.Register(newDeviceConn("u1", "dev-3", "3"))
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Reason != ReasonProtocol {
		t.Fatalf("3rd device Register err = %v, want *RejectError{protocol}", err)
	}
	// 替换同 id 不占限额
	if kicked := mustRegister(t, h, newDeviceConn("u1", "dev-1", "1-new")); kicked == nil {
		t.Fatal("replacing existing device under full cap must succeed and kick old")
	}

	mustRegister(t, h, newClientConn("u1", "cli-1"))
	mustRegister(t, h, newClientConn("u1", "cli-2"))
	_, err = h.Register(newClientConn("u1", "cli-3"))
	if !errors.As(err, &rej) || rej.Reason != ReasonProtocol {
		t.Fatalf("3rd client Register err = %v, want *RejectError{protocol}", err)
	}
}

func TestHubUnregisterSameIDNewerConnSafe(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	d1 := newDeviceConn("u1", "dev-1", "old")
	d2 := newDeviceConn("u1", "dev-1", "new")
	mustRegister(t, h, d1)
	mustRegister(t, h, d2)

	// 旧连延迟触发 Unregister,不得摘除新连
	h.Unregister(d1)
	devs := h.OnlineDevices("u1")
	if len(devs) != 1 || devs[0].DeviceName != "new" {
		t.Fatalf("OnlineDevices after stale Unregister = %+v, want new conn still online", devs)
	}
}

func TestHubShutdown(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	d := newDeviceConn("u1", "dev-1", "phone")
	mustRegister(t, h, d)
	c := newClientConn("u1", "cli-1")
	mustRegister(t, h, c)
	c1frames := c.frameCount() // device 上线 presence(此处为 1)

	if h.ShutdownStarted() {
		t.Fatal("ShutdownStarted before Shutdown = true")
	}
	h.Shutdown(100 * time.Millisecond)
	if !h.ShutdownStarted() {
		t.Fatal("ShutdownStarted after Shutdown = false")
	}

	if d.frameCount() != 1 {
		t.Fatalf("device frames = %d, want 1 (closed shutdown)", d.frameCount())
	}
	m := d.decoded(t, 0)
	if m["type"] != "closed" || m["reason"] != string(ReasonShutdown) {
		t.Fatalf("device closed frame = %v", m)
	}
	if c.frameCount() != c1frames+2 {
		t.Fatalf("client frames = %d, want %d (presence offline + closed shutdown)", c.frameCount(), c1frames+2)
	}
	// presence offline 必须先于 closed shutdown 入队:真实 wsConn 对
	// closed 帧会同步置 closing,反序会让 presence 被静默丢弃(spec §5.3)。
	m = c.decoded(t, c1frames)
	if m["type"] != "presence" || m["device_id"] != "dev-1" || m["online"] != false {
		t.Fatalf("client shutdown presence frame = %v", m)
	}
	m = c.decoded(t, c1frames+1)
	if m["type"] != "closed" || m["reason"] != string(ReasonShutdown) {
		t.Fatalf("client closed frame = %v", m)
	}

	// 停机后拒绝新连接
	_, err := h.Register(newClientConn("u1", "cli-2"))
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Reason != ReasonShutdown {
		t.Fatalf("Register after shutdown err = %v, want *RejectError{shutdown}", err)
	}
}

// TestHubShutdownHonorsWait:Shutdown(wait) 等待房间清空(全部连接
// Unregister)即返回,不空转满 wait;连接迟迟不摘除时按 wait 上限返回。
func TestHubShutdownHonorsWait(t *testing.T) {
	t.Run("rooms drained returns early", func(t *testing.T) {
		h := NewHub(DefaultOptions(), nil)
		d := newDeviceConn("u1", "dev-1", "phone")
		mustRegister(t, h, d)
		cl := newClientConn("u1", "cli-1")
		mustRegister(t, h, cl)

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.Shutdown(5 * time.Second)
		}()
		// 模拟 conn 层 flush 完成后 Unregister(真实 wsConn 的收尾路径):
		// 只摘 device 时房间仍在,Shutdown 不得提前返回
		time.Sleep(50 * time.Millisecond)
		h.Unregister(d)
		select {
		case <-done:
			t.Fatal("Shutdown returned before all conns unregistered (client still in room)")
		case <-time.After(100 * time.Millisecond):
		}
		// client 也摘除后房间清空,Shutdown 应立即返回
		h.Unregister(cl)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Shutdown did not return after rooms drained")
		}
	})

	t.Run("wait deadline caps the wait", func(t *testing.T) {
		h := NewHub(DefaultOptions(), nil)
		mustRegister(t, h, newDeviceConn("u1", "dev-1", "phone"))
		start := time.Now()
		h.Shutdown(100 * time.Millisecond) // fakeConn 永不 Unregister
		elapsed := time.Since(start)
		if elapsed < 100*time.Millisecond {
			t.Fatalf("Shutdown returned after %v, before wait deadline", elapsed)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("Shutdown blocked %v, far past wait deadline", elapsed)
		}
	})

	t.Run("wait<=0 returns immediately", func(t *testing.T) {
		h := NewHub(DefaultOptions(), nil)
		mustRegister(t, h, newDeviceConn("u1", "dev-1", "phone"))
		start := time.Now()
		h.Shutdown(0)
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("Shutdown(0) blocked %v, want immediate return", elapsed)
		}
	})
}

// TestHubRegisterShutdownRace 回归测试:Register 与 Shutdown 并发时,
// 每个 Register 成功的连接都必须最终收到 closed shutdown(否则即
// TOCTOU 漏连:停机后插入房间、永远收不到关闭帧且房间泄漏)。
func TestHubRegisterShutdownRace(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)

	const workers = 16
	const perWorker = 25

	var wg sync.WaitGroup
	success := make(chan *fakeConn, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("w%02d-%02d", w, i)
				var c *fakeConn
				if i%2 == 0 {
					c = newDeviceConn("u-race", "dev-"+id, id)
				} else {
					c = newClientConn("u-race", "cli-"+id)
				}
				if _, err := h.Register(c); err == nil {
					success <- c
				}
			}
		}(w)
	}

	// 让一部分 Register 先跑,然后在 hammer 进行中停机
	time.Sleep(time.Millisecond)
	h.Shutdown(0)

	wg.Wait()
	close(success)

	n := 0
	for c := range success {
		n++
		found := false
		c.mu.Lock()
		for _, raw := range c.frames {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				c.mu.Unlock()
				t.Fatalf("bad frame on registered conn: %v", err)
			}
			if m["type"] == "closed" && m["reason"] == string(ReasonShutdown) {
				found = true
				break
			}
		}
		c.mu.Unlock()
		if !found {
			t.Fatalf("conn %s/%s registered successfully but never received closed shutdown", c.GetRole(), c.UserID())
		}
	}
	if n == 0 {
		t.Fatal("no conn registered before shutdown; test did not exercise the race window")
	}
	t.Logf("%d conns registered before shutdown, all received closed shutdown", n)

	// 停机后注册一律被拒
	_, err := h.Register(newClientConn("u-race", "cli-late"))
	if err == nil {
		t.Fatal("Register after shutdown succeeded, want *RejectError{shutdown}")
	}
}

// ---- relay delta(2026-10-16,跨机协同):发送方盖章 / device→device 定向 /
// presence 扩投 device + 入会快照 ----

// TestHubRouteDeviceDirected:device 连接发出的 App 帧带非空 target_device_id
// 时只定向投递(改动 2),信封 sender 一律按入站连接认证身份盖章(改动 1,
// 发送方自填字段不解析、不采信,出站帧由 relay 重新构造)。
func TestHubRouteDeviceDirected(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	devA := newDeviceConn("u1", "dev-a", "A")
	devB := newDeviceConn("u1", "dev-b", "B")
	mustRegister(t, h, devA)
	mustRegister(t, h, devB)
	cl := newClientConn("u1", "cli-1")
	mustRegister(t, h, cl)
	// 基线:devA 收到 devB 上线 presence;devB 收到 devA 入会快照;cl 无帧。
	baseA, baseB, baseC := devA.frameCount(), devB.frameCount(), cl.frameCount()
	if baseA != 1 || baseB != 1 || baseC != 0 {
		t.Fatalf("presence baseline: A=%d B=%d C=%d, want 1/1/0", baseA, baseB, baseC)
	}

	payload := json.RawMessage(`{"kind":"peer","frame":{"kind":"hello"}}`)

	// 定向:dev-a → dev-b,只有 B 收到;client 与发送方都收不到。
	h.RouteApp(devA, "dev-b", payload)
	if devB.frameCount() != baseB+1 {
		t.Fatalf("directed target frames = %d, want %d", devB.frameCount(), baseB+1)
	}
	m := devB.decoded(t, baseB)
	if m["type"] != "app" {
		t.Fatalf("directed frame = %v, want type=app", m)
	}
	// 改动 1 盖章:from_device_id = 发送连接的真实 device_id,from_client_id 清零。
	if m["from_device_id"] != "dev-a" {
		t.Fatalf("directed frame from_device_id = %v, want dev-a (server-stamped)", m)
	}
	if _, ok := m["from_client_id"]; ok {
		t.Fatalf("device-sourced frame must not carry from_client_id: %v", m)
	}
	// 改动 2:定向投递帧带 target_device_id = 收件人(客户端纵深防御依赖)。
	if m["target_device_id"] != "dev-b" {
		t.Fatalf("directed frame target_device_id = %v, want dev-b", m)
	}
	pm, err := json.Marshal(m["payload"])
	// 解码再序列化后 map 键按字典序重排;断言语义内容而非字节序
	// (payload 字节级原样透传由 WS 级测试锁定)。
	if err != nil || string(pm) != `{"frame":{"kind":"hello"},"kind":"peer"}` {
		t.Fatalf("payload = %s, err=%v", pm, err)
	}
	if devA.frameCount() != baseA {
		t.Fatalf("sender received %d frames, want %d (不回投)", devA.frameCount(), baseA)
	}
	if cl.frameCount() != baseC {
		t.Fatalf("client received %d frames, want %d (定向帧不广播 client)", cl.frameCount(), baseC)
	}

	// 目标离线/不存在 → 沿用 undeliverable 语义,只回发送方。
	h.RouteApp(devA, "ghost", payload)
	if devA.frameCount() != baseA+1 {
		t.Fatalf("sender frames after offline target = %d, want %d", devA.frameCount(), baseA+1)
	}
	m = devA.decoded(t, baseA)
	if m["type"] != "undeliverable" || m["target_device_id"] != "ghost" {
		t.Fatalf("undeliverable frame = %v", m)
	}
	if devB.frameCount() != baseB+1 {
		t.Fatalf("other device frames after undeliverable = %d, want %d", devB.frameCount(), baseB+1)
	}

	// 跨 room 目标:同样 undeliverable,不泄露存在性。
	devC := newDeviceConn("u2", "dev-c", "C")
	mustRegister(t, h, devC)
	h.RouteApp(devA, "dev-c", payload)
	if devA.frameCount() != baseA+2 {
		t.Fatalf("sender frames after cross-room target = %d, want %d", devA.frameCount(), baseA+2)
	}
	m = devA.decoded(t, baseA+1)
	if m["type"] != "undeliverable" || m["target_device_id"] != "dev-c" {
		t.Fatalf("cross-room undeliverable frame = %v", m)
	}
	if devC.frameCount() != 0 {
		t.Fatalf("cross-room device received %d frames, want 0 (existence must not leak)", devC.frameCount())
	}

	// 目标为发送方自身:同样回 undeliverable,不向自己回声(防回声帧干扰
	// 对端握手状态机)。
	h.RouteApp(devA, "dev-a", payload)
	if devA.frameCount() != baseA+3 {
		t.Fatalf("sender frames after self target = %d, want %d (undeliverable)", devA.frameCount(), baseA+3)
	}
	m = devA.decoded(t, baseA+2)
	if m["type"] != "undeliverable" || m["target_device_id"] != "dev-a" {
		t.Fatalf("self-target frame = %v, want undeliverable target_device_id=dev-a", m)
	}
	if devB.frameCount() != baseB+1 || cl.frameCount() != baseC {
		t.Fatalf("self-target leaked: B=%d C=%d", devB.frameCount(), cl.frameCount())
	}

	// 回归:不带 target 的 device 帧维持原语义——只广播房间内全部 client,
	// 不到其他 device;信封仍由服务端盖章。
	h.RouteApp(devA, "", payload)
	if cl.frameCount() != baseC+1 {
		t.Fatalf("client frames after broadcast = %d, want %d", cl.frameCount(), baseC+1)
	}
	m = cl.decoded(t, baseC)
	if m["type"] != "app" || m["from_device_id"] != "dev-a" {
		t.Fatalf("broadcast frame = %v, want app from_device_id=dev-a", m)
	}
	if _, ok := m["from_client_id"]; ok {
		t.Fatalf("device broadcast must not carry from_client_id: %v", m)
	}
	if _, ok := m["target_device_id"]; ok {
		t.Fatalf("device broadcast must not carry target_device_id: %v", m)
	}
	if devB.frameCount() != baseB+1 || devA.frameCount() != baseA+3 {
		t.Fatalf("device broadcast leaked to devices: A=%d B=%d", devA.frameCount(), devB.frameCount())
	}
}

// TestHubPresenceToDevicesAndSnapshot:presence(含 LWT 离线)投递范围扩为
// 房间内全部 client + 全部 device(不含事件主体自身);device 入会补发其他
// 在线 device 的 presence 快照,逐台分帧(改动 3)。
func TestHubPresenceToDevicesAndSnapshot(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	devA := newDeviceConn("u1", "dev-a", "Alpha")
	mustRegister(t, h, devA)
	if devA.frameCount() != 0 {
		t.Fatalf("sole device frames = %d, want 0 (无其他 device,无快照)", devA.frameCount())
	}

	// B 后于 A 入会:B 收到含 A 的在线快照,A 收到 B 的上线 presence。
	devB := newDeviceConn("u1", "dev-b", "Beta")
	mustRegister(t, h, devB)
	if devB.frameCount() != 1 {
		t.Fatalf("joiner snapshot frames = %d, want 1", devB.frameCount())
	}
	m := devB.decoded(t, 0)
	if m["type"] != "presence" || m["device_id"] != "dev-a" || m["online"] != true {
		t.Fatalf("snapshot frame = %v, want presence dev-a online", m)
	}
	meta, ok := m["meta"].(map[string]any)
	if !ok || meta["device_name"] != "Alpha" {
		t.Fatalf("snapshot meta = %v, want device_name=Alpha", m)
	}
	if devA.frameCount() != 1 {
		t.Fatalf("existing device frames = %d, want 1 (peer online presence)", devA.frameCount())
	}
	m = devA.decoded(t, 0)
	if m["type"] != "presence" || m["device_id"] != "dev-b" || m["online"] != true {
		t.Fatalf("peer online frame = %v, want presence dev-b online", m)
	}

	// client 入会/离会不触达 device(client 侧 presence 语义不变)。
	cl := newClientConn("u1", "cli-1")
	mustRegister(t, h, cl)
	h.Unregister(cl)
	if devA.frameCount() != 1 || devB.frameCount() != 1 {
		t.Fatalf("client join/leave leaked to devices: A=%d B=%d, want 1/1",
			devA.frameCount(), devB.frameCount())
	}

	// B 离线(LWT)→ A 收到 B 的离线 presence(此前只有 client 收到);
	// 事件主体自身不收自己的 presence。
	h.Unregister(devB)
	if devA.frameCount() != 2 {
		t.Fatalf("peer frames after LWT = %d, want 2", devA.frameCount())
	}
	m = devA.decoded(t, 1)
	if m["type"] != "presence" || m["device_id"] != "dev-b" || m["online"] != false {
		t.Fatalf("peer offline frame = %v, want presence dev-b offline", m)
	}
	if _, hasMeta := m["meta"]; hasMeta {
		t.Fatalf("offline presence must not carry meta: %v", m)
	}
	if devB.frameCount() != 1 {
		t.Fatalf("subject received own offline presence: frames = %d, want 1", devB.frameCount())
	}
}

// TestHubShutdownPresenceToDevices:优雅停机的 presence offline 同样扩投
// 房间内其他 device,且仍先于 closed shutdown 入队。
func TestHubShutdownPresenceToDevices(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	devA := newDeviceConn("u1", "dev-a", "A")
	devB := newDeviceConn("u1", "dev-b", "B")
	mustRegister(t, h, devA)
	mustRegister(t, h, devB)
	cl := newClientConn("u1", "cli-1")
	mustRegister(t, h, cl)
	baseA, baseB, baseC := devA.frameCount(), devB.frameCount(), cl.frameCount()

	h.Shutdown(0)

	// 每台 device:对方 offline presence 先于自己的 closed shutdown。
	for _, tc := range []struct {
		conn   *fakeConn
		base   int
		peerID string
	}{{devA, baseA, "dev-b"}, {devB, baseB, "dev-a"}} {
		if tc.conn.frameCount() != tc.base+2 {
			t.Fatalf("%s frames = %d, want %d (peer offline presence + closed)",
				tc.conn.deviceID, tc.conn.frameCount(), tc.base+2)
		}
		m := tc.conn.decoded(t, tc.base)
		if m["type"] != "presence" || m["device_id"] != tc.peerID || m["online"] != false {
			t.Fatalf("%s shutdown frame[0] = %v, want presence %s offline", tc.conn.deviceID, m, tc.peerID)
		}
		m = tc.conn.decoded(t, tc.base+1)
		if m["type"] != "closed" || m["reason"] != string(ReasonShutdown) {
			t.Fatalf("%s shutdown frame[1] = %v, want closed shutdown", tc.conn.deviceID, m)
		}
	}

	// client:两台 device 的 offline presence(顺序不限)先于 closed。
	if cl.frameCount() != baseC+3 {
		t.Fatalf("client frames = %d, want %d (2 presence + closed)", cl.frameCount(), baseC+3)
	}
	seen := map[string]bool{}
	for i := baseC; i < baseC+2; i++ {
		m := cl.decoded(t, i)
		if m["type"] != "presence" || m["online"] != false {
			t.Fatalf("client shutdown frame[%d] = %v, want presence offline", i, m)
		}
		seen[m["device_id"].(string)] = true
	}
	if !seen["dev-a"] || !seen["dev-b"] {
		t.Fatalf("client shutdown presence set = %v, want dev-a+dev-b", seen)
	}
	m := cl.decoded(t, baseC+2)
	if m["type"] != "closed" || m["reason"] != string(ReasonShutdown) {
		t.Fatalf("client shutdown last frame = %v, want closed shutdown", m)
	}
}

// TestHubSnapshotSortedByDeviceID:入会快照逐台分帧按 device_id 升序
// (map 迭代序随机,排序是显式行为,回归时不得丢失)。
func TestHubSnapshotSortedByDeviceID(t *testing.T) {
	h := NewHub(DefaultOptions(), nil)
	// 乱序注册三台在先 device。
	mustRegister(t, h, newDeviceConn("u1", "dev-c", "C"))
	mustRegister(t, h, newDeviceConn("u1", "dev-a", "A"))
	mustRegister(t, h, newDeviceConn("u1", "dev-b", "B"))

	joiner := newDeviceConn("u1", "dev-d", "D")
	mustRegister(t, h, joiner)

	if joiner.frameCount() != 3 {
		t.Fatalf("snapshot frames = %d, want 3", joiner.frameCount())
	}
	want := []string{"dev-a", "dev-b", "dev-c"}
	for i, id := range want {
		m := joiner.decoded(t, i)
		if m["type"] != "presence" || m["device_id"] != id || m["online"] != true {
			t.Fatalf("snapshot frame[%d] = %v, want presence %s online (sorted)", i, m, id)
		}
	}
}

// TestHubRegisterSnapshotUnregisterRace 回归测试:device 入会快照的收集与
// 入队都在 h.mu 临界区内完成,与并发 Unregister 互斥——新入会 device 对同一
// peer 收到的 presence 帧序必须 online 在前、offline 在后(或仅有其一),
// 绝不能 offline 之后又收到 online(陈旧在线状态永不纠正)。配合 -race 运行。
func TestHubRegisterSnapshotUnregisterRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		h := NewHub(DefaultOptions(), nil)
		d := newDeviceConn("u1", "dev-d", "D")
		mustRegister(t, h, d)
		c := newDeviceConn("u1", "dev-c", "C")

		var wg sync.WaitGroup
		var regErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, regErr = h.Register(c) }()
		go func() { defer wg.Done(); h.Unregister(d) }()
		wg.Wait()
		if regErr != nil {
			t.Fatalf("iter %d: Register joiner: %v", i, regErr)
		}

		seenOffline := false
		c.mu.Lock()
		for _, raw := range c.frames {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				c.mu.Unlock()
				t.Fatalf("iter %d: bad frame: %v", i, err)
			}
			if m["type"] != "presence" || m["device_id"] != "dev-d" {
				continue
			}
			if m["online"] == false {
				seenOffline = true
			} else if seenOffline {
				c.mu.Unlock()
				t.Fatalf("iter %d: joiner received presence online AFTER offline for dev-d (stale online)", i)
			}
		}
		c.mu.Unlock()
	}
}
