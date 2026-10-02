package paddle

import (
	"context"
	"strings"
	"testing"
	"time"
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
