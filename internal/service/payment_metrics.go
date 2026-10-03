package service

import (
	"github.com/prometheus/client_golang/prometheus"
)

// PaymentMetrics 汇总 payment 模块的 Prometheus 收集器。指标名/label
// 逐字固定:payment_late_payment_honored_total{channel,env}。env 以
// ConstLabels 注入。全部方法 nil 接收者安全:NewPaymentService 不
// SetMetrics 时(单元测试)无需判空。
type PaymentMetrics struct {
	latePaymentHonored *prometheus.CounterVec // payment_late_payment_honored_total{channel,env}
}

func NewPaymentMetrics(reg prometheus.Registerer, env string) *PaymentMetrics {
	m := &PaymentMetrics{
		latePaymentHonored: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name:        "payment_late_payment_honored_total",
			Help:        "Orders honored (marked paid) after their expiry — the channel settled late. A rising rate surfaces 'success page showed failure but the payment went through' complaints before they arrive.",
			ConstLabels: prometheus.Labels{"env": env},
		}, []string{"channel"}),
	}
	if reg != nil {
		reg.MustRegister(m.latePaymentHonored)
	}
	return m
}

// LatePaymentHonored increments the late-honor counter for a channel.
// Nil-receiver safe.
func (m *PaymentMetrics) LatePaymentHonored(channel string) {
	if m == nil {
		return
	}
	m.latePaymentHonored.WithLabelValues(channel).Inc()
}
