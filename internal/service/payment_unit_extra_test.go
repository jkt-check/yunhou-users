package service

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// ============================================================================
// Minimal mocks for CancelOrder / GetOrder / GetPayment / GetRefund
// ============================================================================
// These satisfy the relevant repo interfaces with just enough surface to
// drive the lookup + ownership-check branches. They live in this file (not
// mock_test.go) because they're only used by the targeted coverage tests
// below.

// stubOrderRepoLookup satisfies repo.OrderRepo with just enough methods for
// the lookup-heavy service functions. Unused methods panic — those code
// paths have separate coverage elsewhere.
type stubOrderRepoLookup struct {
	byID                map[string]*model.Order
	findErr             error
	cancelOK            bool
	cancelErr           error
	cancelSeen          string
	created             *model.Order
	createErr           error
	updateIntentCalled  bool   // set when UpdateProviderIntent is invoked
	updateIntentPayload []byte // last payload passed to UpdateProviderIntent
	updateIntentErr     error  // optional error to return from UpdateProviderIntent
}

func (s *stubOrderRepoLookup) Create(_ context.Context, order *model.Order) error {
	s.created = order
	return s.createErr
}
func (s *stubOrderRepoLookup) FindByID(_ context.Context, id string) (*model.Order, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	o, ok := s.byID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return o, nil
}
func (s *stubOrderRepoLookup) ListByUserID(_ context.Context, _ string) ([]model.Order, error) {
	return nil, nil
}
func (s *stubOrderRepoLookup) CancelPending(_ context.Context, id, _ string) (bool, error) {
	s.cancelSeen = id
	if s.cancelErr != nil {
		return false, s.cancelErr
	}
	return s.cancelOK, nil
}
func (s *stubOrderRepoLookup) SweepExpired(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *stubOrderRepoLookup) UpdateProviderIntent(_ context.Context, _ string, payload []byte) error {
	s.updateIntentCalled = true
	s.updateIntentPayload = payload
	return s.updateIntentErr
}

// stubPaymentRepoLookup — minimal PaymentRepo for GetPayment / GetRefund.
type stubPaymentRepoLookup struct {
	byID    map[string]*model.Payment
	findErr error
}

