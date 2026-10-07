package service

import (
	"encoding/json"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// Pure-unit coverage top-ups for the M3 event helpers (no DB needed).

func TestBillingCycleFromInterval(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		days *int
		want string
	}{
		{"nil interval omits the key", nil, ""},
		{"zero omits", ptr(0), ""},
		{"ambiguous short omits", ptr(27), ""},
		{"28 days is monthly", ptr(28), "monthly"},
		{"30 days is monthly", ptr(30), "monthly"},
		{"31 days is monthly", ptr(31), "monthly"},
		{"32 days ambiguous omits", ptr(32), ""},
		{"359 ambiguous omits", ptr(359), ""},
		{"360 days is yearly", ptr(360), "yearly"},
		{"365 days is yearly", ptr(365), "yearly"},
		{"370 days is yearly", ptr(370), "yearly"},
		{"371 ambiguous omits", ptr(371), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := billingCycleFromInterval(tc.days); got != tc.want {
				t.Errorf("billingCycleFromInterval(%v) = %q, want %q", tc.days, got, tc.want)
			}
		})
	}
}

func ptr(n int) *int { return &n }

// The nil-analytics guard: unwired services must emit nothing and never
// panic (PaymentMetrics posture).
func TestEmitRefundCompleted_NilAnalytics(t *testing.T) {
	t.Parallel()
	svc := &PaymentService{}
	svc.emitRefundCompleted(
		&model.Payment{ID: "p-1", OrderID: "o-1", ExternalTxnID: "txn_1", Currency: "CNY"},
		&model.Order{ID: "o-1", UserID: "u-1"},
		"r-1", "adj_1", 5.0)
}

func TestMergeAttributionUTM(t *testing.T) {
	t.Parallel()
	raw := func(s string) *json.RawMessage { r := json.RawMessage(s); return &r }

	cases := []struct {
		name string
		attr *json.RawMessage
		want map[string]any // expected utm_* keys merged into custom_data
	}{
		{"nil attribution", nil, nil},
		{"empty bytes", ptrRawMessageIfNotEmpty(nil), nil},
		{"invalid json is a no-op", raw(`{`), nil},
		{"no last_touch", raw(`{"first_touch":{"utm_source":"first"}}`), nil},
		{"empty last_touch", raw(`{"last_touch":{}}`), nil},
		{"source only", raw(`{"last_touch":{"utm_source":"google"}}`), map[string]any{"utm_source": "google"}},
		{"campaign only", raw(`{"last_touch":{"utm_campaign":"spring"}}`), map[string]any{"utm_campaign": "spring"}},
		{"all three", raw(`{"last_touch":{"utm_source":"google","utm_medium":"cpc","utm_campaign":"spring"}}`),
			map[string]any{"utm_source": "google", "utm_medium": "cpc", "utm_campaign": "spring"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := map[string]any{"order_id": "o-1"}
			mergeAttributionUTM(data, tc.attr)
			if data["order_id"] != "o-1" {
				t.Errorf("order_id clobbered: %v", data)
			}
			delete(data, "order_id")
			if len(data) != len(tc.want) {
				t.Fatalf("merged keys = %v, want %v", data, tc.want)
			}
			for k, v := range tc.want {
				if data[k] != v {
					t.Errorf("custom_data[%q] = %v, want %v", k, data[k], v)
				}
			}
		})
	}
}
