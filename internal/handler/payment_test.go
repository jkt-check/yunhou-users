package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// mockPaymentSvc is the test double for service.PaymentServiceInterface.
// Each method has a configurable error + return value, which is the pattern
// used elsewhere in this package (auth_test.go, app handler tests).
type mockPaymentSvc struct {
	createOrderResp  *model.Order
	createOrderErr   error
	gotCreateChannel string
	cancelOrderErr   error
	confirmResult    *service.ConfirmResult
	confirmErr       error
	gotConfirmInput  *service.ConfirmInput // captures the input Confirm was called with
	refundResult     *service.RefundResult
	refundErr        error
	getOrderResp     *model.Order
	getOrderErr      error
	listPayments     []model.Payment
	listPaymentsErr  error
	getPaymentResp   *model.Payment
	getPaymentErr    error
	listRefunds      []model.Refund
	listRefundsErr   error
	getRefundResp    *model.Refund
	getRefundErr     error
	onWebhookResult  *service.OnWebhookResult
	onWebhookErr     error
}

func (m *mockPaymentSvc) CreateOrder(_ context.Context, _, _, channel string) (*model.Order, error) {
	m.gotCreateChannel = channel
	return m.createOrderResp, m.createOrderErr
}
func (m *mockPaymentSvc) CancelOrder(_ context.Context, _, _ string) error {
	return m.cancelOrderErr
}
func (m *mockPaymentSvc) Confirm(_ context.Context, in service.ConfirmInput) (*service.ConfirmResult, error) {
	m.gotConfirmInput = &in
	return m.confirmResult, m.confirmErr
}
func (m *mockPaymentSvc) Refund(_ context.Context, _ service.RefundInput) (*service.RefundResult, error) {
	return m.refundResult, m.refundErr
}
func (m *mockPaymentSvc) OnWebhook(_ context.Context, _ service.WebhookEvent) (*service.OnWebhookResult, error) {
	return m.onWebhookResult, m.onWebhookErr
}
func (m *mockPaymentSvc) GetOrder(_ context.Context, _, _ string) (*model.Order, error) {
	return m.getOrderResp, m.getOrderErr
}
func (m *mockPaymentSvc) ListUserPayments(_ context.Context, _ string) ([]model.Payment, error) {
	return m.listPayments, m.listPaymentsErr
}
func (m *mockPaymentSvc) GetPayment(_ context.Context, _, _ string) (*model.Payment, error) {
	return m.getPaymentResp, m.getPaymentErr
}
func (m *mockPaymentSvc) ListPaymentRefunds(_ context.Context, _, _ string) ([]model.Refund, error) {
	return m.listRefunds, m.listRefundsErr
}
func (m *mockPaymentSvc) GetRefund(_ context.Context, _, _ string) (*model.Refund, error) {
	return m.getRefundResp, m.getRefundErr
}

// Compile-time check: mockPaymentSvc must satisfy the interface.
var _ service.PaymentServiceInterface = (*mockPaymentSvc)(nil)

// paymentTestEngine builds a Gin engine with /payments and /refunds routes
// wired up against the supplied mock service, with a fake user_id injected
// (bypassing JWT verification — the JWT path is exercised in e2e tests).
func paymentTestEngine(svc service.PaymentServiceInterface, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewPaymentHandler(svc)

	// Inject fake user_id for every request.
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserID, userID)
		c.Next()
	})

	engine.POST("/payments/orders", h.CreateOrder)
	engine.GET("/payments/orders/:id", h.GetOrder)
	engine.DELETE("/payments/orders/:id", h.CancelOrder)
	engine.POST("/payments/orders/:order_id/confirm", h.ConfirmOrder)
	engine.GET("/payments", h.ListPayments)
	engine.GET("/payments/:id", h.GetPayment)
	engine.GET("/payments/:id/refunds", h.ListPaymentRefunds)
	engine.POST("/refunds", h.CreateRefund)
	engine.GET("/refunds/:id", h.GetRefund)
	return engine
}

