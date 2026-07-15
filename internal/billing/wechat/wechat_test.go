package wechat

import (
	"context"
	"errors"
	"fmt"
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

// --- Real-mode UnifiedOrder tests (Task 5) ---

type stubDoer struct {
	resp *HTTPResponse
	err  error
	got  *HTTPRequest // captured for assertion
}

func (s *stubDoer) Do(req *HTTPRequest) (*HTTPResponse, error) {
	s.got = req
	return s.resp, s.err
}

func newRealClient(t *testing.T, doer HTTPDoer) *Client {
	t.Helper()
	key, err := LoadPrivateKey("testdata/sign_test_key.pem")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	serial, err := LoadCertSerial("testdata/sign_test_cert.pem")
	if err != nil {
		t.Fatalf("load cert: %v", err)
	}
	return &Client{
		MockMode:  false,
		Signer:    &Signer{MchID: "1900000109", SerialNo: serial, PrivateKey: key},
		NotifyURL: "https://example.com/webhooks/payment/wechat_pay",
		BaseURL:   "https://api.mch.weixin.qq.com",
		HTTPDoer:  doer,
	}
}

func TestUnifiedOrder_Real_200(t *testing.T) {
	stub := &stubDoer{resp: &HTTPResponse{StatusCode: 200, Body: []byte(`{"code_url":"weixin://wxpay/bizpayurl?pr=ABC123"}`)}}
	c := newRealClient(t, stub)
	resp, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo:  "order-1",
		Description: "plan-monthly",
		Amount:      Amount{Total: 1234, Currency: "CNY"},
		TradeType:   TradeTypeNative,
	})
	if err != nil {
		t.Fatalf("UnifiedOrder: %v", err)
	}
	if resp.CodeURL != "weixin://wxpay/bizpayurl?pr=ABC123" {
		t.Fatalf("code_url = %q", resp.CodeURL)
	}
	if stub.got == nil || !strings.HasPrefix(stub.got.Headers["Authorization"], "WECHATPAY2-SHA256-RSA2048 ") {
		t.Fatalf("Authorization header missing or wrong: %v", stub.got)
	}
	if !strings.Contains(string(stub.got.Body), `"mch_id":"1900000109"`) {
		t.Fatalf("body missing mch_id: %s", stub.got.Body)
	}
	if !strings.Contains(string(stub.got.Body), `"out_trade_no":"order-1"`) {
		t.Fatalf("body missing out_trade_no: %s", stub.got.Body)
	}
	if strings.Contains(string(stub.got.Body), `"appid"`) {
		t.Fatalf("body unexpectedly contains appid: %s", stub.got.Body)
	}
}

func TestUnifiedOrder_Real_4xx(t *testing.T) {
	stub := &stubDoer{resp: &HTTPResponse{StatusCode: 400, Body: []byte(`{"code":"INVALID_REQUEST","message":"bad amount"}`)}}
	c := newRealClient(t, stub)
	_, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo: "order-1", Description: "x",
		Amount:    Amount{Total: 100, Currency: "CNY"},
		TradeType: TradeTypeNative,
	})
	if !errors.Is(err, ErrWeChatUnifiedOrderRejected) {
		t.Fatalf("err = %v, want ErrWeChatUnifiedOrderRejected", err)
	}
}

func TestUnifiedOrder_Real_5xx(t *testing.T) {
	stub := &stubDoer{resp: &HTTPResponse{StatusCode: 500, Body: []byte(`{}`)}}
	c := newRealClient(t, stub)
	_, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo: "order-1", Description: "x",
		Amount:    Amount{Total: 100, Currency: "CNY"},
		TradeType: TradeTypeNative,
	})
	if !errors.Is(err, ErrWeChatUnifiedOrderRejected) {
		t.Fatalf("err = %v, want ErrWeChatUnifiedOrderRejected", err)
	}
}

func TestUnifiedOrder_Real_NetworkErr(t *testing.T) {
	stub := &stubDoer{err: fmt.Errorf("net down")}
	c := newRealClient(t, stub)
	_, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo: "order-1", Description: "x",
		Amount:    Amount{Total: 100, Currency: "CNY"},
		TradeType: TradeTypeNative,
	})
	if !errors.Is(err, ErrWeChatNetwork) {
		t.Fatalf("err = %v, want ErrWeChatNetwork", err)
	}
}

func TestUnifiedOrder_Real_EmptyCodeURL(t *testing.T) {
	stub := &stubDoer{resp: &HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}}
	c := newRealClient(t, stub)
	_, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo: "order-1", Description: "x",
		Amount:    Amount{Total: 100, Currency: "CNY"},
		TradeType: TradeTypeNative,
	})
	if !errors.Is(err, ErrWeChatUnifiedOrderRejected) {
		t.Fatalf("err = %v, want ErrWeChatUnifiedOrderRejected", err)
	}
}

func TestUnifiedOrder_Mock_Unchanged(t *testing.T) {
	c := &Client{MockMode: true}
	resp, err := c.UnifiedOrder(context.Background(), UnifiedOrderRequest{
		OutTradeNo: "order-1", Description: "x",
		Amount:    Amount{Total: 100, Currency: "CNY"},
		TradeType: TradeTypeNative,
	})
	if err != nil {
		t.Fatalf("UnifiedOrder mock: %v", err)
	}
	if !strings.Contains(resp.CodeURL, "pr=mock_order-1") {
		t.Fatalf("mock code_url = %q", resp.CodeURL)
	}
}