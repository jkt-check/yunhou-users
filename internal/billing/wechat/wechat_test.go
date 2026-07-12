package wechat

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClient_UnifiedOrder_MockMode_ReturnsCodeURL(t *testing.T) {
	c := &Client{MockMode: true}
	req := UnifiedOrderRequest{
		OutTradeNo:  "ord-123",
		Description: "monthly plan",
		Amount:      Amount{Total: 2990, Currency: "CNY"},
		TradeType:   TradeTypeNative,
	}
	got, err := c.UnifiedOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("UnifiedOrder (mock): %v", err)
	}
	if got.OutTradeNo != "ord-123" {
		t.Errorf("OutTradeNo = %q, want ord-123", got.OutTradeNo)
	}
	if !strings.Contains(got.CodeURL, "mock_ord-123") {
		t.Errorf("code_url = %q, want it to embed the order id", got.CodeURL)
	}
	if !strings.HasPrefix(got.CodeURL, "weixin://wxpay/bizpayurl?pr=") {
		t.Errorf("code_url = %q, want weixin://wxpay/bizpayurl?pr=... prefix", got.CodeURL)
	}
}

func TestClient_UnifiedOrder_MockMode_DefaultTradeTypeIsNative(t *testing.T) {
	c := &Client{MockMode: true}
	got, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo: "ord-x",
	})
	if err != nil {
		t.Fatalf("UnifiedOrder: %v", err)
	}
	if got.CodeURL == "" {
		t.Errorf("expected non-empty code_url when TradeType is empty (defaults to NATIVE)")
	}
}

func TestClient_UnifiedOrder_RealMode_ReturnsUnimplemented(t *testing.T) {
	c := &Client{MockMode: false}
	_, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{OutTradeNo: "ord-y"})
	if !errors.Is(err, ErrUnimplemented) {
		t.Errorf("err = %v, want ErrUnimplemented", err)
	}
}

func TestClient_UnifiedOrder_EmptyOutTradeNo(t *testing.T) {
	c := &Client{MockMode: true}
	_, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{})
	if err == nil || !strings.Contains(err.Error(), "OutTradeNo") {
		t.Errorf("err = %v, want one mentioning OutTradeNo", err)
	}
}

func TestClient_IsMockMode(t *testing.T) {
	if (&Client{MockMode: true}).IsMockMode() != true {
		t.Error("IsMockMode = false, want true")
	}
	if (&Client{MockMode: false}).IsMockMode() != false {
		t.Error("IsMockMode = true, want false")
	}
}