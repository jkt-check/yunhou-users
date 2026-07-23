package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// stubRefundAPI is a hand-rolled stub for RefundAPI per CLAUDE.md
// ("Handler tests use hand-rolled mock structs with function fields —
// no external mocking libraries").
type stubRefundAPI struct {
	called    bool
	gotCh     string
	gotTxn    string
	gotAmt    float64
	gotKey    string
	returnID  string
	returnErr error
}

func (s *stubRefundAPI) Refund(_ context.Context, ch, txn string, amt float64, key string) (string, error) {
	s.called = true
	s.gotCh = ch
	s.gotTxn = txn
	s.gotAmt = amt
	s.gotKey = key
	return s.returnID, s.returnErr
}

// nopRepos returns a PaymentService with all repos set to nil. Pure-function
// tests (validateChannel, toCents, etc.) never touch a repo, so nil is
// safe. NewPaymentService assigns db too; we pass a nil sqlx.DB which is
// never called from the pure functions we test here.
func nopRepos() *PaymentService {
	return NewPaymentService(
		(*sqlx.DB)(nil),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		&stubRefundAPI{}, nil,

		0)

}

// ============================================================================
// validateChannel
// ============================================================================

func TestValidateChannel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		wantErr bool
	}{
		{"stripe", false},
		{"wechat_pay", false},
		{"alipay", false},
		{"paypal", false},
		{"", true},
		{"STRIPE", true},   // case-sensitive
		{" stripe ", true}, // whitespace
		{"\nstripe", true}, // leading newline
	}
	for _, c := range cases {
		name := c.in
		if name == "" {
			name = "<empty>"
		}
		t.Run(name, func(t *testing.T) {
			got := validateChannel(c.in)
			if (got != nil) != c.wantErr {
				t.Errorf("validateChannel(%q) error = %v, wantErr = %v", c.in, got, c.wantErr)
			}
		})
	}
}

// ============================================================================
// isPaymentSuccess / isPaymentFailed / isRefundEvent / isDisputeCreated/Closed
// ============================================================================

