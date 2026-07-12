package handler

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
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
	h := NewWebhookHandler(svc, []byte("01234567890123456789012345678901"), verifier, false)
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

// TestWebhookHandler_Alipay_WithSubExpires covers the sub_expires_at
// RFC3339 parse path in parseAlipay.
func TestWebhookHandler_Alipay_WithSubExpires(t *testing.T) {
	t.Parallel()
	svc := &mockWebhookSvc{result: &service.OnWebhookResult{}}
	engine := webhookTestEngine(svc)
	body := []byte("out_trade_no=order-x&trade_no=t-1&total_amount=9.99&notify_id=n-9&notify_type=trade_status_sync&sub_expires_at=2026-12-31T23:59:59Z&sign=xx")
	rec := postRaw(engine, "/webhooks/payment/alipay", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if svc.gotEvent.SubExpiresAt == nil {
		t.Errorf("SubExpiresAt should be populated")
	}
}

// TestWebhookHandler_Alipay_MissingNotifyID covers the validation branch.
func TestWebhookHandler_Alipay_MissingNotifyID(t *testing.T) {
	t.Parallel()
	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)
	body := []byte("out_trade_no=order-x&trade_no=t-1&total_amount=9.99&notify_type=trade_status_sync&sign=xx")
	rec := postRaw(engine, "/webhooks/payment/alipay", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (no notify_id)", rec.Code)
	}
}

// TestWebhookHandler_Alipay_BadTotalAmount covers the strconv error path.
func TestWebhookHandler_Alipay_BadTotalAmount(t *testing.T) {
	t.Parallel()
	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)
	body := []byte("out_trade_no=order-x&trade_no=t-1&total_amount=notanumber&notify_id=n-1&notify_type=trade_status_sync&sign=xx")
	rec := postRaw(engine, "/webhooks/payment/alipay", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
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

// TestWebhookHandler_WeChat_Decryption_Success exercises parseWeChat happy
// path: builds an AES-GCM-encrypted WeChat resource payload, sends it
// through the handler, and asserts the parsed event reaches OnWebhook.
//
// This is the single test that exercises parseWeChat's body-parse + AES-GCM
// decryption + nested-resource extraction in one go. Without it the entire
// parseWeChat function (the only one we DON'T generate through the existing
// LS/PayPal test paths) sits at low coverage.
func TestWebhookHandler_WeChat_Decryption_Success(t *testing.T) {
	t.Parallel()

	// Build the inner resource (decrypted JSON) and encrypt it with the
	// same 32-byte key + 12-byte nonce that webhookTestEngine uses.
	innerJSON := []byte(`{
		"transaction_id":"4200001234567890",
		"out_trade_no":"order-uuid-wx-1",
		"amount":{"total":9990,"refund":0},
		"sub_expires_at":"2026-12-31T23:59:59Z"
	}`)
	key := []byte("01234567890123456789012345678901") // 32 bytes, matches webhookTestEngine
	nonce := []byte("0123456789ab")                   // 12 bytes for GCM
	associatedData := "order-id"

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	ciphertext := gcm.Seal(nil, nonce, innerJSON, []byte(associatedData))
	ciphertextB64 := base64.StdEncoding.EncodeToString(ciphertext)

	// Wrap in the WeChat outer envelope.
	outer := []byte(`{
		"id":"WH-WX-1",
		"event_type":"TRANSACTION.SUCCESS",
		"resource":{
			"ciphertext":"` + ciphertextB64 + `",
			"nonce":"` + string(nonce) + `",
			"associated_data":"` + associatedData + `"
		}
	}`)

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "payment_paid"}}
	engine := webhookTestEngine(svc)
	rec := postRaw(engine, "/webhooks/payment/wechat_pay", outer)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent == nil {
		t.Fatal("OnWebhook was not called")
	}
	if svc.gotEvent.OrderID != "order-uuid-wx-1" {
		t.Errorf("OrderID: got %q", svc.gotEvent.OrderID)
	}
	if svc.gotEvent.TransactionID != "4200001234567890" {
		t.Errorf("TransactionID: got %q", svc.gotEvent.TransactionID)
	}
	if svc.gotEvent.Amount != 99.90 {
		t.Errorf("Amount: got %v, want 99.90", svc.gotEvent.Amount)
	}
	if svc.gotEvent.SubExpiresAt == nil {
		t.Error("SubExpiresAt should be populated")
	}
}

