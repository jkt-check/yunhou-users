package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// mockWebhookSvc captures the WebhookEvent the handler passes and returns
// whatever the test configured. Only OnWebhook has a meaningful impl here —
// the rest satisfy the interface but are never called by webhook tests.
type mockWebhookSvc struct {
	gotEvent *service.WebhookEvent
	result   *service.OnWebhookResult
	err      error
}

func (m *mockWebhookSvc) CreateOrder(_ context.Context, _, _ string) (*model.Order, error) {
	return nil, nil
}
func (m *mockWebhookSvc) CancelOrder(_ context.Context, _, _ string) error { return nil }
func (m *mockWebhookSvc) Confirm(_ context.Context, _ service.ConfirmInput) (*service.ConfirmResult, error) {
	return nil, nil
}
func (m *mockWebhookSvc) Refund(_ context.Context, _ service.RefundInput) (*service.RefundResult, error) {
	return nil, nil
}
func (m *mockWebhookSvc) OnWebhook(_ context.Context, e service.WebhookEvent) (*service.OnWebhookResult, error) {
	m.gotEvent = &e
	return m.result, m.err
}
func (m *mockWebhookSvc) GetOrder(_ context.Context, _, _ string) (*model.Order, error) {
	return nil, nil
}
func (m *mockWebhookSvc) ListUserPayments(_ context.Context, _ string) ([]model.Payment, error) {
	return nil, nil
}
func (m *mockWebhookSvc) GetPayment(_ context.Context, _, _ string) (*model.Payment, error) {
	return nil, nil
}
func (m *mockWebhookSvc) ListPaymentRefunds(_ context.Context, _, _ string) ([]model.Refund, error) {
	return nil, nil
}
func (m *mockWebhookSvc) GetRefund(_ context.Context, _, _ string) (*model.Refund, error) {
	return nil, nil
}

func webhookTestEngine(svc service.PaymentServiceInterface) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	verifier := &middleware.WeChatPayV3Verifier{
		APIv3Key:     []byte("01234567890123456789012345678901"), // 32-byte test key
		ReplayWindow: 5 * time.Minute,
	}
	h := NewWebhookHandler(svc, []byte("01234567890123456789012345678901"), verifier)
	engine.POST("/webhooks/payment/:channel", h.Handle)
	return engine
}

// postRaw simulates a signed webhook delivery (signature middleware is NOT in
// this test — it's tested separately in middleware/webhook_sig_test.go).
func postRaw(engine *gin.Engine, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// Stripe parsing
// ============================================================================

// TestWebhookHandler_Stripe_SubExpiresAt asserts the parser populates
// SubExpiresAt from Stripe metadata.sub_expires_at (RFC3339). Without
// this, monthly/quarterly/yearly Stripe-activated subs never expire.
func TestWebhookHandler_Stripe_SubExpiresAt(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "evt_sub",
		"type": "payment_intent.succeeded",
		"data": {"object": {
			"id": "pi_sub",
			"amount": 2990,
			"currency": "cny",
			"metadata": {"order_id": "order-uuid-2", "sub_expires_at": "2027-01-15T00:00:00Z"}
		}}
	}`)
	rec := postRaw(engine, "/webhooks/payment/stripe", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent == nil {
		t.Fatal("expected OnWebhook to be called")
	}
	if svc.gotEvent.SubExpiresAt == nil {
		t.Fatal("SubExpiresAt should be populated from metadata.sub_expires_at")
	}
	want := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	if !svc.gotEvent.SubExpiresAt.Equal(want) {
		t.Errorf("SubExpiresAt: got %v, want %v", svc.gotEvent.SubExpiresAt, want)
	}
}

// TestWebhookHandler_Stripe_EnvelopeCompliance asserts the 200 response
// puts `domain_action` and `duplicate` INSIDE `data` per the CLAUDE.md
// envelope (no top-level keys).
func TestWebhookHandler_Stripe_EnvelopeCompliance(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid", DuplicateEvent: false},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{"id":"evt_env","type":"payment_intent.succeeded","data":{"object":{"id":"pi_env","amount":100,"currency":"cny","metadata":{"order_id":"o-env"}}}}`)
	rec := postRaw(engine, "/webhooks/payment/stripe", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	// Unmarshal into a generic map to detect any top-level keys that
	// shouldn't be there per the CLAUDE.md envelope.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, rec.Body.String())
	}
	for _, allowed := range []string{"code", "data"} {
		if _, ok := raw[allowed]; !ok {
			t.Errorf("envelope key %q missing in body: %s", allowed, rec.Body.String())
		}
	}
	// `message` is allowed to be absent on success (it's set on error
	// envelopes only). Don't assert it must be present.
	for _, banned := range []string{"domain_action", "duplicate", "received"} {
		if _, ok := raw[banned]; ok {
			t.Errorf("envelope key %q must be NESTED in data, not top-level: %s", banned, rec.Body.String())
		}
	}
	// And confirm the nested keys are there.
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw["data"], &data); err != nil {
		t.Fatalf("data unmarshal: %v", err)
	}
	for _, nested := range []string{"received", "domain_action", "duplicate"} {
		if _, ok := data[nested]; !ok {
			t.Errorf("data.%s missing: %s", nested, rec.Body.String())
		}
	}
}

