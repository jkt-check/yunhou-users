package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// HTTPDoer is the minimal HTTP interface the real Client needs. Pulled
// out so tests can inject a stub without dragging in a full transport
// (and so cmd/server can wire *http.Client via a one-line adapter).
type HTTPDoer interface {
	Do(req *HTTPRequest) (*HTTPResponse, error)
}

// HTTPRequest / HTTPResponse are the minimal shapes the real-mode
// UnifiedOrder path needs. Kept here as opaque structs to avoid pulling
// net/http into the package's public surface.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

type HTTPResponse struct {
	StatusCode int
	Body       []byte
}

// userAgent identifies this service in the User-Agent header on
// outbound WeChat Pay requests. Package-level so tests can compare
// against a stable constant.
const userAgent = "yunhou-users/0.1"

// unifiedOrderBody is the v3 /pay/transactions/native request body.
// NATIVE needs only mch_id + amount + trade_type; the `appid` field is
// reserved for in-app / JSAPI flows and is intentionally omitted.
// Amount reuses types.Amount (not an anonymous nested struct) so the
// JSON shape stays in one place.
type unifiedOrderBody struct {
	MchID       string `json:"mch_id"`
	Description string `json:"description"`
	OutTradeNo  string `json:"out_trade_no"`
	NotifyURL   string `json:"notify_url"`
	Amount      Amount `json:"amount"`
	TradeType   string `json:"trade_type"`
}

// Client is the WeChat Pay v3 entry point. Two modes:
//
//   - MockMode=true: UnifiedOrder returns a deterministic code_url
//     derived from OutTradeNo so a BFF can render a "fake" QR. No
//     outbound HTTP call.
//
//   - MockMode=false: the real v3 client. Signs outbound requests with
//     the merchant RSA private key (via Signer), calls
//     api.mch.weixin.qq.com/v3/pay/transactions/native for NATIVE
//     payments, returns the parsed code_url.
type Client struct {
	MockMode bool

	// Real-mode fields (used when MockMode=false). Signer is the
	// single source of truth for the merchant ID: MchID() returns
	// c.Signer.MchID and UnifiedOrder's body also reads from there, so
	// the Authorization header and the JSON body cannot drift apart.
	// APIv3Key is NOT stored on Client — it lives on cfg and is used
	// by the inbound webhook verifier only.
	Signer    *Signer
	NotifyURL string
	BaseURL   string // https://api.mch.weixin.qq.com
	HTTPDoer  HTTPDoer
}

// MchID exposes the merchant ID to callers (e.g. PaymentService writes
// it into orders.provider_intent.mch_id). Required by the service-
// layer wechatClient interface. Returns "" when no Signer is wired so
// callers see a sentinel rather than a panic.
func (c *Client) MchID() string {
	if c.Signer == nil {
		return ""
	}
	return c.Signer.MchID
}

// IsMockMode is a small accessor for handlers / services that need to
// know whether to skip real-mode code paths (e.g. mock payloads are
// plaintext, no resource block; mock code_url is enough for BFF dev).
func (c *Client) IsMockMode() bool { return c.MockMode }

// UnifiedOrder mints a code_url for the given request. In mock mode the
// URL is deterministic from OutTradeNo so tests can assert exact values
// without flakiness; in real mode it POSTs to
// /v3/pay/transactions/native and parses the response.
func (c *Client) UnifiedOrder(ctx context.Context, req UnifiedOrderRequest) (*UnifiedOrderResponse, error) {
	if req.OutTradeNo == "" {
		return nil, errors.New("OutTradeNo is required")
	}
	if req.TradeType == "" {
		req.TradeType = TradeTypeNative
	}
	if c.MockMode {
		return &UnifiedOrderResponse{
			OutTradeNo: req.OutTradeNo,
			CodeURL:    fmt.Sprintf("weixin://wxpay/bizpayurl?pr=mock_%s", req.OutTradeNo),
		}, nil
	}
	_ = ctx

	// Real mode. NATIVE only needs mch_id — the `appid` field is
	// reserved for in-app / JSAPI flows and is intentionally omitted.
	// Amount reuses types.Amount (not an anonymous nested struct) so
	// the JSON shape stays in one place.
	body, err := json.Marshal(unifiedOrderBody{
		MchID:       c.Signer.MchID,
		Description: req.Description,
		OutTradeNo:  req.OutTradeNo,
		NotifyURL:   c.NotifyURL,
		Amount:      req.Amount,
		TradeType:   string(req.TradeType),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	reqPath := "/v3/pay/transactions/native"
	auth, err := c.Signer.BuildAuthHeader("POST", reqPath, body)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}

	resp, err := c.HTTPDoer.Do(&HTTPRequest{
		Method:  "POST",
		URL:     c.BaseURL + reqPath,
		Headers: map[string]string{
			"Authorization": auth,
			"Content-Type":  "application/json",
			"Accept":        "application/json",
			"User-Agent":    userAgent,
		},
		Body: body,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeChatNetwork, err)
	}

	if resp.StatusCode >= 400 {
		var errEnv struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(resp.Body, &errEnv)
		return nil, fmt.Errorf("%w: %d %s: %s", ErrWeChatUnifiedOrderRejected,
			resp.StatusCode, errEnv.Code, errEnv.Message)
	}

	var out struct {
		CodeURL string `json:"code_url"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if out.CodeURL == "" {
		return nil, fmt.Errorf("%w: empty code_url", ErrWeChatUnifiedOrderRejected)
	}
	return &UnifiedOrderResponse{OutTradeNo: req.OutTradeNo, CodeURL: out.CodeURL}, nil
}

// ErrWeChatUnifiedOrderRejected — WeChat returned a 4xx / 5xx response
// (or a 200 with no code_url). This package makes a single attempt and
// returns; the caller's retry policy decides whether to retry (e.g.
// 5xx is transient and could be retried, 4xx is terminal). The order
// row remains in 'pending' on error so the sweeper eventually flips it
// to 'expired' if the caller doesn't clean up.
var ErrWeChatUnifiedOrderRejected = errors.New("wechat unified order rejected")

// ErrWeChatNetwork — outbound HTTP failure (timeout, DNS, connection
// refused). Distinct from ErrWeChatUnifiedOrderRejected so callers can
// classify transient vs terminal failures.
var ErrWeChatNetwork = errors.New("wechat network error")