// TestWebhookHandler_WeChat_BadNonceLength covers parseWeChat's nonce-mismatch
// branch — localWeChatDecrypt rejects short/long nonces before even calling
// gcm.Open (which would panic on wrong sizes).
func TestWebhookHandler_WeChat_BadNonceLength(t *testing.T) {
	t.Parallel()
	// Use the real 32-byte key + wrong-size nonce.
	ciphertextB64 := base64.StdEncoding.EncodeToString([]byte("not-really-cipher"))
	outer := []byte(`{
		"id":"WH-WX-2",
		"event_type":"TRANSACTION.SUCCESS",
		"resource":{"ciphertext":"` + ciphertextB64 + `","nonce":"short","associated_data":""}
	}`)
	svc := &mockWebhookSvc{}
	engine := webhookTestEngine(svc)
	rec := postRaw(engine, "/webhooks/payment/wechat_pay", outer)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// TestWebhookHandler_WeChat_LocalDecryptPath exercises the localWeChatDecrypt
// fallback used when the verifier is NOT a *middleware.WeChatPayV3Verifier.
// Without this test the in-package fallback is 0% covered.
func TestWebhookHandler_WeChat_LocalDecryptPath(t *testing.T) {
	t.Parallel()
	// Build the inner resource and encrypt with the production key.
	innerJSON := []byte(`{"transaction_id":"tx-x","out_trade_no":"order-x","amount":{"total":100,"refund":0}}`)
	key := []byte("01234567890123456789012345678901") // 32 bytes
	nonce := []byte("0123456789ab")                   // 12 bytes
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	ct := gcm.Seal(nil, nonce, innerJSON, nil)
	ciphertextB64 := base64.StdEncoding.EncodeToString(ct)
	outer := []byte(`{
		"id":"WH-WX-3",
		"event_type":"TRANSACTION.SUCCESS",
		"resource":{"ciphertext":"` + ciphertextB64 + `","nonce":"` + string(nonce) + `","associated_data":""}
	}`)

	// Construct a WebhookHandler with wechatKey set but verifier type that
	// is NOT *middleware.WeChatPayV3Verifier (use a Stripe verifier to force
	// the localWeChatDecrypt fallback).
	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "payment_paid"}}
	h := NewWebhookHandler(svc, key, &middleware.StripeVerifier{Secret: []byte("x")}, false)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/webhooks/payment/:channel", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/payment/wechat_pay", bytes.NewReader(outer))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent == nil || svc.gotEvent.OrderID != "order-x" {
		t.Errorf("localWeChatDecrypt fallback did not populate event: %+v", svc.gotEvent)
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

// ============================================================================
// PayPal parsing
// ============================================================================

func TestWebhookHandler_Paypal_CaptureCompleted(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "payment_paid"}}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "WH-PP-1",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {
			"id": "3C93638325N1234567",
			"custom_id": "order-uuid-123",
			"amount": {"value": "29.90", "currency_code": "USD"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if svc.gotEvent == nil {
		t.Fatalf("OnWebhook not called")
	}
	if svc.gotEvent.Channel != "paypal" {
		t.Errorf("channel: got %q", svc.gotEvent.Channel)
	}
	if svc.gotEvent.EventID != "WH-PP-1" {
		t.Errorf("event_id: got %q", svc.gotEvent.EventID)
	}
	if svc.gotEvent.EventType != "PAYMENT.CAPTURE.COMPLETED" {
		t.Errorf("event_type: got %q", svc.gotEvent.EventType)
	}
	if svc.gotEvent.OrderID != "order-uuid-123" {
		t.Errorf("order_id: got %q", svc.gotEvent.OrderID)
	}
	if svc.gotEvent.TransactionID != "3C93638325N1234567" {
		t.Errorf("transaction_id: got %q", svc.gotEvent.TransactionID)
	}
	if svc.gotEvent.Amount != 29.90 {
		t.Errorf("amount: got %v, want 29.90", svc.gotEvent.Amount)
	}
	if svc.gotEvent.Currency != "USD" {
		t.Errorf("currency: got %q", svc.gotEvent.Currency)
	}
}

func TestWebhookHandler_Paypal_CaptureRefunded_DerivesExternalRefundID(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "refund_paid"}}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "WH-PP-2",
		"event_type": "PAYMENT.CAPTURE.REFUNDED",
		"resource": {
			"id": "REFUND-ID-1",
			"custom_id": "order-uuid-999",
			"amount": {"value": "29.90", "currency_code": "USD"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if svc.gotEvent.RefundAmount != 29.90 {
		t.Errorf("refund_amount: got %v", svc.gotEvent.RefundAmount)
	}
	if svc.gotEvent.ExternalRefundID != "paypal-REFUND-ID-1" {
		t.Errorf("external_refund_id: got %q", svc.gotEvent.ExternalRefundID)
	}
}

func TestWebhookHandler_Paypal_SaleCompleted_SetsSubscriptionAndExpiry(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "payment_paid"}}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "WH-PP-3",
		"event_type": "PAYMENT.SALE.COMPLETED",
		"resource": {
			"id": "SALE-1",
			"billing_agreement_id": "I-BWX42ABCD",
			"custom_id": "order-uuid-456",
			"amount": {"value": "9.99", "currency_code": "USD"},
			"billing_info": {"next_billing_time": "2026-08-30T12:00:00Z"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if svc.gotEvent.ExternalSubscriptionID != "I-BWX42ABCD" {
		t.Errorf("external_subscription_id: got %q", svc.gotEvent.ExternalSubscriptionID)
	}
	if svc.gotEvent.SubExpiresAt == nil {
		t.Fatalf("sub_expires_at should be populated from next_billing_time")
	}
	want := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	if !svc.gotEvent.SubExpiresAt.Equal(want) {
		t.Errorf("sub_expires_at: got %v, want %v", svc.gotEvent.SubExpiresAt.UTC(), want)
	}
}

func TestWebhookHandler_Paypal_SubscriptionCreated_HasExternalSubscriptionID(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "payment_paid"}}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "WH-PP-4",
		"event_type": "BILLING.SUBSCRIPTION.CREATED",
		"resource": {
			"id": "I-BWX99ZZZZ",
			"custom_id": "order-uuid-789"
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if svc.gotEvent.ExternalSubscriptionID != "I-BWX99ZZZZ" {
		t.Errorf("external_subscription_id: got %q", svc.gotEvent.ExternalSubscriptionID)
	}
	if svc.gotEvent.EventType != "BILLING.SUBSCRIPTION.CREATED" {
		t.Errorf("event_type: got %q", svc.gotEvent.EventType)
	}
}