func TestWebhookHandler_Stripe_Success(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "evt_123",
		"type": "payment_intent.succeeded",
		"data": {"object": {
			"id": "pi_abc",
			"amount": 2990,
			"currency": "cny",
			"metadata": {"order_id": "order-uuid-1"}
		}}
	}`)
	rec := postRaw(engine, "/webhooks/payment/stripe", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent == nil {
		t.Fatal("expected OnWebhook to be called")
	}
	if svc.gotEvent.EventID != "evt_123" {
		t.Errorf("event_id: got %q, want evt_123", svc.gotEvent.EventID)
	}
	if svc.gotEvent.TransactionID != "pi_abc" {
		t.Errorf("transaction_id: got %q, want pi_abc", svc.gotEvent.TransactionID)
	}
	if svc.gotEvent.OrderID != "order-uuid-1" {
		t.Errorf("order_id: got %q", svc.gotEvent.OrderID)
	}
	// Stripe sends amounts in cents (2990); handler divides by 100.
	if svc.gotEvent.Amount != 29.90 {
		t.Errorf("amount: got %v, want 29.90", svc.gotEvent.Amount)
	}
	if svc.gotEvent.Currency != "CNY" {
		t.Errorf("currency: got %q, want CNY", svc.gotEvent.Currency)
	}
}

func TestWebhookHandler_Stripe_BadJSON(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)
	rec := postRaw(engine, "/webhooks/payment/stripe", []byte(`{not-json}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// ============================================================================
// Alipay parsing
// ============================================================================