func doRequest(engine *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// CreateOrder
// ============================================================================

func TestPaymentHandler_CreateOrder(t *testing.T) {
	t.Parallel()

	t.Run("success returns 201 with order", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			createOrderResp: &model.Order{
				ID:     "o-1",
				UserID: "user-1",
				PlanID: "monthly",
				Amount: 29.9,
				Status: "pending",
			},
		}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders", map[string]string{"plan_id": "monthly", "channel": "wechat_pay"})

		if rec.Code != http.StatusCreated {
			t.Errorf("status: got %d, want 201 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.gotCreateChannel != "wechat_pay" {
			t.Errorf("CreateOrder channel: got %q, want wechat_pay", svc.gotCreateChannel)
		}
	})

	t.Run("bad plan → 400", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{createOrderErr: service.ErrPlanNotFound}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders", map[string]string{"plan_id": "ghost", "channel": "stripe"})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400", rec.Code)
		}
	})

	t.Run("already has active sub → 409", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{createOrderErr: service.ErrUserHasActiveSub}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders", map[string]string{"plan_id": "monthly", "channel": "wechat_pay"})

		if rec.Code != http.StatusConflict {
			t.Errorf("status: got %d, want 409", rec.Code)
		}
	})

	t.Run("missing plan_id → 400 (binding)", func(t *testing.T) {
		t.Parallel()
		engine := paymentTestEngine(&mockPaymentSvc{}, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders", map[string]string{"channel": "stripe"})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400 (binding error)", rec.Code)
		}
	})

	t.Run("missing channel → 400 (binding)", func(t *testing.T) {
		t.Parallel()
		engine := paymentTestEngine(&mockPaymentSvc{}, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders", map[string]string{"plan_id": "monthly"})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400 (binding error)", rec.Code)
		}
	})
}

// ============================================================================
// GetOrder
// ============================================================================

func TestPaymentHandler_GetOrder(t *testing.T) {
	t.Parallel()

	t.Run("success returns 200", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			getOrderResp: &model.Order{ID: "o-1", UserID: "user-1", Status: "pending"},
		}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodGet, "/payments/orders/o-1", nil)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200", rec.Code)
		}
	})

	t.Run("not found → 404", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{getOrderErr: service.ErrOrderNotFound}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodGet, "/payments/orders/o-1", nil)

		if rec.Code != http.StatusNotFound {
			t.Errorf("status: got %d, want 404", rec.Code)
		}
	})
}

// ============================================================================
// CancelOrder
// ============================================================================

func TestPaymentHandler_CancelOrder(t *testing.T) {
	t.Parallel()

	t.Run("success returns 200", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodDelete, "/payments/orders/o-1", nil)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200", rec.Code)
		}
	})

	t.Run("missing auth → 401", func(t *testing.T) {
		t.Parallel()
		gin.SetMode(gin.TestMode)
		svc := &mockPaymentSvc{}
		h := NewPaymentHandler(svc)
		engine := gin.New()
		engine.DELETE("/payments/orders/:id", h.CancelOrder)
		req := httptest.NewRequest(http.MethodDelete, "/payments/orders/o-1", nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("missing auth: got %d, want 401", rec.Code)
		}
	})

	t.Run("internal error → 500", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{cancelOrderErr: errors.New("db exploded")}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodDelete, "/payments/orders/o-1", nil)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("internal error: got %d, want 500", rec.Code)
		}
	})

	t.Run("not pending → 409", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{cancelOrderErr: service.ErrOrderNotPending}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodDelete, "/payments/orders/o-1", nil)

		if rec.Code != http.StatusConflict {
			t.Errorf("status: got %d, want 409", rec.Code)
		}
	})

	t.Run("not found → 404", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{cancelOrderErr: service.ErrOrderNotFound}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodDelete, "/payments/orders/o-1", nil)

		if rec.Code != http.StatusNotFound {
			t.Errorf("status: got %d, want 404", rec.Code)
		}
	})
}

// ============================================================================
// ConfirmOrder
// ============================================================================