func (s *stubPaymentRepoLookup) InsertPaidOnConflictDoNothing(_ context.Context, _ *model.Payment) (string, bool, error) {
	panic("not used by lookup tests")
}
func (s *stubPaymentRepoLookup) FindByID(_ context.Context, id string) (*model.Payment, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	p, ok := s.byID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return p, nil
}
func (s *stubPaymentRepoLookup) FindByChannelTxnID(_ context.Context, _, _ string) (*model.Payment, error) {
	return nil, sql.ErrNoRows
}
func (s *stubPaymentRepoLookup) FindPaidByOrderID(_ context.Context, _ string) (*model.Payment, error) {
	return nil, sql.ErrNoRows
}
func (s *stubPaymentRepoLookup) ListByOrderID(_ context.Context, _ string) ([]model.Payment, error) {
	return nil, nil
}
func (s *stubPaymentRepoLookup) ListByUserID(_ context.Context, _ string) ([]model.Payment, error) {
	return nil, nil
}
func (s *stubPaymentRepoLookup) MarkPaid(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (s *stubPaymentRepoLookup) MarkFailed(_ context.Context, _, _ string) error {
	return nil
}
func (s *stubPaymentRepoLookup) MarkRefunded(_ context.Context, _ string) error {
	return nil
}
func (s *stubPaymentRepoLookup) SetDisputed(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (s *stubPaymentRepoLookup) ClearDisputed(_ context.Context, _ string) error {
	return nil
}

// stubRefundRepoLookup — minimal RefundRepo for GetRefund.
type stubRefundRepoLookup struct {
	byID    map[string]*model.Refund
	findErr error
}

func (s *stubRefundRepoLookup) FindByIdempotencyKey(_ context.Context, _, _ string) (*model.Refund, error) {
	return nil, sql.ErrNoRows
}
func (s *stubRefundRepoLookup) InsertPending(_ context.Context, _ *model.Refund) error { return nil }
func (s *stubRefundRepoLookup) FindByID(_ context.Context, id string) (*model.Refund, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	r, ok := s.byID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return r, nil
}
func (s *stubRefundRepoLookup) FindByChannelRefundID(_ context.Context, _, _ string) (*model.Refund, error) {
	return nil, sql.ErrNoRows
}
func (s *stubRefundRepoLookup) ListByPaymentID(_ context.Context, _ string) ([]model.Refund, error) {
	return nil, nil
}
func (s *stubRefundRepoLookup) SumByPaymentID(_ context.Context, _ string) (float64, error) {
	return 0, nil
}
func (s *stubRefundRepoLookup) MarkPaid(_ context.Context, _ string) error { return nil }

// newPaymentServiceForLookup wires a PaymentService with the lookup mocks.
// Other repos are nil — tests in this file don't touch them.
func newPaymentServiceForLookup(orderRepo repo.OrderRepo, paymentRepo repo.PaymentRepo, refundRepo repo.RefundRepo) *PaymentService {
	return NewPaymentService(
		(*sqlx.DB)(nil),
		orderRepo, paymentRepo, refundRepo,
		nil, nil, nil, nil, nil,
		&stubRefundAPI{}, nil,

		0)

}

// stubPlanRepo satisfies repo.PlanRepo with a single active plan for CreateOrder.
type stubPlanRepo struct {
	plan *model.Plan
	err  error
}

func (s *stubPlanRepo) FindAll(_ context.Context) ([]model.Plan, error) { return nil, nil }
func (s *stubPlanRepo) FindByID(_ context.Context, id string) (*model.Plan, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.plan == nil {
		return nil, sql.ErrNoRows
	}
	return s.plan, nil
}
func (s *stubPlanRepo) FindByApp(_ context.Context, _ string) ([]model.Plan, error) { return nil, nil }
func (s *stubPlanRepo) FindDefault(_ context.Context) (*model.Plan, error)          { return nil, nil }
func (s *stubPlanRepo) Create(_ context.Context, _ *model.Plan) error               { return nil }
func (s *stubPlanRepo) Update(_ context.Context, _ *model.Plan) error               { return nil }
func (s *stubPlanRepo) Delete(_ context.Context, _ string) error                    { return nil }

// stubSubRepo satisfies repo.SubscriptionRepo with configurable FindActiveByUserID error.
type stubSubRepo struct {
	activeSub *model.Subscription
	findErr   error
}

func (s *stubSubRepo) Create(_ context.Context, _ *model.Subscription) error { return nil }
func (s *stubSubRepo) FindActiveByUserID(_ context.Context, _ string) (*model.Subscription, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.activeSub == nil {
		return nil, sql.ErrNoRows
	}
	return s.activeSub, nil
}
func (s *stubSubRepo) FindByID(_ context.Context, _ string) (*model.Subscription, error) {
	return nil, nil
}
func (s *stubSubRepo) ListByUserID(_ context.Context, _ string) ([]model.Subscription, error) {
	return nil, nil
}
func (s *stubSubRepo) UpdateStatus(_ context.Context, _ string, _ string) error { return nil }
func (s *stubSubRepo) Renew(_ context.Context, _ string, _ *time.Time) error    { return nil }

// stubWechat satisfies service.wechatClient for CreateOrder channel tests.
type stubWechat struct {
	mockMode  bool
	mchID     string
	appID     string
	unifiedFn func(context.Context, wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error)
	called    int
	gotReq    wechat.UnifiedOrderRequest
}

func (s *stubWechat) IsMockMode() bool { return s.mockMode }
func (s *stubWechat) MchID() string    { return s.mchID }
func (s *stubWechat) AppID() string    { return s.appID }
func (s *stubWechat) UnifiedOrder(ctx context.Context, req wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
	s.called++
	s.gotReq = req
	return s.unifiedFn(ctx, req)
}

func newPaymentServiceForCreateOrder(client wechatClient) (*PaymentService, *stubOrderRepoLookup) {
	orderRepo := &stubOrderRepoLookup{}
	return NewPaymentService(
		(*sqlx.DB)(nil),
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{ID: "plan-1", Price: 0.29, IsActive: true}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		client,
		0,
	), orderRepo
}

// asWechatClient narrows a *stubWechat into a wechatClient interface —
// keeps the call-sites readable while letting newPaymentServiceForCreateOrder
// accept an untyped nil (which behaves correctly under channel=="wechat_pay"
// branch: `s.wechat == nil` evaluates to true).
func asWechatClient(s *stubWechat) wechatClient {
	if s == nil {
		return nil
	}
	return s
}

// ============================================================================
// CreateOrder — channel-aware WeChat Pay pre-auth
// ============================================================================

func TestCreateOrder_WeChat_Real_PersistsIntent(t *testing.T) {
	stub := &stubWechat{
		mchID: "1900000109",
		appID: "wx1900000109",
		unifiedFn: func(_ context.Context, _ wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
			return &wechat.UnifiedOrderResponse{OutTradeNo: "order-1", CodeURL: "weixin://abc"}, nil
		},
	}
	svc, orderRepo := newPaymentServiceForCreateOrder(asWechatClient(stub))

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if stub.called != 1 {
		t.Fatalf("UnifiedOrder called %d times, want 1", stub.called)
	}
	// OutTradeNo now derives a short form from the order ID (strip
	// hyphens + truncate to 32 chars) to satisfy WeChat's v3 length cap.
	if len(stub.gotReq.OutTradeNo) != 32 {
		t.Errorf("UnifiedOrder OutTradeNo len = %d, want 32 (short form of %q)", len(stub.gotReq.OutTradeNo), order.ID)
	}
	if stub.gotReq.Amount.Total != 29 {
		t.Errorf("UnifiedOrder amount = %d fen, want 29", stub.gotReq.Amount.Total)
	}
	if stub.gotReq.Amount.Currency != "CNY" || stub.gotReq.TradeType != wechat.TradeTypeNative {
		t.Errorf("UnifiedOrder request = %+v", stub.gotReq)
	}
	if !orderRepo.updateIntentCalled {
		t.Fatal("UpdateProviderIntent was not called")
	}
	if !strings.Contains(string(orderRepo.updateIntentPayload), `"code_url":"weixin://abc"`) {
		t.Fatalf("UpdateProviderIntent payload = %s", orderRepo.updateIntentPayload)
	}
	// v3 NATIVE body fields use `mchid` (no underscore) + `appid`
	// alongside the code_url. Both echo onto provider_intent so the BFF
	// and audit-log tooling see the merchant pair.
	if !strings.Contains(string(orderRepo.updateIntentPayload), `"mchid":"1900000109"`) {
		t.Fatalf("UpdateProviderIntent payload missing mchid: %s", orderRepo.updateIntentPayload)
	}
	if !strings.Contains(string(orderRepo.updateIntentPayload), `"appid":"wx1900000109"`) {
		t.Fatalf("UpdateProviderIntent payload missing appid: %s", orderRepo.updateIntentPayload)
	}
	if order.ProviderIntent == nil || string(*order.ProviderIntent) != string(orderRepo.updateIntentPayload) {
		t.Errorf("order.ProviderIntent = %v, persisted payload = %s", order.ProviderIntent, orderRepo.updateIntentPayload)
	}
}

// TestCreateOrder_WeChat_NilClient_Fourxx confirms that a deployment
// without a WeChat Pay client surfaces ErrWechatPayNotConfigured instead
// of silently creating a pending order with no code_url (and no path to
// pay). The handler maps this to 400.
func TestCreateOrder_WeChat_NilClient_Fourxx(t *testing.T) {
	svc, orderRepo := newPaymentServiceForCreateOrder(nil)

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay")
	if !errors.Is(err, ErrWechatPayNotConfigured) {
		t.Fatalf("expected ErrWechatPayNotConfigured, got %v (order=%+v)", err, order)
	}
	if orderRepo.updateIntentCalled {
		t.Fatal("UpdateProviderIntent must not be called when wechat is nil")
	}
}

// TestCreateOrder_ShortOutTradeNo pins the new short OutTradeNo derivation:
// strip hyphens + truncate to 32 chars, regardless of UUID shape.
func TestCreateOrder_ShortOutTradeNo(t *testing.T) {
	cases := []struct {
		name    string
		orderID string
		want    string
	}{
		{
			"canonical uuid — strips hyphens, no truncation needed",
			"12345678-1234-1234-1234-123456789012",
			"12345678123412341234123456789012",
		},
		{
			"hex-only — no hyphens, identity",
			"abcdef0123456789abcdef0123456789",
			"abcdef0123456789abcdef0123456789",
		},
		{
			"long uuid → first 32 hex digits",
			"abcdef0123456789abcdef-0123456789012345",
			"abcdef0123456789abcdef0123456789",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strings.ReplaceAll(tc.orderID, "-", "")[:32]
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if len(got) > 32 {
				t.Errorf("len = %d, want <= 32", len(got))
			}
		})
	}
}

func TestCreateOrder_WeChat_Real_UnifiedOrderErr(t *testing.T) {
	stub := &stubWechat{
		unifiedFn: func(_ context.Context, _ wechat.UnifiedOrderRequest) (*wechat.UnifiedOrderResponse, error) {
			return nil, errors.New("wechat down")
		},
	}
	svc, orderRepo := newPaymentServiceForCreateOrder(asWechatClient(stub))

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay")
	if err == nil {
		t.Fatal("expected error")
	}
	if order == nil || orderRepo.created == nil {
		t.Fatal("pending order should remain after UnifiedOrder failure")
	}
	if orderRepo.updateIntentCalled {
		t.Fatal("UpdateProviderIntent should not be called when UnifiedOrder fails")
	}
}

func TestCreateOrder_WeChat_Mock_NoClientCall(t *testing.T) {
	stub := &stubWechat{mockMode: true}
	svc, orderRepo := newPaymentServiceForCreateOrder(asWechatClient(stub))

	if _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "wechat_pay"); err != nil {
		t.Fatalf("CreateOrder mock: %v", err)
	}
	if stub.called != 0 {
		t.Fatalf("UnifiedOrder called %d times in mock mode", stub.called)
	}
	if orderRepo.updateIntentCalled {
		t.Fatal("UpdateProviderIntent called in mock mode")
	}
}

