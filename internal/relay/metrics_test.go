package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ---- gather 断言辅助 ----

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// metricValue 返回匹配 name+labels 的 gauge/counter 值;不存在则为 0
// (CounterVec 子序列在首次观测前不存在,语义等同于 0)。
func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m, labels) {
				continue
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue()
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}

func histCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsMatch(m, labels) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// waitMetric 轮询直到指标达到 want(WS 层断言:服务端计数与客户端观测到
// closed 之间无同步保证,必须容忍短暂的计数滞后)。
func waitMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := metricValue(t, reg, name, labels)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s%v = %v, want %v", name, labels, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMetricsHubFlow 经 Hub 的 fakeConn 级流程验证连接/房间/帧/undeliverable/
// 时长全部指标(spec §9)。
func TestMetricsHubFlow(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, "test")
	h := NewHub(DefaultOptions(), m)

	d := newDeviceConn("u1", "dev-1", "phone")
	c1 := newClientConn("u1", "cli-1")
	c2 := newClientConn("u1", "cli-2")
	mustRegister(t, h, c1)
	mustRegister(t, h, c2)
	mustRegister(t, h, d)

	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "device", "env": "test"}); got != 1 {
		t.Fatalf("relay_connections{role=device} = %v, want 1", got)
	}
	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "client", "env": "test"}); got != 2 {
		t.Fatalf("relay_connections{role=client} = %v, want 2", got)
	}
	if got := metricValue(t, reg, "relay_rooms_active", map[string]string{"env": "test"}); got != 1 {
		t.Fatalf("relay_rooms_active = %v, want 1", got)
	}

	// device 发 1 帧 app → in/device +1,两个 client 各 1 帧 → out/client +2
	h.RouteApp(d, "", json.RawMessage(`{"a":1}`))
	if got := metricValue(t, reg, "relay_frames_total", map[string]string{"direction": "in", "role": "device", "env": "test"}); got != 1 {
		t.Fatalf("frames in/device = %v, want 1", got)
	}
	if got := metricValue(t, reg, "relay_frames_total", map[string]string{"direction": "out", "role": "client", "env": "test"}); got != 2 {
		t.Fatalf("frames out/client = %v, want 2", got)
	}

	// client 发 1 帧到不存在的 device → in/client +1、undeliverable +1、out/device 0
	h.RouteApp(c1, "ghost", json.RawMessage(`{"a":2}`))
	if got := metricValue(t, reg, "relay_frames_total", map[string]string{"direction": "in", "role": "client", "env": "test"}); got != 1 {
		t.Fatalf("frames in/client = %v, want 1", got)
	}
	if got := metricValue(t, reg, "relay_undeliverable_total", map[string]string{"env": "test"}); got != 1 {
		t.Fatalf("undeliverable = %v, want 1", got)
	}
	if got := metricValue(t, reg, "relay_frames_total", map[string]string{"direction": "out", "role": "device", "env": "test"}); got != 0 {
		t.Fatalf("frames out/device = %v, want 0", got)
	}

	// 断开:connections 归零、时长直方图计数 +1、房间清空
	h.Unregister(d)
	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "device", "env": "test"}); got != 0 {
		t.Fatalf("relay_connections{role=device} after unregister = %v, want 0", got)
	}
	if got := histCount(t, reg, "relay_connection_duration_seconds", map[string]string{"role": "device", "env": "test"}); got != 1 {
		t.Fatalf("conn duration count{role=device} = %v, want 1", got)
	}
	h.Unregister(c1)
	h.Unregister(c2)
	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "client", "env": "test"}); got != 0 {
		t.Fatalf("relay_connections{role=client} after unregister = %v, want 0", got)
	}
	if got := histCount(t, reg, "relay_connection_duration_seconds", map[string]string{"role": "client", "env": "test"}); got != 2 {
		t.Fatalf("conn duration count{role=client} = %v, want 2", got)
	}
	if got := metricValue(t, reg, "relay_rooms_active", map[string]string{"env": "test"}); got != 0 {
		t.Fatalf("relay_rooms_active after all unregister = %v, want 0", got)
	}
}