func TestPaymentHandler_ConfirmOrder(t *testing.T) {
	t.Parallel()

	t.Run("success returns 200", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			confirmResult: &service.ConfirmResult{
				PaymentID:             "p-1",
				OrderID:               "o-1",
				Status:                "paid",
				ActivatedSubscription: true,
			},
		}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders/o-1/confirm", map[string]any{
			"channel":         "stripe",
			"external_txn_id": "pi_xxx",
		})

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	// TestPaymentHandler_ConfirmOrder_PropagatesExpiresAt asserts the
	// handler forwards the caller's `expires_at` to ConfirmInput. Without
	// this, the subscription activation writes expires_at=NULL and
	// monthly/quarterly/yearly plans never expire (regression of round-5
	// fix #1).
	t.Run("forwards expires_at to ConfirmInput", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			confirmResult: &service.ConfirmResult{PaymentID: "p-x", OrderID: "o-x", Status: "paid"},
		}
		engine := paymentTestEngine(svc, "user-1")
		want := "2027-06-01T00:00:00Z"
		rec := doRequest(engine, http.MethodPost, "/payments/orders/o-x/confirm", map[string]any{
			"channel":         "stripe",
			"external_txn_id": "pi_x",
			"expires_at":      want,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		if svc.gotConfirmInput == nil {
			t.Fatal("Confirm not called")
		}
		if svc.gotConfirmInput.ExpiresAt == nil {
			t.Fatal("ConfirmInput.ExpiresAt should be populated from request body")
		}
		gotStr := svc.gotConfirmInput.ExpiresAt.Format(time.RFC3339)
		if gotStr != want {
			t.Errorf("ExpiresAt: got %s, want %s", gotStr, want)
		}
	})

	// omits-ExpiresAt is allowed (free plan / explicit no-end).
	t.Run("nil expires_at is allowed", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			confirmResult: &service.ConfirmResult{PaymentID: "p-y", OrderID: "o-y", Status: "paid"},
		}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders/o-y/confirm", map[string]any{
			"channel":         "stripe",
			"external_txn_id": "pi_y",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", rec.Code)
		}
		if svc.gotConfirmInput == nil {
			t.Fatal("Confirm not called")
		}
		if svc.gotConfirmInput.ExpiresAt != nil {
			t.Errorf("ExpiresAt should be nil when omitted, got %v", svc.gotConfirmInput.ExpiresAt)
		}
	})

	t.Run("channel mismatch → 409", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{confirmErr: service.ErrOrderChannelMismatch}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders/o-1/confirm", map[string]any{
			"channel":         "wechat_pay",
			"external_txn_id": "wx_1",
		})

		if rec.Code != http.StatusConflict {
			t.Errorf("status: got %d, want 409", rec.Code)
		}
	})

	t.Run("terminal-non-recoverable → 409", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{confirmErr: service.ErrOrderAlreadyTerminal}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders/o-1/confirm", map[string]any{
			"channel":         "stripe",
			"external_txn_id": "pi_xxx",
		})

		if rec.Code != http.StatusConflict {
			t.Errorf("status: got %d, want 409", rec.Code)
		}
	})

	t.Run("missing channel → 400 binding", func(t *testing.T) {
		t.Parallel()
		engine := paymentTestEngine(&mockPaymentSvc{}, "user-1")
		rec := doRequest(engine, http.MethodPost, "/payments/orders/o-1/confirm", map[string]any{
			"external_txn_id": "pi_xxx",
		})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400", rec.Code)
		}
	})
}

// ============================================================================
// CreateRefund — Idempotency-Key is REQUIRED
// ============================================================================