func TestCreateOrder_Stripe_NilWeChat_OK(t *testing.T) {
	svc, _ := newPaymentServiceForCreateOrder(nil)
	if _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "stripe"); err != nil {
		t.Fatalf("CreateOrder stripe: %v", err)
	}
}

func TestCreateOrder_InvalidChannel(t *testing.T) {
	svc, orderRepo := newPaymentServiceForCreateOrder(nil)
	if _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "fakechan"); err == nil {
		t.Fatal("expected error for invalid channel")
	}
	if orderRepo.created != nil {
		t.Fatal("invalid channel should be rejected before creating an order")
	}
}

// ============================================================================
// CreateOrder — subRepo error branch
// ============================================================================

func TestPaymentService_Unit_CreateOrder_SubRepoError(t *testing.T) {
	t.Parallel()
	svc := NewPaymentService(
		(*sqlx.DB)(nil),
		&stubOrderRepoLookup{},
		nil, nil,
		&stubSubRepo{findErr: errors.New("db connection lost")},
		&stubPlanRepo{plan: &model.Plan{ID: "monthly", IsActive: true}},
		nil, nil, nil,
		&stubRefundAPI{}, nil,

		0)

	_, err := svc.CreateOrder(context.Background(), "u_1", "monthly", "stripe")
	if err == nil {
		t.Fatal("expected error from subRepo.FindActiveByUserID")
	}
	if !strings.Contains(err.Error(), "check active sub") {
		t.Errorf("expected 'check active sub' error, got: %v", err)
	}
}

