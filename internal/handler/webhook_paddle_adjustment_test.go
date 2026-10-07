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

// Approved chargeback / chargeback_reverse adjustments must be
// distinguishable for ops alerting: parsed enough to carry the action,
// transaction id, amount and currency through — but NEVER onto the refund
// fields (they must not reach branchRefund / entitlement logic).
func TestParsePaddle_AdjustmentChargeback(t *testing.T) {
	h := &WebhookHandler{}
	for _, action := range []string{"chargeback", "chargeback_reverse"} {
		t.Run(action, func(t *testing.T) {
			raw := []byte(`{
			  "event_id": "evt_cb_` + action + `",
			  "event_type": "adjustment.updated",
			  "data": {
			    "id": "adj_cb_1",
			    "action": "` + action + `",
			    "status": "approved",
			    "transaction_id": "txn_cb_1",
			    "currency_code": "USD",
			    "totals": {"total": "1990", "subtotal": "1990", "tax": "0", "fee": "0"}
			  }
			}`)
			we, err := h.parsePaddle(raw)
			if err != nil {
				t.Fatalf("chargeback adjustment must parse: %v", err)
			}
			if we.AdjustmentAction != action {
				t.Errorf("AdjustmentAction = %q, want %q", we.AdjustmentAction, action)
			}
			if we.TransactionID != "txn_cb_1" || we.Amount != 19.90 || we.Currency != "USD" {
				t.Errorf("ops context not carried: %+v", we)
			}
			if we.ExternalRefundID != "" || we.RefundAmount != 0 {
				t.Errorf("chargeback must NOT populate refund fields: %+v", we)
			}
		})
	}
}

// An approved refund adjustment with missing or unparseable totals.total
// would otherwise reach onRefundSucceeded with RefundAmount=0 and violate
// refunds.amount CHECK (amount > 0) → 500 → Paddle retries forever.
// Hard-fail at parse, consistent with the data.id / data.transaction_id
// guards.
func TestParsePaddle_AdjustmentRefundBadTotals(t *testing.T) {
	h := &WebhookHandler{}
	cases := map[string]string{
		"missing totals":    `{"event_id":"evt_bt1","event_type":"adjustment.updated","data":{"id":"adj_bt1","action":"refund","status":"approved","transaction_id":"txn_1","currency_code":"USD"}}`,
		"empty total":       `{"event_id":"evt_bt2","event_type":"adjustment.updated","data":{"id":"adj_bt2","action":"refund","status":"approved","transaction_id":"txn_1","currency_code":"USD","totals":{"total":""}}}`,
		"unparseable total": `{"event_id":"evt_bt3","event_type":"adjustment.updated","data":{"id":"adj_bt3","action":"refund","status":"approved","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"abc"}}}`,
		"zero total":        `{"event_id":"evt_bt4","event_type":"adjustment.updated","data":{"id":"adj_bt4","action":"refund","status":"approved","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"0"}}}`,
		"negative total":    `{"event_id":"evt_bt5","event_type":"adjustment.updated","data":{"id":"adj_bt5","action":"refund","status":"approved","transaction_id":"txn_1","currency_code":"USD","totals":{"total":"-500"}}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.parsePaddle([]byte(raw)); err == nil {
				t.Fatal("approved refund adjustment with bad totals.total must error")
			}
		})
	}
}

// A chargeback with an unparseable totals.total still parses (amount is
// ops context, not a ledger key) — the amount just stays zero.
func TestParsePaddle_AdjustmentChargebackBadTotalsLenient(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_cb_bad",
	  "event_type": "adjustment.updated",
	  "data": {"id": "adj_cb_bad", "action": "chargeback", "status": "approved",
	           "transaction_id": "txn_cb_2", "currency_code": "USD",
	           "totals": {"total": "abc"}}
	}`)
	we, err := h.parsePaddle(raw)
	if err != nil {
		t.Fatalf("chargeback parse must stay lenient on totals: %v", err)
	}
	if we.AdjustmentAction != "chargeback" || we.Amount != 0 {
		t.Errorf("got %+v, want action=chargeback with amount 0", we)
	}
}

// Symmetric with the data.id guard: an approved refund without
// data.transaction_id can't key the payment lookup, and malformed
// adjustment data JSON hard-fails like every other branch's data.
func TestParsePaddle_AdjustmentRefundMissingTransactionID(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{
	  "event_id": "evt_adj_notxn",
	  "event_type": "adjustment.updated",
	  "data": {"id": "adj_1", "action": "refund", "status": "approved",
	           "transaction_id": "", "currency_code": "USD", "totals": {"total": "100"}}
	}`)
	if _, err := h.parsePaddle(raw); err == nil {
		t.Fatal("refund-approved adjustment with empty data.transaction_id must error")
	}
}

func TestParsePaddle_AdjustmentMalformedData(t *testing.T) {
	h := &WebhookHandler{}
	raw := []byte(`{"event_id":"evt_adj_broken","event_type":"adjustment.updated","data":{"id":`)
	if _, err := h.parsePaddle(raw); err == nil {
		t.Fatal("malformed adjustment data must error")
	}
}