func TestPaymentHandler_CreateRefund(t *testing.T) {
	t.Parallel()

	t.Run("missing Idempotency-Key → 400", func(t *testing.T) {
		t.Parallel()
		engine := paymentTestEngine(&mockPaymentSvc{}, "user-1")
		rec := doRequest(engine, http.MethodPost, "/refunds", map[string]any{
			"payment_id": "p-1",
			"amount":     5.0,
		})

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400", rec.Code)
		}
	})

	t.Run("Idempotency-Key too short → 400", func(t *testing.T) {
		t.Parallel()
		engine := paymentTestEngine(&mockPaymentSvc{}, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":5.0}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "short")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("Idempotency-Key with invalid chars → 400", func(t *testing.T) {
		t.Parallel()
		engine := paymentTestEngine(&mockPaymentSvc{}, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":5.0}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "has spaces here!")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("Idempotency-Key valid: letters/digits/separators", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			refundResult: &service.RefundResult{
				Refund: &model.Refund{ID: "r-1", PaymentID: "p-1", Amount: 5.0, Status: "pending"},
			},
		}
		engine := paymentTestEngine(svc, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":5.0}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "uuid_abc-123.def:42")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("success returns 200", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			refundResult: &service.RefundResult{
				Refund: &model.Refund{ID: "r-1", PaymentID: "p-1", Amount: 5.0, Status: "pending"},
			},
		}
		engine := paymentTestEngine(svc, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":5.0}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-001")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid amount → 400", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{refundErr: service.ErrRefundAmountInvalid}
		engine := paymentTestEngine(svc, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":-1}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-bad")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400", rec.Code)
		}
	})

	t.Run("channel API failure → 502", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{refundErr: service.ErrRefundChannelFailed}
		engine := paymentTestEngine(svc, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":5.0}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-channel-fail")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Errorf("status: got %d, want 502", rec.Code)
		}
	})

	t.Run("sum exceeds payment → 400", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{refundErr: service.ErrRefundSumExceedsPayment}
		engine := paymentTestEngine(svc, "user-1")
		req := httptest.NewRequest(http.MethodPost, "/refunds",
			bytes.NewReader([]byte(`{"payment_id":"p-1","amount":1000.0}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-sum")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status: got %d, want 400", rec.Code)
		}
	})
}

// ============================================================================
// GetRefund
// ============================================================================

func TestPaymentHandler_GetRefund(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			getRefundResp: &model.Refund{ID: "r-1", PaymentID: "p-1", Amount: 5.0, Status: "paid"},
		}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodGet, "/refunds/r-1", nil)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200", rec.Code)
		}
	})

	t.Run("not found → 404", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{getRefundErr: service.ErrRefundNotFound}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodGet, "/refunds/r-1", nil)

		if rec.Code != http.StatusNotFound {
			t.Errorf("status: got %d, want 404", rec.Code)
		}
	})
}

// ============================================================================
// ListPayments / GetPayment / ListPaymentRefunds (smoke)
// ============================================================================

func TestPaymentHandler_ListPayments(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		svc := &mockPaymentSvc{
			listPayments: []model.Payment{{ID: "p-1", OrderID: "o-1", Status: "paid"}},
		}
		engine := paymentTestEngine(svc, "user-1")
		rec := doRequest(engine, http.MethodGet, "/payments", nil)

		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d, want 200", rec.Code)
		}
	})
}

func TestPaymentHandler_GetPayment_NotFound(t *testing.T) {
	t.Parallel()

	svc := &mockPaymentSvc{getPaymentErr: service.ErrPaymentNotFound}
	engine := paymentTestEngine(svc, "user-1")
	rec := doRequest(engine, http.MethodGet, "/payments/p-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestPaymentHandler_ListPaymentRefunds_PropagatesOwnershipErr(t *testing.T) {
	t.Parallel()

	// Service returns ErrPaymentNotFound when caller doesn't own the
	// payment — handler should surface 404.
	svc := &mockPaymentSvc{listRefundsErr: service.ErrPaymentNotFound}
	engine := paymentTestEngine(svc, "user-1")
	rec := doRequest(engine, http.MethodGet, "/payments/p-1/refunds", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestPaymentHandler_ListPayments_NoAuth_401(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	svc := &mockPaymentSvc{}
	h := NewPaymentHandler(svc)
	engine := gin.New()
	engine.GET("/payments", h.ListPayments)
	rec := doRequest(engine, http.MethodGet, "/payments", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d, want 401", rec.Code)
	}
}

func TestPaymentHandler_ListPayments_Error_500(t *testing.T) {
	t.Parallel()
	svc := &mockPaymentSvc{listPaymentsErr: errors.New("db exploded")}
	engine := paymentTestEngine(svc, "user-1")
	rec := doRequest(engine, http.MethodGet, "/payments", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("DB error: got %d, want 500", rec.Code)
	}
}

func TestPaymentHandler_GetPayment_NoAuth_401(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	svc := &mockPaymentSvc{}
	h := NewPaymentHandler(svc)
	engine := gin.New()
	engine.GET("/payments/:id", h.GetPayment)
	rec := doRequest(engine, http.MethodGet, "/payments/p-1", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d, want 401", rec.Code)
	}
}

func TestPaymentHandler_GetPayment_InternalErr_500(t *testing.T) {
	t.Parallel()
	svc := &mockPaymentSvc{getPaymentErr: errors.New("db exploded")}
	engine := paymentTestEngine(svc, "user-1")
	rec := doRequest(engine, http.MethodGet, "/payments/p-1", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("DB error: got %d, want 500", rec.Code)
	}
}

func TestPaymentHandler_ListPaymentRefunds_NoAuth_401(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	svc := &mockPaymentSvc{}
	h := NewPaymentHandler(svc)
	engine := gin.New()
	engine.GET("/payments/:id/refunds", h.ListPaymentRefunds)
	rec := doRequest(engine, http.MethodGet, "/payments/p-1/refunds", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d, want 401", rec.Code)
	}
}

func TestPaymentHandler_ListPaymentRefunds_InternalErr_500(t *testing.T) {
	t.Parallel()
	svc := &mockPaymentSvc{listRefundsErr: errors.New("db exploded")}
	engine := paymentTestEngine(svc, "user-1")
	rec := doRequest(engine, http.MethodGet, "/payments/p-1/refunds", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("DB error: got %d, want 500", rec.Code)
	}
}

// ============================================================================
// Internal error → 500
// ============================================================================

func TestPaymentHandler_InternalError_500(t *testing.T) {
	t.Parallel()

	svc := &mockPaymentSvc{
		getOrderErr: errors.New("db unreachable"),
	}
	engine := paymentTestEngine(svc, "user-1")
	rec := doRequest(engine, http.MethodGet, "/payments/orders/o-1", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

// unused import shims so go vet stays quiet when the file is partially edited
var _ = time.Now
var _ = mockPaymentSvc{}
var _ = service.PaymentServiceInterface(nil)

// ============================================================================
// writePaymentError — drives every branch in the central error mapper by
// invoking CreateRefund / CancelOrder / ConfirmOrder with the matching sentinels.
// ============================================================================

func TestWritePaymentError_Branches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		svc         *mockPaymentSvc
		method      string // GET / POST / PATCH
		path        string
		body        string
		headers     map[string]string
		wantStatus  int
		wantMessage string
	}{
		// Confirms drive writePaymentError via createOrderErr + confirmErr
		{"ConfirmOrder_ErrPlanNotFound_400",
			&mockPaymentSvc{confirmErr: service.ErrPlanNotFound},
			http.MethodPost, "/payments/orders/o-1/confirm",
			`{"channel":"stripe","external_txn_id":"t-1"}`, nil,
			http.StatusBadRequest, "plan not found"},
		{"ConfirmOrder_ErrUserHasActiveSub_409",
			&mockPaymentSvc{confirmErr: service.ErrUserHasActiveSub},
			http.MethodPost, "/payments/orders/o-1/confirm",
			`{"channel":"stripe","external_txn_id":"t-1"}`, nil,
			http.StatusConflict, "already has an active"},
		{"ConfirmOrder_ErrOrderAlreadyTerminal_409",
			&mockPaymentSvc{confirmErr: service.ErrOrderAlreadyTerminal},
			http.MethodPost, "/payments/orders/o-1/confirm",
			`{"channel":"stripe","external_txn_id":"t-1"}`, nil,
			http.StatusConflict, "terminal"},
		{"ConfirmOrder_ErrOrderChannelMismatch_409",
			&mockPaymentSvc{confirmErr: service.ErrOrderChannelMismatch},
			http.MethodPost, "/payments/orders/o-1/confirm",
			`{"channel":"paypal","external_txn_id":"t-1"}`, nil,
			http.StatusConflict, "channel"},
		{"ConfirmOrder_ErrMissingIdempotencyKey_400",
			&mockPaymentSvc{confirmErr: service.ErrMissingIdempotencyKey},
			http.MethodPost, "/payments/orders/o-1/confirm",
			`{"channel":"stripe","external_txn_id":"t-1"}`, nil,
			http.StatusBadRequest, "Idempotency-Key"},

		// CancelOrder via cancelOrderErr
		{"CancelOrder_ErrOrderNotFound_404",
			&mockPaymentSvc{cancelOrderErr: service.ErrOrderNotFound},
			http.MethodDelete, "/payments/orders/o-1", "", nil,
			http.StatusNotFound, "not found"},
		{"CancelOrder_ErrOrderNotPending_409",
			&mockPaymentSvc{cancelOrderErr: service.ErrOrderNotPending},
			http.MethodDelete, "/payments/orders/o-1", "", nil,
			http.StatusConflict, "not in pending"},

		// Refund branches (need Idempotency-Key header to pass entry guard)
		{"CreateRefund_ErrRefundAmountInvalid_400",
			&mockPaymentSvc{refundErr: service.ErrRefundAmountInvalid},
			http.MethodPost, "/refunds", `{"payment_id":"p-1","amount":-1}`,
			map[string]string{"Idempotency-Key": "test-key-12345"},
			http.StatusBadRequest, "refund amount"},
		{"CreateRefund_ErrRefundSumExceedsPayment_400",
			&mockPaymentSvc{refundErr: service.ErrRefundSumExceedsPayment},
			http.MethodPost, "/refunds", `{"payment_id":"p-1","amount":1}`,
			map[string]string{"Idempotency-Key": "test-key-12345"},
			http.StatusBadRequest, "sum of refunds"},
		{"CreateRefund_ErrRefundChannelFailed_502",
			&mockPaymentSvc{refundErr: service.ErrRefundChannelFailed},
			http.MethodPost, "/refunds", `{"payment_id":"p-1","amount":1}`,
			map[string]string{"Idempotency-Key": "test-key-12345"},
			http.StatusBadGateway, "channel refund"},
		{"CreateRefund_ErrPaymentNotFound_404",
			&mockPaymentSvc{refundErr: service.ErrPaymentNotFound},
			http.MethodPost, "/refunds", `{"payment_id":"missing","amount":1}`,
			map[string]string{"Idempotency-Key": "test-key-12345"},
			http.StatusNotFound, "not found"},
		{"CreateRefund_ErrPaymentNotPaid_409",
			&mockPaymentSvc{refundErr: service.ErrPaymentNotPaid},
			http.MethodPost, "/refunds", `{"payment_id":"p-1","amount":1}`,
			map[string]string{"Idempotency-Key": "test-key-12345"},
			http.StatusConflict, "not in paid"},
		{"CreateRefund_unknown_500",
			&mockPaymentSvc{refundErr: errors.New("db exploded")},
			http.MethodPost, "/refunds", `{"payment_id":"p-1","amount":1}`,
			map[string]string{"Idempotency-Key": "test-key-12345"},
			http.StatusInternalServerError, "internal"},

		// GetRefund
		{"GetRefund_ErrRefundNotFound_404",
			&mockPaymentSvc{getRefundErr: service.ErrRefundNotFound},
			http.MethodGet, "/refunds/r-1", "", nil,
			http.StatusNotFound, "not found"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := paymentTestEngine(tc.svc, "user-1")
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}