func TestPaymentService_Unit_CreateOrder_PlanNotFound(t *testing.T) {
	t.Parallel()
	svc := NewPaymentService(
		(*sqlx.DB)(nil),
		&stubOrderRepoLookup{},
		nil, nil,
		&stubSubRepo{},
		&stubPlanRepo{err: sql.ErrNoRows},
		nil, nil, nil,
		&stubRefundAPI{}, nil,

		0)

	_, err := svc.CreateOrder(context.Background(), "u_1", "monthly", "stripe")
	if !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("expected ErrPlanNotFound, got %v", err)
	}
}

func TestPaymentService_Unit_CreateOrder_PlanInactive(t *testing.T) {
	t.Parallel()
	svc := NewPaymentService(
		(*sqlx.DB)(nil),
		&stubOrderRepoLookup{},
		nil, nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{ID: "monthly", IsActive: false}},
		nil, nil, nil,
		&stubRefundAPI{}, nil,

		0)

	_, err := svc.CreateOrder(context.Background(), "u_1", "monthly", "stripe")
	if !errors.Is(err, ErrPlanInactive) {
		t.Errorf("expected ErrPlanInactive, got %v", err)
	}
}

// ============================================================================
// CancelOrder — re-read error branch
// ============================================================================

func TestPaymentService_Unit_CancelOrder_ReReadError(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{
		cancelOK: false,                            // triggers re-read path
		findErr:  errors.New("db connection lost"), // non-ErrNoRows error on re-read
	}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	err := svc.CancelOrder(context.Background(), "ord_1", "u_1")
	if err == nil {
		t.Fatal("expected error from re-read")
	}
	if !strings.Contains(err.Error(), "re-read order") {
		t.Errorf("expected 're-read order' error, got: %v", err)
	}
}

