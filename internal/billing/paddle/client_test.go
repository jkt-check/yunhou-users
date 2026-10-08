package paddle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	paddle "github.com/PaddleHQ/paddle-go-sdk/v5"
	paddleerr "github.com/PaddleHQ/paddle-go-sdk/v5/pkg/paddleerr"
)

func TestNewClient_UnknownEnv(t *testing.T) {
	if _, err := NewClient("key", "staging"); err == nil {
		t.Fatal("expected error for unknown env")
	}
}

func TestMock_CreateCheckoutTransaction(t *testing.T) {
	c := &Client{MockMode: true}
	res, err := c.CreateCheckoutTransaction(context.Background(), "pri_x", map[string]any{"order_id": "o1"}, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.TransactionID, "txn_mock_") {
		t.Fatalf("unexpected txn id %q", res.TransactionID)
	}
	if !strings.Contains(res.CheckoutURL, "_ptxn="+res.TransactionID) {
		t.Fatalf("checkout url missing _ptxn: %q", res.CheckoutURL)
	}
}

func TestReal_CreateCheckoutTransaction_RequiresSDK(t *testing.T) {
	c := &Client{}
	if _, err := c.CreateCheckoutTransaction(context.Background(), "pri_x", nil, "USD"); err == nil {
		t.Fatal("expected error when SDK not wired")
	}
}

func TestMock_GetSubscriptionNextBilledAt(t *testing.T) {
	c := &Client{MockMode: true}
	before := time.Now()
	at, err := c.GetSubscriptionNextBilledAt(context.Background(), "sub_x")
	if err != nil {
		t.Fatal(err)
	}
	if at == nil || !at.After(before.AddDate(0, 1, 0).Add(-time.Minute)) {
		t.Fatalf("expected ~1 month in the future, got %v", at)
	}
}

func TestReal_GetSubscriptionNextBilledAt_RequiresSDK(t *testing.T) {
	c := &Client{}
	if _, err := c.GetSubscriptionNextBilledAt(context.Background(), "sub_x"); err == nil {
		t.Fatal("expected error when SDK not wired")
	}
}

func TestClientToken_RoundTrip(t *testing.T) {
	c := &Client{}
	if c.ClientToken() != "" {
		t.Fatalf("zero value should have empty token, got %q", c.ClientToken())
	}
	c.SetClientToken("test_xxx")
	if c.ClientToken() != "test_xxx" {
		t.Fatalf("token round trip failed: %q", c.ClientToken())
	}
}

func TestMock_CancelSubscription(t *testing.T) {
	c := &Client{MockMode: true}
	if err := c.CancelSubscription(context.Background(), "sub_x"); err != nil {
		t.Fatal(err)
	}
}

func TestReal_CancelSubscription_RequiresSDK(t *testing.T) {
	c := &Client{}
	if err := c.CancelSubscription(context.Background(), "sub_x"); err == nil {
		t.Fatal("expected error when SDK not wired")
	}
}

func TestMock_UpdateSubscriptionPrice(t *testing.T) {
	c := &Client{MockMode: true}
	before := time.Now()
	at, err := c.UpdateSubscriptionPrice(context.Background(), "sub_x", "pri_yearly")
	if err != nil {
		t.Fatal(err)
	}
	if at == nil || !at.After(before.AddDate(0, 11, 0)) {
		t.Fatalf("expected ~1 year in the future, got %v", at)
	}
}

func TestReal_UpdateSubscriptionPrice_RequiresSDK(t *testing.T) {
	c := &Client{}
	if _, err := c.UpdateSubscriptionPrice(context.Background(), "sub_x", "pri_yearly"); err == nil {
		t.Fatal("expected error when SDK not wired")
	}
}

// IsSubscriptionGoneError classifies the "channel-side billing relationship
// is already over" errors via the SDK's typed sentinels (Type+Code match).
func TestIsSubscriptionGoneError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"not_found (unknown subscription)", fmt.Errorf("paddle cancel subscription: %w", &paddleerr.Error{
			Type: paddleerr.ErrorTypeRequestError, Code: "not_found",
		}), true},
		{"is_canceled_action_invalid (double cancel)", fmt.Errorf("paddle cancel subscription: %w", &paddleerr.Error{
			Type: paddleerr.ErrorTypeRequestError, Code: "subscription_is_canceled_action_invalid",
		}), true},
		{"unwrapped sentinel matches too", paddle.ErrNotFound, true},
		{"other request error is NOT gone-class", &paddleerr.Error{
			Type: paddleerr.ErrorTypeRequestError, Code: "subscription_locked_renewal",
		}, false},
		{"api error type is NOT gone-class", &paddleerr.Error{
			Type: paddleerr.ErrorTypeAPIError, Code: "not_found",
		}, false},
		{"plain error", errors.New("paddle 503"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSubscriptionGoneError(tc.err); got != tc.want {
				t.Errorf("IsSubscriptionGoneError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