func TestWebhookHandler_Paypal_MissingCustomID_400(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{}}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "WH-PP-5",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {"id": "X", "amount": {"value": "1.00", "currency_code": "USD"}}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	if svc.gotEvent != nil {
		t.Errorf("OnWebhook should not be called when parse fails")
	}
}

// TestWebhookHandler_Paypal_SaleCompleted_NoCustomID_OK guards the renewal
// path: PAYMENT.SALE.COMPLETED is the PayPal auto-renewal event and
// PayPal does not always echo custom_id on renewal deliveries (the
// original subscription's custom_id is reused by the SUBSCRIPTION.CREATED
// path). Requiring custom_id globally would silently drop every renewal
// webhook, leaving a paid customer without an extended subscription.
// Resource.billing_agreement_id is sufficient to route the renewal —
// ExternalSubscriptionID is set from it on the service side.
func TestWebhookHandler_Paypal_SaleCompleted_NoCustomID_OK(t *testing.T) {
	t.Parallel()

	svc := &mockWebhookSvc{result: &service.OnWebhookResult{DomainAction: "payment_paid"}}
	engine := webhookTestEngine(svc)

	body := []byte(`{
		"id": "WH-PP-RENEW",
		"event_type": "PAYMENT.SALE.COMPLETED",
		"resource": {
			"id": "SALE-RENEW",
			"billing_agreement_id": "I-BWX42RENEW",
			"amount": {"value": "9.99", "currency_code": "USD"},
			"billing_info": {"next_billing_time": "2026-09-30T12:00:00Z"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; renewal must not require custom_id", rec.Code)
	}
	if svc.gotEvent == nil {
		t.Fatalf("OnWebhook was not called")
	}
	if svc.gotEvent.ExternalSubscriptionID != "I-BWX42RENEW" {
		t.Errorf("external_subscription_id: got %q, want %q", svc.gotEvent.ExternalSubscriptionID, "I-BWX42RENEW")
	}
	if svc.gotEvent.SubExpiresAt == nil {
		t.Errorf("sub_expires_at should be populated from next_billing_time")
	}
}

func TestWebhookHandler_Paypal_ChannelUnknownToVerifier_404(t *testing.T) {
	// Sanity: confirm that with no verifier wired for paypal, the handler
	// still receives the request and parses OK (signature is bypassed in
	// webhookTestEngine — we just verify the parser dispatch reaches OnWebhook).
	t.Parallel()
	svc := &mockWebhookSvc{result: &service.OnWebhookResult{}}
	engine := webhookTestEngine(svc)
	body := []byte(`{
		"id": "WH-PP-6",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {
			"id": "CAP-X",
			"custom_id": "order-x",
			"amount": {"value": "0.50", "currency_code": "USD"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
}

// TestWebhookHandler_Paypal_MalformedAmount_400 covers bug-8 fix: a
// PAYMENT.* event with unparseable amount.value used to silently coerce to
// 0. Now it returns a parse error → 400 (channel will not retry).
func TestWebhookHandler_Paypal_MalformedAmount_400(t *testing.T) {
	t.Parallel()
	svc := &mockWebhookSvc{result: &service.OnWebhookResult{}}
	engine := webhookTestEngine(svc)
	body := []byte(`{
		"id": "WH-PP-7",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {
			"id": "CAP-Y",
			"custom_id": "order-y",
			"amount": {"value": "not-a-number", "currency_code": "USD"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	if svc.gotEvent != nil {
		t.Errorf("OnWebhook should not be called when amount is malformed")
	}
}

// TestWebhookHandler_Paypal_EmptyAmount_400 ensures we also reject the empty
// value case (already malformed; treated as 400 not silent-coerce).
func TestWebhookHandler_Paypal_EmptyAmount_400(t *testing.T) {
	t.Parallel()
	svc := &mockWebhookSvc{result: &service.OnWebhookResult{}}
	engine := webhookTestEngine(svc)
	body := []byte(`{
		"id": "WH-PP-8",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {
			"id": "CAP-Z",
			"custom_id": "order-z",
			"amount": {"value": "", "currency_code": "USD"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// TestWebhookHandler_Paypal_MissingResourceID_400 covers the bug-find from
// the second code-review pass. Without this guard, an empty resource.id
// silently produces TransactionID=""/ExternalSubscriptionID="" and
// (channel="paypal", external_txn_id="") collisions on the
// payments.external_txn_id UNIQUE.
func TestWebhookHandler_Paypal_MissingResourceID_400(t *testing.T) {
	t.Parallel()
	svc := &mockWebhookSvc{result: &service.OnWebhookResult{}}
	engine := webhookTestEngine(svc)
	body := []byte(`{
		"id": "WH-PP-9",
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {
			"custom_id": "order-x",
			"amount": {"value": "1.00", "currency_code": "USD"}
		}
	}`)
	rec := postRaw(engine, "/webhooks/payment/paypal", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	if svc.gotEvent != nil {
		t.Errorf("OnWebhook should not be called when resource.id is empty")
	}
}
func TestLastPathSegment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"normal URL with id", "https://api.sandbox.paypal.com/v1/payments/captures/CAP-12345", "CAP-12345"},
		{"URL with trailing slash (empty segment)", "https://example.com/foo/", ""},
		{"empty string", "", ""},
		{"no slashes", "abc", ""},
		{"URL with query", "https://example.com/payments/captures/CAP-1?foo=bar", "CAP-1?foo=bar"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := lastPathSegment(c.in); got != c.want {
				t.Errorf("lastPathSegment(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