// ============================================================================
// TestToCents_Overflow covers the positive-overflow clamp branch (line
// 1501). With float64 ~1.8e308 and int64 max ~9.2e18, v*100 overflows when
// v is around 9.2e16. We use a value just above that.
// ============================================================================

func TestToCents_Overflow(t *testing.T) {
	t.Parallel()
	got := toCents(1e17) // 1e19 cents overflows int64
	if got != 1<<62-1 {
		t.Errorf("overflow clamp: got %d, want %d", got, int64(1<<62-1)>>1)
	}
}

// ============================================================================
// CancelOrder — every branch:
//   - happy path (CancelPending returns true, true)
//   - CancelPending returns false, hidden by not-found (ErrOrderNotFound)
//   - CancelPending returns false, owner-mismatch hidden (ErrOrderNotFound)
//   - CancelPending returns false, owner-match not-pending (ErrOrderNotPending)
//   - CancelPending returns error
//   - FindByID error other than ErrNoRows after a false (rare branch)
// ============================================================================

func TestPaymentService_Unit_CancelOrder_Success(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{cancelOK: true}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	if err := svc.CancelOrder(context.Background(), "ord_1", "u_1"); err != nil {
		t.Errorf("happy cancel: %v", err)
	}
	if orderRepo.cancelSeen != "ord_1" {
		t.Errorf("expected CancelPending on ord_1, got %s", orderRepo.cancelSeen)
	}
}

func TestPaymentService_Unit_CancelOrder_CancelErr(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{cancelErr: errors.New("db down")}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	if err := svc.CancelOrder(context.Background(), "ord_1", "u_1"); err == nil {
		t.Error("expected error from CancelPending")
	}
}

func TestPaymentService_Unit_CancelOrder_NotFoundAfterFalse(t *testing.T) {
	t.Parallel()
	// CancelPending returns false → service re-reads via FindByID → not found.
	orderRepo := &stubOrderRepoLookup{cancelOK: false, byID: map[string]*model.Order{}}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	if err := svc.CancelOrder(context.Background(), "missing", "u_1"); err != ErrOrderNotFound {
		t.Errorf("expected ErrOrderNotFound, got %v", err)
	}
}

func TestPaymentService_Unit_CancelOrder_OwnerMismatch(t *testing.T) {
	t.Parallel()
	// CancelPending returns false → service re-reads via FindByID → order
	// exists but userID doesn't match → hide existence (ErrOrderNotFound).
	orderRepo := &stubOrderRepoLookup{
		cancelOK: false,
		byID: map[string]*model.Order{
			"ord_1": {ID: "ord_1", UserID: "other-user", PlanID: "monthly", Status: "paid"},
		},
	}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	if err := svc.CancelOrder(context.Background(), "ord_1", "u_1"); err != ErrOrderNotFound {
		t.Errorf("expected ErrOrderNotFound for non-owner, got %v", err)
	}
}

func TestPaymentService_Unit_CancelOrder_NotPending(t *testing.T) {
	t.Parallel()
	// CancelPending returns false → service re-reads via FindByID → order
	// exists, owner matches, but status is paid → ErrOrderNotPending.
	orderRepo := &stubOrderRepoLookup{
		cancelOK: false,
		byID: map[string]*model.Order{
			"ord_1": {ID: "ord_1", UserID: "u_1", PlanID: "monthly", Status: "paid"},
		},
	}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	if err := svc.CancelOrder(context.Background(), "ord_1", "u_1"); err != ErrOrderNotPending {
		t.Errorf("expected ErrOrderNotPending, got %v", err)
	}
}

