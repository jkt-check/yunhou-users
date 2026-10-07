package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	billingpaddle "github.com/yunhou/users/internal/billing/paddle"
	"github.com/yunhou/users/internal/model"
)

// stubPaddle is the test double for the paddleClient interface.
type stubPaddle struct {
	txnID    string
	checkout string
	txnErr   error
	next     *time.Time
	nextErr  error
	calls    int
	gotPrice string
	gotCurr  string
	gotData  map[string]any

	cancelErr   error
	cancelCalls int
	cancelGotID string

	updateNext     *time.Time
	updateErr      error
	updateCalls    int
	updateGotID    string
	updateGotPrice string
}

func (s *stubPaddle) IsMockMode() bool { return false }
func (s *stubPaddle) ClientToken() string {
	return "test_client_token"
}
func (s *stubPaddle) CreateCheckoutTransaction(_ context.Context, priceID string, customData map[string]any, currency string) (*billingpaddle.CheckoutTransaction, error) {
	s.calls++
	s.gotPrice = priceID
	s.gotData = customData
	s.gotCurr = currency
	if s.txnErr != nil {
		return nil, s.txnErr
	}
	return &billingpaddle.CheckoutTransaction{TransactionID: s.txnID, CheckoutURL: s.checkout}, nil
}
func (s *stubPaddle) GetSubscriptionNextBilledAt(_ context.Context, _ string) (*time.Time, error) {
	return s.next, s.nextErr
}
func (s *stubPaddle) CancelSubscription(_ context.Context, subscriptionID string) error {
	s.cancelCalls++
	s.cancelGotID = subscriptionID
	return s.cancelErr
}
func (s *stubPaddle) UpdateSubscriptionPrice(_ context.Context, subscriptionID, priceID string) (*time.Time, error) {
	s.updateCalls++
	s.updateGotID = subscriptionID
	s.updateGotPrice = priceID
	return s.updateNext, s.updateErr
}

func TestValidateChannel_Paddle(t *testing.T) {
	if err := validateChannel("paddle"); err != nil {
		t.Fatalf("paddle must be a valid channel: %v", err)
	}
}

func TestChannelAutoRenews(t *testing.T) {
	for _, ch := range []string{"paypal", "paddle"} {
		if !channelAutoRenews(ch) {
			t.Fatalf("%s should auto-renew", ch)
		}
	}
	for _, ch := range []string{"stripe", "wechat_pay", "alipay", ""} {
		if channelAutoRenews(ch) {
			t.Fatalf("%s must not auto-renew", ch)
		}
	}
}