// TestMetricsKickUnregistersOld 同 id 踢连:旧连的房间成员资格在替换时结束,
// connections gauge 不得泄漏(+1/-1 配平),旧连随后的 Unregister 不重复扣减。
func TestMetricsKickUnregistersOld(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, "test")
	h := NewHub(DefaultOptions(), m)

	d1 := newDeviceConn("u1", "dev-1", "old")
	d2 := newDeviceConn("u1", "dev-1", "new")
	mustRegister(t, h, d1)
	mustRegister(t, h, d2)

	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "device", "env": "test"}); got != 1 {
		t.Fatalf("relay_connections after kick = %v, want 1", got)
	}
	if got := histCount(t, reg, "relay_connection_duration_seconds", map[string]string{"role": "device", "env": "test"}); got != 1 {
		t.Fatalf("conn duration count after kick = %v, want 1 (kicked conn observed)", got)
	}

	// 旧连延迟 Unregister(removed=false)→ 不得再扣;新连 Unregister → 归零
	h.Unregister(d1)
	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "device", "env": "test"}); got != 1 {
		t.Fatalf("relay_connections after stale unregister = %v, want 1", got)
	}
	h.Unregister(d2)
	if got := metricValue(t, reg, "relay_connections", map[string]string{"role": "device", "env": "test"}); got != 0 {
		t.Fatalf("relay_connections final = %v, want 0", got)
	}
}

// TestMetricsNilReceiver NewHub(opts, nil) 下全部指标路径必须是 no-op
// (现有 hub/conn 测试均以此运行,此处显式回归)。
func TestMetricsNilReceiver(t *testing.T) {
	var m *Metrics
	m.connRegistered(RoleDevice)
	m.connUnregistered(RoleDevice, time.Second)
	m.roomsActiveSet(1)
	m.frameIn(RoleDevice)
	m.frameOut(RoleClient)
	m.undeliverableInc()
	m.helloFailed("auth")
	m.renewFailed()
	m.slowConsumerInc()
}

// TestMetricsWSFailureCounters WS 级:hello 失败按 reason 分类计数
// (timeout/auth/protocol),renew 校验失败计数。
func TestMetricsWSFailureCounters(t *testing.T) {
	opts := testOptions()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, "test")
	env := &wsTestEnv{
		hub:     NewHub(opts, m),
		tickets: newStubTickets(),
		fails:   NewHelloFailLimiter(10),
	}
	env.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HandleWS(w, r, env.hub, env.tickets, env.fails)
	}))
	t.Cleanup(func() {
		env.server.Close()
		env.fails.Stop()
	})

	t.Run("bad ticket", func(t *testing.T) {
		c := env.dial(t, "")
		defer c.CloseNow()
		writeFrame(t, c, map[string]any{
			"v": 1, "type": "hello", "ticket": "forged", "role": "device",
			"device_id": "d1", "device_name": "Mac", "app_version": "2.4.0",
		})
		expectClosedReason(t, c, "auth")
		waitMetric(t, reg, "relay_hello_failures_total", map[string]string{"reason": "auth", "env": "test"}, 1)
	})

	t.Run("bad json", func(t *testing.T) {
		c := env.dial(t, "")
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := c.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
			t.Fatalf("write: %v", err)
		}
		expectClosedReason(t, c, "protocol")
		waitMetric(t, reg, "relay_hello_failures_total", map[string]string{"reason": "protocol", "env": "test"}, 1)
	})

	t.Run("hello timeout", func(t *testing.T) {
		c := env.dial(t, "")
		defer c.CloseNow()
		expectConnClosed(t, c, 2*time.Second)
		waitMetric(t, reg, "relay_hello_failures_total", map[string]string{"reason": "timeout", "env": "test"}, 1)
	})

	t.Run("forged renew", func(t *testing.T) {
		ticket := env.tickets.issue("u-met", 30*time.Second)
		dev := env.dial(t, "")
		defer dev.CloseNow()
		helloDevice(t, dev, ticket, "d-met")
		writeFrame(t, dev, map[string]any{"v": 1, "type": "renew", "ticket": "forged"})
		expectClosedReason(t, dev, "auth")
		waitMetric(t, reg, "relay_renew_failures_total", map[string]string{"env": "test"}, 1)
	})
}

// TestUserHash 日志用 user 哈希:确定性、12 hex 字符、不含明文。
func TestUserHash(t *testing.T) {
	h1 := UserHash("user-123")
	if h1 != UserHash("user-123") {
		t.Fatal("UserHash not deterministic")
	}
	if len(h1) != 12 {
		t.Fatalf("UserHash len = %d, want 12", len(h1))
	}
	if h1 == UserHash("user-456") {
		t.Fatal("UserHash collides for different inputs")
	}
}