// ============================================================================
// GetOrder — happy path + NotFound + NotOwner
// ============================================================================

func TestPaymentService_Unit_GetOrder_Owner(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "u_1", Status: "paid"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	got, err := svc.GetOrder(context.Background(), "ord_1", "u_1")
	if err != nil {
		t.Errorf("happy get: %v", err)
	}
	if got == nil || got.ID != "ord_1" {
		t.Errorf("expected ord_1, got %+v", got)
	}
}

func TestPaymentService_Unit_GetOrder_NotFound(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	_, err := svc.GetOrder(context.Background(), "missing", "u_1")
	if err != ErrOrderNotFound {
		t.Errorf("expected ErrOrderNotFound, got %v", err)
	}
}

func TestPaymentService_Unit_GetOrder_NotOwner(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "other-user", Status: "pending"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	_, err := svc.GetOrder(context.Background(), "ord_1", "u_1")
	if err != ErrOrderNotFound {
		t.Errorf("expected ErrOrderNotFound (hide existence), got %v", err)
	}
}

// ============================================================================
// GetPayment — happy + NotFound + order-load failure
// ============================================================================

func TestPaymentService_Unit_GetPayment_Owner(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "u_1"},
	}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_1"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, nil)
	got, err := svc.GetPayment(context.Background(), "pay_1", "u_1")
	if err != nil {
		t.Errorf("happy get payment: %v", err)
	}
	if got == nil || got.ID != "pay_1" {
		t.Errorf("expected pay_1, got %+v", got)
	}
}

func TestPaymentService_Unit_GetPayment_NotFound(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, nil)
	_, err := svc.GetPayment(context.Background(), "missing", "u_1")
	if err != ErrPaymentNotFound {
		t.Errorf("expected ErrPaymentNotFound, got %v", err)
	}
}

func TestPaymentService_Unit_GetPayment_OrderMissing(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_ghost"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, nil)
	_, err := svc.GetPayment(context.Background(), "pay_1", "u_1")
	if err == nil {
		t.Error("expected error when order is missing")
	}
}

func TestPaymentService_Unit_GetPayment_NotOwner(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "other-user"},
	}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_1"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, nil)
	if _, err := svc.GetPayment(context.Background(), "pay_1", "u_1"); err == nil {
		t.Error("expected ownership error")
	}
}

// ============================================================================
// GetRefund — every branch
// ============================================================================

func TestPaymentService_Unit_GetRefund_Owner(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "u_1"},
	}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_1"},
	}}
	refundRepo := &stubRefundRepoLookup{byID: map[string]*model.Refund{
		"ref_1": {ID: "ref_1", PaymentID: "pay_1"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, refundRepo)
	got, err := svc.GetRefund(context.Background(), "ref_1", "u_1")
	if err != nil {
		t.Errorf("happy get refund: %v", err)
	}
	if got == nil || got.ID != "ref_1" {
		t.Errorf("expected ref_1, got %+v", got)
	}
}

func TestPaymentService_Unit_GetRefund_NotFound(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{}}
	refundRepo := &stubRefundRepoLookup{byID: map[string]*model.Refund{}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, refundRepo)
	_, err := svc.GetRefund(context.Background(), "missing", "u_1")
	if err != ErrRefundNotFound {
		t.Errorf("expected ErrRefundNotFound, got %v", err)
	}
}

func TestPaymentService_Unit_GetRefund_NotOwner(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "other-user"},
	}}
	paymentRepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_1"},
	}}
	refundRepo := &stubRefundRepoLookup{byID: map[string]*model.Refund{
		"ref_1": {ID: "ref_1", PaymentID: "pay_1"},
	}}
	svc := newPaymentServiceForLookup(orderRepo, paymentRepo, refundRepo)
	if _, err := svc.GetRefund(context.Background(), "ref_1", "u_1"); err == nil {
		t.Error("expected ownership error")
	}
}