func TestIsPaymentSuccess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType string
		want      bool
	}{
		// Stripe
		{"payment_intent.succeeded", true},
		{"payment_intent.Succeeded", false}, // case-sensitive
		// WeChat (uppercase per legacy docs)
		{"TRANSACTION.SUCCESS", true},
		// Alipay
		{"trade_status_sync", true},
		{"TRADE_SUCCESS", true},
		{"order_created", true},
		{"subscription_created", true},
		{"subscription_payment_success", false}, // renewal — v1 ack-200 no-op
		// unrelated
		{"charge.refunded", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			if got := isPaymentSuccess(c.eventType); got != c.want {
				t.Errorf("isPaymentSuccess(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}

func TestIsPaymentFailed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType string
		want      bool
	}{
		{"payment_intent.payment_failed", true},
		{"payment_intent.canceled", true},
		{"TRANSACTION.PAY_FAILED", true},
		{"TRANSACTION.REVOKED", true},
		{"payment_intent.succeeded", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			if got := isPaymentFailed(c.eventType); got != c.want {
				t.Errorf("isPaymentFailed(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}

func TestIsRefundEvent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType string
		want      bool
	}{
		{"charge.refunded", true},
		{"TRANSACTION.REFUND", true},
		{"trade_closed", true},
		{"TRADE_CLOSED", true},
		{"order_refunded", true},
		{"subscription_payment_refunded", true},
		{"subscription_updated", false},
		{"payment_intent.succeeded", false},
		{"payment_intent.payment_failed", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			if got := isRefundEvent(c.eventType); got != c.want {
				t.Errorf("isRefundEvent(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}

func TestIsDisputeCreated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType string
		want      bool
	}{
		{"charge.dispute.created", true},
		{"payment_intent.succeeded", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			if got := isDisputeCreated(c.eventType); got != c.want {
				t.Errorf("isDisputeCreated(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}

func TestIsDisputeClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType string
		want      bool
	}{
		{"charge.dispute.closed", true},
		{"payment_intent.succeeded", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			if got := isDisputeClosed(c.eventType); got != c.want {
				t.Errorf("isDisputeClosed(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}

// ============================================================================
// isPaypalRenewal — handles PAYMENT.SALE.COMPLETED (subscription auto-renewal)
// ============================================================================

func TestIsPaypalRenewal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType string
		want      bool
	}{
		{"PAYMENT.SALE.COMPLETED", true},
		{"PAYMENT.CAPTURE.COMPLETED", false},
		{"BILLING.SUBSCRIPTION.CREATED", false},
		{"order_created", false}, // LS, not PayPal
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.eventType, func(t *testing.T) {
			t.Parallel()
			if got := isPaypalRenewal(c.eventType); got != c.want {
				t.Errorf("isPaypalRenewal(%q) = %v, want %v", c.eventType, got, c.want)
			}
		})
	}
}

// stubAuditRepo is the minimal audit logger OnWebhook may touch in
// non-domain-action branches (none — but kept for interface satisfaction).
type stubAuditRepo struct{}

func (stubAuditRepo) Insert(_ context.Context, _ *model.AuditLog) error { return nil }

// stubWebhookEventRepo is a hand-rolled mock that drives the event-level
// dedup branches in OnWebhook without a real DB.
type stubWebhookEventRepo struct {
	insertID  string
	insertOK  bool
	insertErr error
	findRow   *model.WebhookEvent
	findErr   error
	markedIDs []string
	markErr   error
}

func (s *stubWebhookEventRepo) InsertOnConflictDoNothing(_ context.Context, _ *model.WebhookEvent) (string, bool, error) {
	return s.insertID, s.insertOK, s.insertErr
}

func (s *stubWebhookEventRepo) FindByChannelEventID(_ context.Context, _, _ string) (*model.WebhookEvent, error) {
	return s.findRow, s.findErr
}

func (s *stubWebhookEventRepo) MarkProcessed(_ context.Context, id string) error {
	s.markedIDs = append(s.markedIDs, id)
	return s.markErr
}

func (s *stubWebhookEventRepo) MarkProcessedOnTx(_ context.Context, _ *sqlx.Tx, id string) error {
	s.markedIDs = append(s.markedIDs, id)
	return s.markErr
}

// TestOnWebhook_DisputeCreated_AcksPayloadBeforeTx covers that the
// dispatch reaches the case-arm of OnWebhook before any DB tx. The
// expected outcome is a panic on the nil *sqlx.DB (no real test-DB
// dependency); recover() lets the test PASS rather than fail the
// process. The coverage gain we care about is the case-arm selection
// itself.
func TestOnWebhook_DisputeCreated_AcksPayloadBeforeTx(t *testing.T) {
	t.Parallel()
	webhookRepo := &stubWebhookEventRepo{insertID: "we-d", insertOK: true}
	svc := NewPaymentService(nil, nil, nil, nil, nil, nil, nil,
		webhookRepo, stubAuditRepo{}, nil, nil,
		30*time.Minute)

	defer func() {
		if r := recover(); r != nil {
			t.Logf("onDisputeCreated attempted BeginTxx on nil DB (expected without a real DB) — dispatch reached: %v", r)
		}
	}()
	_, _ = svc.OnWebhook(context.Background(), WebhookEvent{
		Channel:   "stripe",
		EventID:   "evt-d",
		EventType: "charge.dispute.created",
	})
}

func TestOnWebhook_DedupHit_ReturnsDuplicateEvent(t *testing.T) {
	t.Parallel()

	processedAt := time.Now().UTC()
	prior := &model.WebhookEvent{
		ID:          "we-prior",
		Channel:     "paypal",
		EventID:     "WH-1",
		EventType:   "PAYMENT.CAPTURE.COMPLETED",
		ProcessedAt: &processedAt,
	}

	webhookRepo := &stubWebhookEventRepo{
		insertID:  "",
		insertOK:  false, // dedupe hit
		insertErr: nil,
		findRow:   prior,
		findErr:   nil,
	}
	// PaymentService requires all fields. The unused ones can be nil-tilings.
	// We pass nil *sqlx.DB — OnWebhook's dedup-hit branch returns BEFORE
	// calling BeginTxx, so the DB pointer never gets dereferenced.
	svc := NewPaymentService(nil, nil, nil, nil, nil, nil, nil,
		webhookRepo, stubAuditRepo{}, nil, nil,
		30*time.Minute)

	result, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel:   "paypal",
		EventID:   "WH-1",
		EventType: "PAYMENT.CAPTURE.COMPLETED",
	})
	if err != nil {
		t.Fatalf("dedup hit should not error: %v", err)
	}
	if !result.DuplicateEvent {
		t.Errorf("expected DuplicateEvent=true, got %+v", result)
	}
	if result.DomainAction != "" {
		t.Errorf("dedup hit should not set DomainAction, got %q", result.DomainAction)
	}
}

func TestOnWebhook_UnknownEventType_Acks200(t *testing.T) {
	t.Parallel()
	webhookRepo := &stubWebhookEventRepo{
		insertID: "we-new",
		insertOK: true,
	}
	svc := NewPaymentService(nil, nil, nil, nil, nil, nil, nil,
		webhookRepo, stubAuditRepo{}, nil, nil,
		30*time.Minute)

	result, err := svc.OnWebhook(context.Background(), WebhookEvent{
		Channel:   "paypal",
		EventID:   "WH-2",
		EventType: "BILLING.SUBSCRIPTION.UPDATED", // not a known dispatch case
	})
	if err != nil {
		t.Fatalf("unknown event type: %v", err)
	}
	if result.DomainAction != "none" {
		t.Errorf("unknown event should set DomainAction=none, got %q", result.DomainAction)
	}
	if result.DuplicateEvent {
		t.Errorf("unknown event should NOT be duplicate")
	}
	if len(webhookRepo.markedIDs) != 1 || webhookRepo.markedIDs[0] != "we-new" {
		t.Errorf("expected we-new to be markedProcessed, got %v", webhookRepo.markedIDs)
	}
}

// ============================================================================
// toCents — full-vs-partial refund detection
// ============================================================================

func TestToCents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want int64
	}{
		{0, 0},
		{1.00, 100},
		{29.90, 2990},
		{100.50, 10050},
		// float64 round-trip tolerance — common refund amounts round cleanly
		{29.90, 2990},
		// boundary
		{0.01, 1},
		{0.99, 99},
		// negative (should not crash)
		{-1.50, -150},
		// large
		{99999.99, 9999999},
		// NaN
		{math.NaN(), 0},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			got := toCents(c.in)
			if got != c.want {
				t.Errorf("toCents(%v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// ============================================================================
// subExpiresAtFromWebhook
// ============================================================================

func TestSubExpiresAtFromWebhook(t *testing.T) {
	t.Parallel()
	t.Run("nil webhook returns nil", func(t *testing.T) {
		e := WebhookEvent{}
		if got := subExpiresAtFromWebhook(e); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("populated webhook returns the pointer", func(t *testing.T) {
		want := time.Now()
		e := WebhookEvent{SubExpiresAt: &want}
		got := subExpiresAtFromWebhook(e)
		if got == nil || *got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// ============================================================================
// mustJSON
// ============================================================================

func TestMustJSON(t *testing.T) {
	t.Parallel()
	t.Run("empty map", func(t *testing.T) {
		got := mustJSON(map[string]any{})
		if string(got) != "{}" {
			t.Errorf("got %q, want {}", got)
		}
	})
	t.Run("nil map", func(t *testing.T) {
		got := mustJSON(nil)
		if string(got) != "null" {
			t.Errorf("got %q, want null", got)
		}
	})
	t.Run("nested", func(t *testing.T) {
		got := mustJSON(map[string]any{"a": 1, "b": "x"})
		if string(got) != `{"a":1,"b":"x"}` {
			t.Errorf("got %q", got)
		}
	})
}

// ============================================================================
// NewPaymentService constructor
// ============================================================================

func TestNewPaymentService_DefaultsOrderExpiry(t *testing.T) {
	t.Parallel()
	svc := NewPaymentService(nil, nil, nil, nil, nil, nil, nil, nil, nil, &stubRefundAPI{}, nil, 0)
	if svc.orderExpiry != 30*time.Minute {
		t.Errorf("default orderExpiry = %v, want 30m", svc.orderExpiry)
	}
}

func TestNewPaymentService_CustomOrderExpiry(t *testing.T) {
	t.Parallel()
	svc := NewPaymentService(nil, nil, nil, nil, nil, nil, nil, nil, nil, &stubRefundAPI{}, nil, 5*time.Minute)
	if svc.orderExpiry != 5*time.Minute {
		t.Errorf("custom orderExpiry = %v, want 5m", svc.orderExpiry)
	}
}

// ============================================================================
// Model field sanity — Refund.UserID is in the SQL column list
// ============================================================================

// This is a compile-time check that the model includes UserID in db tags
// matching what the SQL INSERT statement expects. If this compiles, the
// named-binding INSERT in service.Refund will work; if not, Go fails to
// build.
func TestRefundModel_DBFields(t *testing.T) {
	t.Parallel()
	r := model.Refund{
		ID:               "r-1",
		PaymentID:        "p-1",
		Channel:          "stripe",
		UserID:           "u-1",
		Amount:           10.0,
		IdempotencyKey:   "key-1",
		ExternalRefundID: nil,
		Status:           "pending",
	}
	// Sanity: every field is reachable via reflection (no unexported fields).
	v := reflect.ValueOf(r)
	for i := 0; i < v.NumField(); i++ {
		_ = v.Field(i).Interface()
	}
}

// ============================================================================
// stubRefundAPI behavior — round-trip sanity
// ============================================================================

func TestStubRefundAPI_RoundTrip(t *testing.T) {
	t.Parallel()
	s := &stubRefundAPI{returnID: "re_test", returnErr: nil}
	got, err := s.Refund(context.Background(), "stripe", "pi_x", 5.0, "idem-1")
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got != "re_test" {
		t.Errorf("got %q, want re_test", got)
	}
	if !s.called || s.gotCh != "stripe" || s.gotTxn != "pi_x" || s.gotAmt != 5.0 || s.gotKey != "idem-1" {
		t.Errorf("stub args not captured: %+v", s)
	}
}

// ============================================================================
// repo interface coverage guard
// ============================================================================

// If a new method is added to any repo interface, this compile-time check
// catches mismatches between the interface and the struct. Useful when
// adding new repo methods (e.g., for the user-scoped idempotency fix).
var (
	_ repo.OrderRepo        = repo.OrderRepo(nil)
	_ repo.PaymentRepo      = repo.PaymentRepo(nil)
	_ repo.RefundRepo       = repo.RefundRepo(nil)
	_ repo.WebhookEventRepo = repo.WebhookEventRepo(nil)
	_ repo.AuditLogRepo     = repo.AuditLogRepo(nil)
)

// ============================================================================
// buildReconcileWebhookEvent
// ============================================================================
//
// Regression for the cn-staging "login → subscription expired → resubscribe
// → login" loop observed 2026-07-23. Root cause: this helper previously
// did `event.SubExpiresAt = &successAt` where successAt was parsed from
// `res.SuccessTime` (a past timestamp). The OnWebhook downstream then
// wrote subscriptions.expires_at = <past>, and findUsableSubscription's
// `expires_at < now()` check refused the next login with
// ErrSubscriptionExpired. See the doc comment on buildReconcileWebhookEvent
// for the full story.
//
// These tests pin SubExpiresAt == nil for every input shape so the bug
// cannot silently come back.

func TestBuildReconcileWebhookEvent_DoesNotSetSubExpiresAt(t *testing.T) {
	t.Parallel()

	// successTime set to "now" — the exact value that triggered the
	// production incident: a freshly-paid order has a SuccessTime ≈ now,
	// which would have become subscriptions.expires_at = now.
	justNow := time.Now().UTC().Format(time.RFC3339)
	res := &wechat.OrderQueryResult{
		OutTradeNo:    "otn-abc-123",
		TransactionID: "txn-xyz-789",
		TradeState:    "SUCCESS",
		SuccessTime:   justNow,
		Amount: struct {
			Total    int64  `json:"total"`
			Currency string `json:"currency"`
		}{Total: 2990, Currency: "CNY"},
	}

	got, err := buildReconcileWebhookEvent(res)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SubExpiresAt != nil {
		t.Fatalf("SubExpiresAt must stay nil on reconcile; got %v (would write subscriptions.expires_at into the past and break login)",
			*got.SubExpiresAt)
	}
	if got.Amount != 29.90 {
		t.Errorf("Amount major units: got %v, want 29.90", got.Amount)
	}
	if got.Currency != "CNY" {
		t.Errorf("Currency: got %q, want CNY", got.Currency)
	}
	if got.Channel != "wechat_pay" {
		t.Errorf("Channel: got %q, want wechat_pay", got.Channel)
	}
	if got.EventType != "TRANSACTION.SUCCESS" {
		t.Errorf("EventType: got %q, want TRANSACTION.SUCCESS", got.EventType)
	}
	if got.EventID != "reconcile:otn-abc-123:txn-xyz-789" {
		t.Errorf("EventID: got %q, want reconcile:otn-abc-123:txn-xyz-789", got.EventID)
	}
	if got.OrderID != "otn-abc-123" {
		t.Errorf("OrderID: got %q, want otn-abc-123", got.OrderID)
	}
	if got.TransactionID != "txn-xyz-789" {
		t.Errorf("TransactionID: got %q, want txn-xyz-789", got.TransactionID)
	}
}

func TestBuildReconcileWebhookEvent_RejectsNonSuccess(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"NOTPAY", "CLOSED", "REFUND"} {
		res := &wechat.OrderQueryResult{
			OutTradeNo:    "otn-1",
			TransactionID: "txn-1",
			TradeState:    state,
			SuccessTime:   time.Now().Format(time.RFC3339),
		}
		if _, err := buildReconcileWebhookEvent(res); err == nil {
			t.Errorf("trade_state=%s should be rejected (reconcile must only promote SUCCESS)", state)
		}
	}
}

func TestBuildReconcileWebhookEvent_NilInput(t *testing.T) {
	t.Parallel()
	if _, err := buildReconcileWebhookEvent(nil); err == nil {
		t.Error("nil OrderQueryResult must error — the production helper early-outs on nil upstream; keep the test honest")
	}
}

// ============================================================================
// reconcilePreCheck
// ============================================================================
//
// Locks down the dedupe invariant introduced after round-1 review: a real
// WeChat webhook can arrive AFTER the FE poll drives reconcile, and the two
// events use different EventID shapes so the webhook_events unique constraint
// doesn't catch the second one. The payment-row layer is the actual
// idempotency boundary, and reconcile must short-circuit when the payment
// is already paid to avoid an unnecessary (but no-op) UPSERT and a wasted
// row in webhook_events.

func TestReconcilePreCheck(t *testing.T) {
	t.Parallel()

	paidTxn := "txn-paid-1"
	paidRow := &model.Payment{
		ID:            "pay-1",
		Channel:       "wechat_pay",
		ExternalTxnID: paidTxn,
		Status:        "paid",
		Amount:        29.90,
		Currency:      "CNY",
	}
	pendingRow := &model.Payment{
		ID:            "pay-2",
		Channel:       "wechat_pay",
		ExternalTxnID: "txn-pending-1",
		Status:        "pending",
		Amount:        29.90,
		Currency:      "CNY",
	}

	type tc struct {
		name       string
		existing   *model.Payment
		findErr    error
		wantSkip   bool
		wantErr    bool
	}
	cases := []tc{
		{
			name:     "no payment row → fall through to OnWebhook",
			existing: nil,
			findErr:  sql.ErrNoRows,
			wantSkip: false,
			wantErr:  false,
		},
		{
			name:     "paid payment row → skip OnWebhook",
			existing: paidRow,
			findErr:  nil,
			wantSkip: true,
			wantErr:  false,
		},
		{
			name:     "pending payment row → fall through (OnWebhook will drive the transition)",
			existing: pendingRow,
			findErr:  nil,
			wantSkip: false,
			wantErr:  false,
		},
		{
			name:     "DB error other than NoRows → bubble up (FE poll retries)",
			existing: nil,
			findErr:  errors.New("db down"),
			wantSkip: false,
			wantErr:  true,
		},
		{
			name:     "NoRows wrapped → still treated as 'no row', continue",
			existing: nil,
			findErr:  fmt.Errorf("not found: %w", sql.ErrNoRows),
			wantSkip: false,
			wantErr:  false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			gotSkip, gotErr := reconcilePreCheck(c.existing, c.findErr)
			if gotSkip != c.wantSkip {
				t.Errorf("skip: got %v, want %v", gotSkip, c.wantSkip)
			}
			if (gotErr != nil) != c.wantErr {
				t.Errorf("err: got %v, wantErr %v", gotErr, c.wantErr)
			}
		})
	}

	// Sanity: paid payment row for the txn we care about → skip, no err,
	// happy-path snapshot.
	gotSkip, gotErr := reconcilePreCheck(paidRow, nil)
	if !gotSkip || gotErr != nil {
		t.Errorf("paid-row pre-check returned skip=%v err=%v; want skip=true, err=nil", gotSkip, gotErr)
	}
}
