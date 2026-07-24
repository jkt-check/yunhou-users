package handler

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// doPayRequest is like payment_test.go's doRequest but accepts a string body
// verbatim (no JSON re-encoding). Returns the response recorder.
func doPayRequest(engine interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, method, path, body string) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	var req *http.Request
	if rdr == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, rdr)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// doPayRequestWithHeaders adds custom headers to the request.
func doPayRequestWithHeaders(engine interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	var req *http.Request
	if rdr == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, rdr)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestPaymentHandler_ListPayments_Empty(t *testing.T) {
	svc := &mockPaymentSvc{listPayments: nil}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestPaymentHandler_ListPayments_Error(t *testing.T) {
	svc := &mockPaymentSvc{listPaymentsErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_GetPayment_Owner(t *testing.T) {
	svc := &mockPaymentSvc{getPaymentResp: &model.Payment{ID: "p-1", Status: "paid"}}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments/p-1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", w.Code)
	}
}

func TestPaymentHandler_GetPayment_InternalError(t *testing.T) {
	svc := &mockPaymentSvc{getPaymentErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments/p-1", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_ListPaymentRefunds_Owner(t *testing.T) {
	svc := &mockPaymentSvc{listRefunds: []model.Refund{{ID: "r-1"}}}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments/p-1/refunds", "")
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", w.Code)
	}
}

func TestPaymentHandler_ListPaymentRefunds_Error(t *testing.T) {
	svc := &mockPaymentSvc{listRefundsErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments/p-1/refunds", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_GetRefund_Owner(t *testing.T) {
	svc := &mockPaymentSvc{getRefundResp: &model.Refund{ID: "r-1", Status: "pending"}}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/refunds/r-1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", w.Code)
	}
}

func TestPaymentHandler_GetRefund_InternalError(t *testing.T) {
	svc := &mockPaymentSvc{getRefundErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/refunds/r-1", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_CreateOrder_PlanNotFound(t *testing.T) {
	svc := &mockPaymentSvc{createOrderErr: service.ErrPlanNotFound}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders", `{"plan_id":"missing","channel":"stripe"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateOrder_PlanInactive(t *testing.T) {
	svc := &mockPaymentSvc{createOrderErr: service.ErrPlanInactive}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders", `{"plan_id":"inactive","channel":"stripe"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateOrder_HasActiveSub(t *testing.T) {
	svc := &mockPaymentSvc{createOrderErr: service.ErrUserHasActiveSub}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders", `{"plan_id":"monthly","channel":"stripe"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestPaymentHandler_CreateOrder_PlanNotAcceptingNew(t *testing.T) {
	svc := &mockPaymentSvc{createOrderErr: service.ErrPlanNotAcceptingNew}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders", `{"plan_id":"quarterly","channel":"paypal"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestPaymentHandler_CreateOrder_PlanCurrencyMismatch(t *testing.T) {
	svc := &mockPaymentSvc{createOrderErr: service.ErrPlanCurrencyMismatch}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders", `{"plan_id":"monthly","channel":"paypal"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}
func TestPaymentHandler_CreateOrder_InternalError(t *testing.T) {
	svc := &mockPaymentSvc{createOrderErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders", `{"plan_id":"monthly","channel":"stripe"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_GetOrder_InternalError(t *testing.T) {
	svc := &mockPaymentSvc{getOrderErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodGet, "/payments/orders/o-1", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_CancelOrder_NotPending(t *testing.T) {
	svc := &mockPaymentSvc{cancelOrderErr: service.ErrOrderNotPending}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodDelete, "/payments/orders/o-1", "")
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestPaymentHandler_CancelOrder_NotFound(t *testing.T) {
	svc := &mockPaymentSvc{cancelOrderErr: service.ErrOrderNotFound}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodDelete, "/payments/orders/missing", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestPaymentHandler_CancelOrder_InternalError(t *testing.T) {
	svc := &mockPaymentSvc{cancelOrderErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodDelete, "/payments/orders/o-1", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_ConfirmOrder_OrderNotFound(t *testing.T) {
	svc := &mockPaymentSvc{confirmErr: service.ErrOrderNotFound}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders/o-1/confirm",
		`{"channel":"stripe","external_txn_id":"pi-x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestPaymentHandler_ConfirmOrder_ChannelMismatch(t *testing.T) {
	svc := &mockPaymentSvc{confirmErr: service.ErrOrderChannelMismatch}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders/o-1/confirm",
		`{"channel":"stripe","external_txn_id":"pi-x"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestPaymentHandler_ConfirmOrder_AlreadyTerminal(t *testing.T) {
	svc := &mockPaymentSvc{confirmErr: service.ErrOrderAlreadyTerminal}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders/o-1/confirm",
		`{"channel":"stripe","external_txn_id":"pi-x"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestPaymentHandler_ConfirmOrder_InternalError(t *testing.T) {
	svc := &mockPaymentSvc{confirmErr: errors.New("db boom")}
	g := paymentTestEngine(svc, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders/o-1/confirm",
		`{"channel":"stripe","external_txn_id":"pi-x"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestPaymentHandler_ConfirmOrder_BadJSON(t *testing.T) {
	g := paymentTestEngine(&mockPaymentSvc{}, "u-1")
	w := doPayRequest(g, http.MethodPost, "/payments/orders/o-1/confirm", "not json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateRefund_BadJSON(t *testing.T) {
	g := paymentTestEngine(&mockPaymentSvc{}, "u-1")
	w := doPayRequest(g, http.MethodPost, "/refunds", "not json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateRefund_MissingAuth(t *testing.T) {
	g := paymentTestEngine(&mockPaymentSvc{}, "")
	w := doPayRequest(g, http.MethodPost, "/refunds",
		`{"payment_id":"p-1","amount":1.0}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
}

func TestPaymentHandler_CreateRefund_MissingIdemKey(t *testing.T) {
	g := paymentTestEngine(&mockPaymentSvc{}, "u-1")
	w := doPayRequest(g, http.MethodPost, "/refunds",
		`{"payment_id":"p-1","amount":1.0}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateRefund_ShortIdemKey(t *testing.T) {
	g := paymentTestEngine(&mockPaymentSvc{}, "u-1")
	w := doPayRequestWithHeaders(g, http.MethodPost, "/refunds",
		`{"payment_id":"p-1","amount":1.0}`, map[string]string{
			"Authorization":   "Bearer x",
			"Idempotency-Key": "short",
		})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateRefund_BadIdemKeyChars(t *testing.T) {
	g := paymentTestEngine(&mockPaymentSvc{}, "u-1")
	w := doPayRequestWithHeaders(g, http.MethodPost, "/refunds",
		`{"payment_id":"p-1","amount":1.0}`, map[string]string{
			"Authorization":   "Bearer x",
			"Idempotency-Key": "valid-length-but-bad!chars",
		})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestPaymentHandler_CreateRefund_AllErrorTypes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ErrPlanNotFound", service.ErrPlanNotFound, 400},
		{"ErrPlanInactive", service.ErrPlanInactive, 400},
		{"ErrUserHasActiveSub", service.ErrUserHasActiveSub, 409},
		{"ErrOrderNotFound", service.ErrOrderNotFound, 404},
		{"ErrOrderNotPending", service.ErrOrderNotPending, 409},
		{"ErrOrderChannelMismatch", service.ErrOrderChannelMismatch, 409},
		{"ErrOrderAlreadyTerminal", service.ErrOrderAlreadyTerminal, 409},
		{"ErrPaymentNotFound", service.ErrPaymentNotFound, 404},
		{"ErrPaymentNotPaid", service.ErrPaymentNotPaid, 409},
		{"ErrRefundAmountInvalid", service.ErrRefundAmountInvalid, 400},
		{"ErrRefundSumExceedsPayment", service.ErrRefundSumExceedsPayment, 400},
		{"ErrRefundChannelFailed", service.ErrRefundChannelFailed, 502},
		{"ErrMissingIdempotencyKey", service.ErrMissingIdempotencyKey, 400},
		{"ErrInvalidChannel", service.ErrInvalidChannel, 400},
		{"generic", errors.New("db boom"), 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &mockPaymentSvc{refundErr: c.err}
			g := paymentTestEngine(svc, "u-1")
			w := doPayRequestWithHeaders(g, http.MethodPost, "/refunds",
				`{"payment_id":"p-1","amount":1.0}`, map[string]string{
					"Authorization":   "Bearer x",
					"Idempotency-Key": "valid-length-key-1",
				})
			if w.Code != c.want {
				t.Errorf("status=%d, want %d", w.Code, c.want)
			}
		})
	}
}