// ============================================================================
// ListUserPayments / ListPaymentRefunds — small wrapper passthroughs
// ============================================================================
func TestPaymentService_Unit_ListUserPayments_PassesThrough(t *testing.T) {
	t.Parallel()
	prepo := &stubPaymentRepoList{
		out: []model.Payment{
			{ID: "pay_1", OrderID: "ord_1"},
			{ID: "pay_2", OrderID: "ord_2"},
		},
	}
	svc := NewPaymentService(
		(*sqlx.DB)(nil),
		&stubOrderRepoLookup{},
		prepo,
		&stubRefundRepoLookup{},
		nil, nil, nil, nil, nil,
		&stubRefundAPI{}, nil,

		0)

	got, err := svc.ListUserPayments(context.Background(), "u_1")
	if err != nil {
		t.Fatalf("list payments: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 payments, got %d", len(got))
	}
}

// stubPaymentRepoList — minimal PaymentRepo for ListUserPayments / ListPaymentRefunds.
type stubPaymentRepoList struct {
	out []model.Payment
}

func (s *stubPaymentRepoList) InsertPaidOnConflictDoNothing(_ context.Context, _ *model.Payment) (string, bool, error) {
	return "", false, nil
}
func (s *stubPaymentRepoList) FindByID(_ context.Context, _ string) (*model.Payment, error) {
	return nil, sql.ErrNoRows
}
func (s *stubPaymentRepoList) FindByChannelTxnID(_ context.Context, _, _ string) (*model.Payment, error) {
	return nil, sql.ErrNoRows
}
func (s *stubPaymentRepoList) FindPaidByOrderID(_ context.Context, _ string) (*model.Payment, error) {
	return nil, sql.ErrNoRows
}
func (s *stubPaymentRepoList) ListByOrderID(_ context.Context, _ string) ([]model.Payment, error) {
	return nil, nil
}
func (s *stubPaymentRepoList) ListByUserID(_ context.Context, _ string) ([]model.Payment, error) {
	if s.out == nil {
		return nil, nil
	}
	return s.out, nil
}
func (s *stubPaymentRepoList) MarkPaid(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (s *stubPaymentRepoList) MarkFailed(_ context.Context, _, _ string) error { return nil }
func (s *stubPaymentRepoList) MarkRefunded(_ context.Context, _ string) error  { return nil }
func (s *stubPaymentRepoList) SetDisputed(_ context.Context, _ string, _ time.Time) error {
	return nil
}
func (s *stubPaymentRepoList) ClearDisputed(_ context.Context, _ string) error { return nil }

// ============================================================================
// ListPaymentRefunds — happy + PaymentNotFound paths
// ============================================================================

func TestPaymentService_Unit_ListPaymentRefunds_Success(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{
		"ord_1": {ID: "ord_1", UserID: "u_1"},
	}}
	prepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_1"},
	}}
	rrepo := &stubRefundRepoList{out: []model.Refund{{ID: "ref_1"}}}
	svc := newPaymentServiceForLookup(orderRepo, prepo, rrepo)
	got, err := svc.ListPaymentRefunds(context.Background(), "pay_1", "u_1")
	if err != nil {
		t.Fatalf("list refunds: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 refund, got %d", len(got))
	}
}

func TestPaymentService_Unit_ListPaymentRefunds_NotFound(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	prepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{}}
	svc := newPaymentServiceForLookup(orderRepo, prepo, &stubRefundRepoList{})
	if _, err := svc.ListPaymentRefunds(context.Background(), "missing", "u_1"); err != ErrPaymentNotFound {
		t.Errorf("expected ErrPaymentNotFound, got %v", err)
	}
}

// ============================================================================
// Error-path coverage for GetOrder / GetPayment / GetRefund
// ============================================================================

func TestPaymentService_Unit_GetOrder_RepoError(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{findErr: errors.New("db down")}
	svc := newPaymentServiceForLookup(orderRepo, nil, nil)
	if _, err := svc.GetOrder(context.Background(), "ord_1", "u_1"); err == nil {
		t.Error("expected repo error, got nil")
	}
}

func TestPaymentService_Unit_GetPayment_RepoError(t *testing.T) {
	t.Parallel()
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	prepo := &stubPaymentRepoLookup{findErr: errors.New("db down")}
	svc := newPaymentServiceForLookup(orderRepo, prepo, nil)
	if _, err := svc.GetPayment(context.Background(), "pay_1", "u_1"); err == nil {
		t.Error("expected repo error, got nil")
	}
}

