package relay

import (
	"encoding/json"
	"errors"
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
	h := NewHub(DefaultOptions())
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
	h := NewHub(DefaultOptions())
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
	h := NewHub(DefaultOptions())
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
	h := NewHub(DefaultOptions())
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
	h := NewHub(DefaultOptions())
	d1 := newDeviceConn("u1", "dev-1", "one")
	d2 := newDeviceConn("u1", "dev-2", "two")
	mustRegister(t, h, d1)
	mustRegister(t, h, d2)
	c := newClientConn("u1", "cli-1")
	mustRegister(t, h, c)

	payload := json.RawMessage(`{"cmd":"sync"}`)
	h.RouteApp(c, "dev-1", payload)

	if d1.frameCount() != 1 {
		t.Fatalf("target device frames = %d, want 1", d1.frameCount())
	}
	m := d1.decoded(t, 0)
	if m["type"] != "app" || m["from_client_id"] != "cli-1" {
		t.Fatalf("app frame = %v, want from_client_id=cli-1", m)
	}
	pm, err := json.Marshal(m["payload"])
	if err != nil || string(pm) != `{"cmd":"sync"}` {
		t.Fatalf("payload = %s, err=%v", pm, err)
	}
	if d2.frameCount() != 0 {
		t.Fatalf("other device received %d frames, want 0", d2.frameCount())
	}
}

func TestHubRouteUndeliverable(t *testing.T) {
	h := NewHub(DefaultOptions())
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
	h := NewHub(opts)

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
	h := NewHub(DefaultOptions())
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
	h := NewHub(DefaultOptions())
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
		t.Fatalf("client frames = %d, want %d (closed shutdown + presence offline)", c.frameCount(), c1frames+2)
	}
	m = c.decoded(t, c1frames)
	if m["type"] != "closed" || m["reason"] != string(ReasonShutdown) {
		t.Fatalf("client closed frame = %v", m)
	}
	m = c.decoded(t, c1frames+1)
	if m["type"] != "presence" || m["device_id"] != "dev-1" || m["online"] != false {
		t.Fatalf("client shutdown presence frame = %v", m)
	}

	// 停机后拒绝新连接
	_, err := h.Register(newClientConn("u1", "cli-2"))
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Reason != ReasonShutdown {
		t.Fatalf("Register after shutdown err = %v, want *RejectError{shutdown}", err)
	}
}
