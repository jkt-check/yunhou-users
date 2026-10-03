package service

import (
	"log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yunhou/users/internal/model"
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

// noteLatePaymentHonored 是"过期订单仍被兑付"的统一观测点:结构化日志 +
// payment_late_payment_honored_total{channel}。Confirm(用户主动确认)与
// onPaymentSucceeded(渠道 webhook)两条兑付路径共用——微信以外的渠道
// Confirm 一律被 ErrConfirmVerificationUnavailable 拒,late honor 全部走
// webhook,只挂 Confirm 会让指标对 paddle/stripe/paypal/alipay 恒为 0。
// 须在事务 commit 之后调用(兑付真正生效才计数)。
func (s *PaymentService) noteLatePaymentHonored(order *model.Order, paymentID, channel string) {
	log.Printf("late payment honored post-expiry: order=%s payment=%s channel=%s user=%s amount=%.2f %s",
		order.ID, paymentID, channel, order.UserID, order.Amount, order.Currency)
	s.metrics.LatePaymentHonored(channel)
}