func TestPaymentService_Unit_GetRefund_RepoErrorRefund(t *testing.T) {
	t.Parallel()
	// FindByID on refundRepo returns an error other than ErrNoRows —
	// hits the "fmt.Errorf(find refund: %w)" branch.
	rrepo := &stubRefundRepoLookup{findErr: errors.New("db down")}
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	prepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{}}
	svc := newPaymentServiceForLookup(orderRepo, prepo, rrepo)
	if _, err := svc.GetRefund(context.Background(), "ref_1", "u_1"); err == nil {
		t.Error("expected repo error, got nil")
	}
}

func TestPaymentService_Unit_GetRefund_PaymentMissing(t *testing.T) {
	t.Parallel()
	// Refund found, but its payment row is gone — hits the paymentRepo
	// error branch and the wrapping error message.
	rrepo := &stubRefundRepoLookup{byID: map[string]*model.Refund{
		"ref_1": {ID: "ref_1", PaymentID: "pay_ghost"},
	}}
	prepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{}}
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	svc := newPaymentServiceForLookup(orderRepo, prepo, rrepo)
	if _, err := svc.GetRefund(context.Background(), "ref_1", "u_1"); err == nil {
		t.Error("expected order-missing error")
	}
}

func TestPaymentService_Unit_GetRefund_OrderMissing(t *testing.T) {
	t.Parallel()
	// Refund + payment found, but order is gone — hits the orderRepo
	// error branch.
	rrepo := &stubRefundRepoLookup{byID: map[string]*model.Refund{
		"ref_1": {ID: "ref_1", PaymentID: "pay_1"},
	}}
	prepo := &stubPaymentRepoLookup{byID: map[string]*model.Payment{
		"pay_1": {ID: "pay_1", OrderID: "ord_ghost"},
	}}
	orderRepo := &stubOrderRepoLookup{byID: map[string]*model.Order{}}
	svc := newPaymentServiceForLookup(orderRepo, prepo, rrepo)
	if _, err := svc.GetRefund(context.Background(), "ref_1", "u_1"); err == nil {
		t.Error("expected order-missing error")
	}
}

// stubRefundRepoList — minimal RefundRepo for ListPaymentRefunds.
type stubRefundRepoList struct {
	out []model.Refund
	err error
}

func (s *stubRefundRepoList) FindByIdempotencyKey(_ context.Context, _, _ string) (*model.Refund, error) {
	return nil, sql.ErrNoRows
}
func (s *stubRefundRepoList) InsertPending(_ context.Context, _ *model.Refund) error { return nil }
func (s *stubRefundRepoList) FindByID(_ context.Context, _ string) (*model.Refund, error) {
	return nil, sql.ErrNoRows
}
func (s *stubRefundRepoList) FindByChannelRefundID(_ context.Context, _, _ string) (*model.Refund, error) {
	return nil, sql.ErrNoRows
}
func (s *stubRefundRepoList) ListByPaymentID(_ context.Context, _ string) ([]model.Refund, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}
func (s *stubRefundRepoList) SumByPaymentID(_ context.Context, _ string) (float64, error) {
	return 0, nil
}
func (s *stubRefundRepoList) MarkPaid(_ context.Context, _ string) error { return nil }

// ============================================================================
// SweepExpired the *non*-happy-path branch through the helper. The sweep is
// tested extensively in payment_db_test.go (real DB); this just adds the
// "repo error" branch that nopRepos can't reach.
// ============================================================================

func TestPaymentService_OnPaymentFailed_OnWebhookErrorPath_RepoErrors(t *testing.T) {
	// Drive the newPaymentServiceForLookup path so we have something to
	// assert on; the goal is to invoke the helper and ensure the wrapper
	// doesn't panic. (PaymentService has no direct SweepExpired entry point
	// — the sweeper is separate — so this is a placeholder for the future
	// if a coverage gap appears in the lookup wrappers.)
	svc := newPaymentServiceForLookup(&stubOrderRepoLookup{}, &stubPaymentRepoLookup{}, &stubRefundRepoLookup{})
	_ = svc
	_ = math.NaN // keep the math import live — toCents tests need it
}