func TestProviderPreAuth_PaddleRequiresClient(t *testing.T) {
	orderRepo := &stubOrderRepoLookup{}
	svc := NewPaymentService(
		nil,
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{
			ID:                        "plan-1",
			Price:                     9.99,
			IsActive:                  true,
			AcceptingNewSubscriptions: true,
			Currency:                  "USD",
		}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
	if err := svc.providerPreAuth("paddle"); !errors.Is(err, ErrPaddleNotConfigured) {
		t.Fatalf("expected ErrPaddleNotConfigured, got %v", err)
	}
	if orderRepo.created != nil {
		t.Fatal("no order row may exist when the pre-auth gate fires")
	}
	svc.SetPaddleClient(&stubPaddle{})
	if err := svc.providerPreAuth("paddle"); err != nil {
		t.Fatalf("expected nil with client wired, got %v", err)
	}
}

func TestCreateOrder_Paddle_PersistsIntent(t *testing.T) {
	stub := &stubPaddle{
		txnID:    "txn_test_1",
		checkout: "https://yunhou.ai/checkout?_ptxn=txn_test_1",
	}
	orderRepo := &stubOrderRepoLookup{}
	svc := NewPaymentService(
		nil,
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{
			ID:                        "plan-1",
			Price:                     9.99,
			IsActive:                  true,
			AcceptingNewSubscriptions: true,
			Currency:                  "USD",
		}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"plan-1": "pri_test_1"})

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("CreateCheckoutTransaction called %d times, want 1", stub.calls)
	}
	if orderRepo.failSeen != "" {
		t.Fatalf("happy path must not flip the order to failed, got FailPending(%q)", orderRepo.failSeen)
	}
	if stub.gotPrice != "pri_test_1" {
		t.Fatalf("priceID = %q", stub.gotPrice)
	}
	if stub.gotCurr != "USD" {
		t.Fatalf("currency = %q, want USD", stub.gotCurr)
	}
	if stub.gotData["order_id"] != order.ID {
		t.Fatalf("custom_data.order_id = %v, want %s", stub.gotData["order_id"], order.ID)
	}
	if !orderRepo.updateIntentCalled {
		t.Fatal("UpdateProviderIntent was not called")
	}
	payload := string(orderRepo.updateIntentPayload)
	for _, want := range []string{`"transaction_id":"txn_test_1"`, `"checkout_url":"https://yunhou.ai/checkout?_ptxn=txn_test_1"`, `"client_token":"test_client_token"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("provider intent missing %s: %s", want, payload)
		}
	}
	if order.ProviderIntent == nil || !strings.Contains(string(*order.ProviderIntent), "txn_test_1") {
		t.Fatalf("order.ProviderIntent = %v", order.ProviderIntent)
	}
}

func TestCreateOrder_Paddle_MissingPrice(t *testing.T) {
	orderRepo := &stubOrderRepoLookup{}
	svc := NewPaymentService(
		nil,
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{
			ID:                        "plan-1",
			Price:                     9.99,
			IsActive:                  true,
			AcceptingNewSubscriptions: true,
			Currency:                  "USD",
		}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
	svc.SetPaddleClient(&stubPaddle{})
	// no SetPaddlePrices — operator forgot PADDLE_PRICES_JSON entry

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", nil)
	if !errors.Is(err, ErrPaddlePriceNotConfigured) {
		t.Fatalf("expected ErrPaddlePriceNotConfigured, got %v (order=%+v)", err, order)
	}
	// Pre-auth compensation: the orphan pending order must be flipped to
	// failed so the pending-order guard does not hold the user's retry
	// for the full ORDER_EXPIRY_DURATION window (2026-10-03 intl-prod
	// incident: misconfigured PADDLE_PRICES_JSON blocked a subscriber's
	// retries for 30 minutes).
	if orderRepo.created == nil {
		t.Fatal("expected the order row to be created before the price lookup")
	}
	if orderRepo.failSeen != orderRepo.created.ID {
		t.Fatalf("FailPending(%q), want the fresh order %s", orderRepo.failSeen, orderRepo.created.ID)
	}
}

// TestCreateOrder_Paddle_TransactionError_MarksOrderFailed covers the
// sibling failure: the Paddle API call itself fails (bad key, unknown
// price id, network). The fresh order must be flipped to failed as
// well, for the same reason as MissingPrice above.
func TestCreateOrder_Paddle_TransactionError_MarksOrderFailed(t *testing.T) {
	stub := &stubPaddle{txnErr: errors.New("paddle api: 401 unauthorized")}
	orderRepo := &stubOrderRepoLookup{}
	svc := NewPaymentService(
		nil,
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{
			ID:                        "plan-1",
			Price:                     9.99,
			IsActive:                  true,
			AcceptingNewSubscriptions: true,
			Currency:                  "USD",
		}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"plan-1": "pri_test_1"})

	_, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", nil)
	if err == nil || !strings.Contains(err.Error(), "paddle checkout transaction") {
		t.Fatalf("expected paddle checkout transaction error, got %v", err)
	}
	if orderRepo.created == nil {
		t.Fatal("expected the order row to be created before the transaction call")
	}
	if orderRepo.failSeen != orderRepo.created.ID {
		t.Fatalf("FailPending(%q), want the fresh order %s", orderRepo.failSeen, orderRepo.created.ID)
	}
}

// TestCreateOrder_Paddle_IntentPersistError_MarksOrderFailed: the Paddle
// transaction was minted channel-side but writing provider_intent failed.
// The fresh order must be flipped to failed; the minted transaction is
// unreachable (no URL leaked to the client), so it can never be paid.
func TestCreateOrder_Paddle_IntentPersistError_MarksOrderFailed(t *testing.T) {
	stub := &stubPaddle{txnID: "txn_persist_1", checkout: "https://yunhou.ai/checkout?_ptxn=txn_persist_1"}
	orderRepo := &stubOrderRepoLookup{updateIntentErr: errors.New("db down")}
	svc := NewPaymentService(
		nil,
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{
			ID:                        "plan-1",
			Price:                     9.99,
			IsActive:                  true,
			AcceptingNewSubscriptions: true,
			Currency:                  "USD",
		}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"plan-1": "pri_test_1"})

	_, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", nil)
	if err == nil || !strings.Contains(err.Error(), "persist provider intent") {
		t.Fatalf("expected persist provider intent error, got %v", err)
	}
	if orderRepo.failSeen != orderRepo.created.ID {
		t.Fatalf("FailPending(%q), want the fresh order %s", orderRepo.failSeen, orderRepo.created.ID)
	}
}

// TestCreateOrder_Paddle_PendingOrderRejected pins the pending-order guard
// for auto-renewing channels (2026-10 review): the active-sub guard only
// fires AFTER the first checkout settles, so without this check a user
// could open two hosted checkouts for the same product, pay both, and end
// up with two channel-side auto-renew subscriptions — the later payment
// overwrites external_subscription_id and the earlier one keeps charging
// with no local cancel handle. An unexpired pending order in the same
// product must reject the second CreateOrder with ErrUserHasPendingOrder
// (the handler maps it to 409 with "unfinished checkout" wording —
// distinct from the active-sub message since 2026-10-03).
func TestCreateOrder_Paddle_PendingOrderRejected(t *testing.T) {
	stub := &stubPaddle{txnID: "txn_pending_1", checkout: "https://yunhou.ai/checkout?_ptxn=txn_pending_1"}
	orderRepo := &stubOrderRepoLookup{
		pendingByProduct: &model.Order{ID: "ord-existing", UserID: "user-1", PlanID: "plan-1", Status: "pending"},
	}
	svc := NewPaymentService(
		nil,
		orderRepo,
		nil,
		nil,
		&stubSubRepo{},
		&stubPlanRepo{plan: &model.Plan{
			ID:                        "plan-1",
			Price:                     9.99,
			IsActive:                  true,
			AcceptingNewSubscriptions: true,
			Currency:                  "USD",
		}},
		nil,
		nil,
		nil,
		&stubRefundAPI{},
		nil,
		0,
	)
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"plan-1": "pri_test_1"})

	_, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", nil)
	if !errors.Is(err, ErrUserHasPendingOrder) {
		t.Fatalf("expected ErrUserHasPendingOrder, got %v", err)
	}
	if stub.calls != 0 {
		t.Fatal("CreateCheckoutTransaction must not run when a pending order blocks the request")
	}
	if orderRepo.created != nil {
		t.Fatal("no second order row may be created while a pending order exists")
	}
}

// M3: the order's sanitized attribution.last_touch UTM triple rides the
// Paddle checkout's custom_data (alongside the order_id the webhook
// lookup depends on), so channel-side records carry the marketing context.
// Only non-nil values are merged; first_touch stays out of custom_data.
func TestCreateOrder_Paddle_CustomData_CarriesLastTouchUTM(t *testing.T) {
	newSvc := func(stub *stubPaddle) *PaymentService {
		svc := NewPaymentService(
			nil,
			&stubOrderRepoLookup{},
			nil,
			nil,
			&stubSubRepo{},
			&stubPlanRepo{plan: &model.Plan{
				ID:                        "plan-1",
				Price:                     9.99,
				IsActive:                  true,
				AcceptingNewSubscriptions: true,
				Currency:                  "USD",
			}},
			nil,
			nil,
			nil,
			&stubRefundAPI{},
			nil,
			0,
		)
		svc.SetPaddleClient(stub)
		svc.SetPaddlePrices(map[string]string{"plan-1": "pri_test_1"})
		return svc
	}

	t.Run("last_touch utm params merged, nil fields omitted", func(t *testing.T) {
		stub := &stubPaddle{txnID: "txn_utm_1", checkout: "https://x/?_ptxn=txn_utm_1"}
		svc := newSvc(stub)
		attr := json.RawMessage(`{"first_touch":{"utm_source":"first"},"last_touch":{"utm_source":"google","utm_medium":"cpc","utm_campaign":null}}`)
		order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", attr)
		if err != nil {
			t.Fatalf("CreateOrder: %v", err)
		}
		if stub.gotData["order_id"] != order.ID {
			t.Errorf("custom_data order_id = %v, want %s", stub.gotData["order_id"], order.ID)
		}
		if stub.gotData["utm_source"] != "google" || stub.gotData["utm_medium"] != "cpc" {
			t.Errorf("custom_data utm = %v/%v, want google/cpc", stub.gotData["utm_source"], stub.gotData["utm_medium"])
		}
		if _, has := stub.gotData["utm_campaign"]; has {
			t.Errorf("utm_campaign was null — key must be omitted: %v", stub.gotData)
		}
		if len(stub.gotData) != 3 {
			t.Errorf("custom_data = %v, want exactly order_id + utm_source + utm_medium", stub.gotData)
		}
	})

	t.Run("no attribution → order_id only", func(t *testing.T) {
		stub := &stubPaddle{txnID: "txn_utm_2", checkout: "https://x/?_ptxn=txn_utm_2"}
		svc := newSvc(stub)
		if _, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle", nil); err != nil {
			t.Fatalf("CreateOrder: %v", err)
		}
		if len(stub.gotData) != 1 {
			t.Errorf("custom_data = %v, want only order_id", stub.gotData)
		}
		if _, has := stub.gotData["order_id"]; !has {
			t.Errorf("order_id missing from custom_data: %v", stub.gotData)
		}
	})
}

// paddleManagedSubID keys on the M1 channel column going forward; the sub_
// prefix remains as a fallback for pre-M1 rows whose channel is still NULL.
func TestPaddleManagedSubID_ChannelKeying(t *testing.T) {
	paddleCh, paypalCh, wechatCh := "paddle", "paypal", "wechat_pay"
	paddleExt, paypalExt := "sub_x1", "I-X1"
	cases := []struct {
		name    string
		sub     *model.Subscription
		wantID  string
		wantErr bool
	}{
		{"channel=paddle with sub_ id", &model.Subscription{Channel: &paddleCh, ExternalSubscriptionID: &paddleExt}, "sub_x1", false},
		{"channel=paypal rejected", &model.Subscription{Channel: &paypalCh, ExternalSubscriptionID: &paypalExt}, "", true},
		{"channel=wechat_pay rejected", &model.Subscription{Channel: &wechatCh, ExternalSubscriptionID: nil}, "", true},
		{"NULL channel falls back to sub_ prefix (legacy row)", &model.Subscription{ExternalSubscriptionID: &paddleExt}, "sub_x1", false},
		{"NULL channel + I- id rejected (legacy paypal row)", &model.Subscription{ExternalSubscriptionID: &paypalExt}, "", true},
		{"NULL channel + no external id rejected (local sub)", &model.Subscription{}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := paddleManagedSubID(tc.sub)
			if tc.wantErr {
				if !errors.Is(err, ErrSubscriptionNotChannelManaged) {
					t.Fatalf("expected ErrSubscriptionNotChannelManaged, got %v", err)
				}
				return
			}
			if err != nil || id != tc.wantID {
				t.Fatalf("got (%q, %v), want (%q, nil)", id, err, tc.wantID)
			}
		})
	}
}
