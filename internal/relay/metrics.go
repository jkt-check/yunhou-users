package relay

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics 汇总 relay 模块全部 Prometheus 收集器(spec §9)。指标名/label
// 逐字固定:relay_connections{role,env}、relay_rooms_active{env}、
// relay_frames_total{direction,role,env}、relay_undeliverable_total{env}、
// relay_hello_failures_total{reason,env}、relay_renew_failures_total{env}、
// relay_slow_consumer_closes_total{env}、
// relay_connection_duration_seconds{role,env}。
// env 以 ConstLabels 注入,方法签名不再重复传。全部方法 nil 接收者安全:
// NewHub(opts, nil) 时 Hub/conn 侧无需判空(测试便捷)。
type Metrics struct {
	env           string
	connections   *prometheus.GaugeVec     // relay_connections{role,env}
	roomsActive   prometheus.Gauge         // relay_rooms_active{env}
	framesTotal   *prometheus.CounterVec   // relay_frames_total{direction,role,env}
	undeliverable prometheus.Counter       // relay_undeliverable_total{env}
	helloFailures *prometheus.CounterVec   // relay_hello_failures_total{reason,env}
	renewFailures prometheus.Counter       // relay_renew_failures_total{env}
	slowConsumer  prometheus.Counter       // relay_slow_consumer_closes_total{env}
	connDuration  *prometheus.HistogramVec // relay_connection_duration_seconds{role,env}
}

func NewMetrics(reg prometheus.Registerer, env string) *Metrics {
	constLabels := prometheus.Labels{"env": env}
	m := &Metrics{
		env: env,
		connections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name:        "relay_connections",
			Help:        "Currently registered relay WebSocket connections.",
			ConstLabels: constLabels,
		}, []string{"role"}),
		roomsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        "relay_rooms_active",
			Help:        "Currently non-empty relay rooms.",
			ConstLabels: constLabels,
		}),
		framesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "relay_frames_total",
			Help:        "App frames routed by the relay hub.",
			ConstLabels: constLabels,
		}, []string{"direction", "role"}),
		undeliverable: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "relay_undeliverable_total",
			Help:        "Client app frames whose target device was unreachable.",
			ConstLabels: constLabels,
		}),
		helloFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "relay_hello_failures_total",
			Help:        "Failed hello handshakes by reason (timeout/auth/protocol).",
			ConstLabels: constLabels,
		}, []string{"reason"}),
		renewFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "relay_renew_failures_total",
			Help:        "Renew frames failing ticket validation.",
			ConstLabels: constLabels,
		}),
		slowConsumer: prometheus.NewCounter(prometheus.CounterOpts{
			Name:        "relay_slow_consumer_closes_total",
			Help:        "Connections closed for a full outbound buffer.",
			ConstLabels: constLabels,
		}),
		connDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:        "relay_connection_duration_seconds",
			Help:        "Lifetime of relay connections.",
			Buckets:     prometheus.ExponentialBuckets(1, 4, 8),
			ConstLabels: constLabels,
		}, []string{"role"}),
	}
	reg.MustRegister(
		m.connections, m.roomsActive, m.framesTotal, m.undeliverable,
		m.helloFailures, m.renewFailures, m.slowConsumer, m.connDuration,
	)
	return m
}

func (m *Metrics) connRegistered(role Role) {
	if m == nil {
		return
	}
	m.connections.WithLabelValues(string(role)).Inc()
}

func (m *Metrics) connUnregistered(role Role, alive time.Duration) {
	if m == nil {
		return
	}
	m.connections.WithLabelValues(string(role)).Dec()
	m.connDuration.WithLabelValues(string(role)).Observe(alive.Seconds())
}

func (m *Metrics) roomsActiveSet(n int) {
	if m == nil {
		return
	}
	m.roomsActive.Set(float64(n))
}

func (m *Metrics) frameIn(role Role) {
	if m == nil {
		return
	}
	m.framesTotal.WithLabelValues("in", string(role)).Inc()
}

func (m *Metrics) frameOut(role Role) {
	if m == nil {
		return
	}
	m.framesTotal.WithLabelValues("out", string(role)).Inc()
}

func (m *Metrics) undeliverableInc() {
	if m == nil {
		return
	}
	m.undeliverable.Inc()
}

func (m *Metrics) helloFailed(reason string) {
	if m == nil {
		return
	}
	m.helloFailures.WithLabelValues(reason).Inc()
}

func (m *Metrics) renewFailed() {
	if m == nil {
		return
	}
	m.renewFailures.Inc()
}

func (m *Metrics) slowConsumerInc() {
	if m == nil {
		return
	}
	m.slowConsumer.Inc()
}

// connectedSinceConn 由 *wsConn 实现,供 Hub 在 Unregister/踢连时观测真实
// 连接存活时长;测试 fakeConn 不实现,按 0 观测(仅影响测试进程)。
type connectedSinceConn interface{ ConnectedSince() time.Time }

func connAge(c Conn) time.Duration {
	if cs, ok := c.(connectedSinceConn); ok {
		return time.Since(cs.ConnectedSince())
	}
	return 0
}
