package service

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// lateHonorMetricValue reads the current payment_late_payment_honored_total
// value for one channel label from a test-built PaymentMetrics.
func lateHonorMetricValue(t *testing.T, m *PaymentMetrics, channel string) float64 {
	t.Helper()
	c, err := m.latePaymentHonored.GetMetricWithLabelValues(channel)
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return pb.Counter.GetValue()
}

// Late honor of an expired order must bump
// payment_late_payment_honored_total{channel} exactly once; a normal
// on-time Confirm must not bump it at all.
func TestPaymentMetrics_LatePaymentHonored(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	m := NewPaymentMetrics(prometheus.NewRegistry(), "test")
	svc.SetMetrics(m)
	uid := seedUser(t, db)

	// On-time confirm: no late-honor signal.
	onTime, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	if _, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: onTime.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi_ontime_1",
	}); err != nil {
		t.Fatalf("Confirm on-time: %v", err)
	}
	if v := lateHonorMetricValue(t, m, "stripe"); v != 0 {
		t.Fatalf("on-time confirm bumped late counter: %v", v)
	}

	// Expired order honored late: counter increments once.
	late, _ := svc.CreateOrder(context.Background(), uid, "monthly", "stripe")
	_, _ = db.ExecContext(context.Background(),
		`UPDATE orders SET status = 'expired' WHERE id = $1`, late.ID)
	res, err := svc.Confirm(context.Background(), ConfirmInput{
		OrderID: late.ID, UserID: uid, Channel: "stripe", ExternalTxnID: "pi_late_metric_1",
	})
	if err != nil {
		t.Fatalf("Confirm late: %v", err)
	}
	if !res.WasLatePayment {
		t.Fatal("WasLatePayment = false, want true")
	}
	if v := lateHonorMetricValue(t, m, "stripe"); v != 1 {
		t.Fatalf("late counter = %v, want 1", v)
	}
}

// Nil-metrics service (unit-test default) must tolerate the late-honor path.
func TestPaymentMetrics_NilSafe(t *testing.T) {
	var m *PaymentMetrics
	m.LatePaymentHonored("stripe") // must not panic
}