func TestWebhookHandler_Alipay_Success(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte("out_trade_no=order-uuid-1&trade_no=2023110&total_amount=29.90&notify_id=n_1&notify_type=trade_status_sync&sign=xx")
	rec := postRaw(engine, "/webhooks/payment/alipay", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent.OrderID != "order-uuid-1" {
		t.Errorf("order_id: got %q", svc.gotEvent.OrderID)
	}
	if svc.gotEvent.TransactionID != "2023110" {
		t.Errorf("transaction_id: got %q", svc.gotEvent.TransactionID)
	}
	if svc.gotEvent.Amount != 29.90 {
		t.Errorf("amount: got %v", svc.gotEvent.Amount)
	}
}

func TestWebhookHandler_Alipay_RefundEvent_DerivesExternalRefundID(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "refund_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte("out_trade_no=order-uuid-1&trade_no=2023110&total_amount=29.90&refund_amount=29.90&notify_id=n_2&notify_type=trade_closed&sign=xx")
	rec := postRaw(engine, "/webhooks/payment/alipay", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if svc.gotEvent.ExternalRefundID != "alipay-n_2" {
		t.Errorf("external_refund_id: got %q, want alipay-n_2", svc.gotEvent.ExternalRefundID)
	}
	if svc.gotEvent.RefundAmount != 29.90 {
		t.Errorf("refund_amount: got %v, want 29.90", svc.gotEvent.RefundAmount)
	}
}

// ============================================================================
// LemonSqueezy parsing — JSON:API with synthesized event_id, custom_data
// ============================================================================

func TestWebhookHandler_LS_OrderCreated_PaymentSucceeded(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"meta": {
			"event_name": "order_created",
			"custom_data": { "order_id": "order-uuid-ls-1" }
		},
		"data": {
			"type": "orders",
			"id": "42",
			"attributes": { "total": 2990, "currency": "usd" }
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/lemonsqueezy", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent == nil {
		t.Fatal("expected OnWebhook to be called")
	}
	// event_id is synthesized as <event_name>:<data.id>
	if svc.gotEvent.EventID != "order_created:42" {
		t.Errorf("event_id: got %q, want order_created:42", svc.gotEvent.EventID)
	}
	if svc.gotEvent.Channel != "lemonsqueezy" {
		t.Errorf("channel: got %q, want lemonsqueezy", svc.gotEvent.Channel)
	}
	if svc.gotEvent.TransactionID != "42" {
		t.Errorf("transaction_id: got %q, want 42 (data.id)", svc.gotEvent.TransactionID)
	}
	if svc.gotEvent.OrderID != "order-uuid-ls-1" {
		t.Errorf("order_id from custom_data: got %q", svc.gotEvent.OrderID)
	}
	if svc.gotEvent.Amount != 29.90 {
		t.Errorf("amount: got %v, want 29.90 (cents 2990 → major)", svc.gotEvent.Amount)
	}
	if svc.gotEvent.Currency != "USD" {
		t.Errorf("currency: got %q, want USD (uppercased)", svc.gotEvent.Currency)
	}
}

func TestWebhookHandler_LS_SubscriptionCreated_WithSubExpiresAt(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"meta": {
			"event_name": "subscription_created",
			"custom_data": {
				"order_id": "order-uuid-sub-1",
				"sub_expires_at": "2027-01-15T00:00:00Z"
			}
		},
		"data": {
			"type": "subscriptions",
			"id": "99",
			"attributes": { "total": 9990, "currency": "eur" }
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/lemonsqueezy", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if svc.gotEvent.EventID != "subscription_created:99" {
		t.Errorf("event_id: got %q", svc.gotEvent.EventID)
	}
	if svc.gotEvent.SubExpiresAt == nil {
		t.Fatal("SubExpiresAt should be populated from custom_data")
	}
	want := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	if !svc.gotEvent.SubExpiresAt.Equal(want) {
		t.Errorf("SubExpiresAt: got %v, want %v", svc.gotEvent.SubExpiresAt, want)
	}
}

func TestWebhookHandler_LS_OrderRefunded_DerivesExternalRefundID(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "refund_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"meta": { "event_name": "order_refunded" },
		"data": {
			"type": "orders",
			"id": "42",
			"attributes": { "total": 2990, "refunded_amount": 2990, "currency": "usd" }
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/lemonsqueezy", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if svc.gotEvent.ExternalRefundID != "ls-42" {
		t.Errorf("external_refund_id: got %q, want ls-42", svc.gotEvent.ExternalRefundID)
	}
	if svc.gotEvent.RefundAmount != 29.90 {
		t.Errorf("refund_amount: got %v, want 29.90", svc.gotEvent.RefundAmount)
	}
	// OrderID intentionally empty — order_refunded events don't carry custom_data.
	if svc.gotEvent.OrderID != "" {
		t.Errorf("order_id should be empty for refund event, got %q", svc.gotEvent.OrderID)
	}
}

func TestWebhookHandler_LS_SubscriptionPaymentRefunded_UsesSubscriptionID(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "refund_paid"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"meta": { "event_name": "subscription_payment_refunded" },
		"data": {
			"type": "subscription-invoices",
			"id": "invoice-7",
			"attributes": {
				"total": 9990,
				"refunded_amount": 9990,
				"currency": "usd",
				"subscription_id": "99"
			}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/lemonsqueezy", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	// Event ID uses the invoice's own data.id (NOT the subscription_id) so two
	// distinct renewal invoices dedupe independently.
	if svc.gotEvent.EventID != "subscription_payment_refunded:invoice-7" {
		t.Errorf("event_id: got %q, want subscription_payment_refunded:invoice-7", svc.gotEvent.EventID)
	}
	// TransactionID uses the parent subscription_id so the refund's lookup
	// matches the originating subscription_created payment row.
	if svc.gotEvent.TransactionID != "99" {
		t.Errorf("transaction_id: got %q, want 99 (subscription_id)", svc.gotEvent.TransactionID)
	}
	// custom_data is absent on subscription-invoice events.
	if svc.gotEvent.OrderID != "" {
		t.Errorf("order_id should be empty on invoice events, got %q", svc.gotEvent.OrderID)
	}
}

func TestWebhookHandler_LS_EnvelopeCompliance(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		result: &service.OnWebhookResult{DomainAction: "payment_paid", DuplicateEvent: false},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{"meta":{"event_name":"order_created"},"data":{"type":"orders","id":"1","attributes":{}}}`)
	rec := postRaw(engine, "/webhooks/payment/lemonsqueezy", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Top-level envelope: code + data only.
	for _, allowed := range []string{"code", "data"} {
		if _, ok := raw[allowed]; !ok {
			t.Errorf("envelope key %q missing", allowed)
		}
	}
	// domain_action / duplicate / received live INSIDE data, never at top level.
	for _, banned := range []string{"domain_action", "duplicate", "received"} {
		if _, ok := raw[banned]; ok {
			t.Errorf("envelope key %q must be NESTED in data", banned)
		}
	}
}

func TestWebhookHandler_LS_MissingEventName_400(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)

	// meta.event_name missing → parser rejects → 400.
	body := []byte(`{"meta":{},"data":{"type":"orders","id":"1","attributes":{}}}`)
	rec := postRaw(engine, "/webhooks/payment/lemonsqueezy", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 for missing event_name", rec.Code)
	}
}

// ============================================================================
// WeChat — would need real AES-GCM encryption; we only test the parse error
// path here. Full roundtrip is covered by middleware tests + e2e.
// ============================================================================

func TestWebhookHandler_WeChat_BadResourceJSON(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)

	// resource.ciphertext is not valid base64 → decrypt fails → 400.
	body := []byte(`{"id":"evt_1","event_type":"TRANSACTION.SUCCESS","resource":{"ciphertext":"!!!not-base64!!!","nonce":"abc","associated_data":""}}`)
	rec := postRaw(engine, "/webhooks/payment/wechat_pay", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Service errors → 500 (so channel retries)
// ============================================================================

func TestWebhookHandler_ServiceError_500(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{
		err: &transientSvcError{msg: "db down"},
	}
	engine := webhookTestEngine(svc)

	body := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","amount":100,"currency":"cny","metadata":{"order_id":"o-1"}}}}`)
	rec := postRaw(engine, "/webhooks/payment/stripe", body)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
}

func TestWebhookHandler_UnsupportedChannel_400(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)
	rec := postRaw(engine, "/webhooks/payment/paypal", []byte(`{}`))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "could not parse event") {
		t.Errorf("expected parse error message, got %s", rec.Body.String())
	}
}

// transientSvcError is a non-sentinel service error used by TestWebhookHandler_ServiceError_500.
type transientSvcError struct{ msg string }

func (e *transientSvcError) Error() string { return e.msg }

// helper unused at the moment — keep for future webhook test expansions.
var _ = json.Marshal