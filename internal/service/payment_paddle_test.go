package service

import (
	"context"
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

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle")
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("CreateCheckoutTransaction called %d times, want 1", stub.calls)
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

	order, err := svc.CreateOrder(context.Background(), "user-1", "plan-1", "paddle")
	if !errors.Is(err, ErrPaddlePriceNotConfigured) {
		t.Fatalf("expected ErrPaddlePriceNotConfigured, got %v (order=%+v)", err, order)
	}
}
