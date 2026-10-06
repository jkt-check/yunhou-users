package handler

import (
	"testing"
)

// Paddle fires adjustment.updated when an adjustment's status changes;
// a completed refund arrives as action="refund" + status="approved"
// (including console-initiated manual refunds). The data shape mirrors
// transaction totals: minor-unit strings under data.totals. NOTE: the
// repo has no vendored adjustment fixtures — this payload shape is taken
// from the Paddle Billing webhook docs (adjustment object, 2026-10).
func TestParsePaddle_AdjustmentRefundApproved(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_adj_1",
	  "event_type": "adjustment.updated",
	  "occurred_at": "2026-10-06T09:00:00Z",
	  "data": {
	    "id": "adj_01m3x",
	    "action": "refund",
	    "status": "approved",
	    "transaction_id": "txn_01m3x",
	    "currency_code": "USD",
	    "totals": {"total": "1990", "subtotal": "1990", "tax": "0", "fee": "0"}
	  }
	}`)
	we, err := h.parsePaddle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if we.Channel != "paddle" || we.EventID != "evt_adj_1" || we.EventType != "adjustment.updated" {
		t.Fatalf("bad envelope: %+v", we)
	}
	if we.ExternalRefundID != "adj_01m3x" {
		t.Errorf("ExternalRefundID = %q, want adj_01m3x (adjustment id keys the refund row)", we.ExternalRefundID)
	}
	if we.TransactionID != "txn_01m3x" {
		t.Errorf("TransactionID = %q, want txn_01m3x", we.TransactionID)
	}
	if we.RefundAmount != 19.90 {
		t.Errorf("RefundAmount = %v, want 19.90 (minor → major)", we.RefundAmount)
	}
	if we.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", we.Currency)
	}
}

// Non-refund / non-approved adjustments stay audit-only: the parser must
// NOT populate the refund fields, which is what keeps them off
// branchRefund in resolveBranch.
func TestParsePaddle_AdjustmentNonRefundStaysBare(t *testing.T) {
	h := &WebhookHandler{}
	cases := map[string]string{
		"credit adjustment":  `{"event_id":"evt_a2","event_type":"adjustment.updated","data":{"id":"adj_cr1","action":"credit","status":"approved","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"500"}}}`,
		"pending approval":   `{"event_id":"evt_a3","event_type":"adjustment.updated","data":{"id":"adj_p1","action":"refund","status":"pending_approval","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"500"}}}`,
		"rejected refund":    `{"event_id":"evt_a4","event_type":"adjustment.updated","data":{"id":"adj_r1","action":"refund","status":"rejected","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"500"}}}`,
		"adjustment.created": `{"event_id":"evt_a5","event_type":"adjustment.created","data":{"id":"adj_c1","action":"refund","status":"pending_approval","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"500"}}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			we, err := h.parsePaddle([]byte(raw))
			if err != nil {
				t.Fatalf("parse must stay lenient for audit-only adjustments: %v", err)
			}
			if we.ExternalRefundID != "" || we.RefundAmount != 0 {
				t.Errorf("non-refund adjustment populated refund fields: %+v", we)
			}
		})
	}
}

// A refund-approved adjustment without data.id must hard-fail: an empty
// ExternalRefundID would collapse refunds.(channel, external_refund_id)
// dedupe onto one row for every malformed event (same invariant as the
// transaction missing-data.id guard).
func TestParsePaddle_AdjustmentRefundMissingID(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_adj_bad",
	  "event_type": "adjustment.updated",
	  "data": {"id": "", "action": "refund", "status": "approved",
	           "transaction_id": "txn_01m3x", "currency_code": "USD",
	           "totals": {"total": "100"}}
	}`)
	if _, err := h.parsePaddle(raw); err == nil {
		t.Fatal("refund-approved adjustment with empty data.id must error")
	}
}
