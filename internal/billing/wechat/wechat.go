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
// The ctx parameter lets callers propagate cancellation / deadlines into
// the outbound request — production wires *http.Client (via
// http.NewRequestWithContext); tests can capture the ctx to verify
// cancellation behaviour without spinning up a real server.
type HTTPDoer interface {
	Do(ctx context.Context, req *HTTPRequest) (*HTTPResponse, error)
}

// ClientIface is the public interface satisfied by *Client. Exposed so
// callers (notably cmd/server/main.go) can hold a nil interface value
// when WeChat Pay is not configured, avoiding the typed-nil pitfall
// where a *Client assigned to an interface field with `= nil` is not
// == nil at the language level — `s.wechat.IsMockMode()` on a nil
// receiver would panic. By declaring the local variable as ClientIface,
// an untyped nil stays == nil and `s.wechat != nil` guards behave
// correctly.
type ClientIface interface {
	IsMockMode() bool
	MchID() string
	AppID() string
	UnifiedOrder(ctx context.Context, req UnifiedOrderRequest) (*UnifiedOrderResponse, error)
	QueryOrder(ctx context.Context, outTradeNo string) (*OrderQueryResult, error)
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
//
// WeChat Pay v3 NATIVE rejects unknown fields with HTTP 400 +
// code=PARAM_ERROR ("请求中含有未在API文档中定义的参数") — verified live on
// 2026-07-23 against mch=1115525931 with `trade_type` in the body. v3
// infers trade_type from the URL path `/v3/pay/transactions/native`
// (and the analogous H5/JSAPI/APP paths), so the field does NOT belong
// in this body — including it is what triggered the
// "wechat pay rejected the order (4xx)" user-facing error. The
// UnifiedOrderRequest keeps a TradeType field for callers and future
// product endpoints (H5, JSAPI) that DO need it, but only this struct
// goes on the wire for /transactions/native — and it has no trade_type.
//
// NATIVE requires BOTH `appid` and `mchid` in v3 (the field name is
// `mchid`, NOT `mch_id` — JSON-tag drift here caused the prior
// mch_id = "" mismatch wechat reject). Amount reuses types.Amount
// (not an anonymous nested struct) so the JSON shape stays in one place.
type unifiedOrderBody struct {
	AppID       string `json:"appid"`
	MchID       string `json:"mchid"`
	Description string `json:"description"`
	OutTradeNo  string `json:"out_trade_no"`
	NotifyURL   string `json:"notify_url"`
	Amount      Amount `json:"amount"`
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
	Signer     *Signer
	AppIDValue string // WeChat Open Platform 网站应用 appid; required in v3 NATIVE body. Stored as AppIDValue so the AppID() getter (used by the service layer to echo into provider_intent.appid) doesn't collide with the field name.
	NotifyURL  string
	BaseURL    string // https://api.mch.weixin.qq.com
	HTTPDoer   HTTPDoer
}

// MchID exposes the merchant ID to callers (e.g. PaymentService writes
// it into orders.provider_intent.mchid). Required by the service-
// layer wechatClient interface. Returns "" when no Signer is wired so
// callers see a sentinel rather than a panic.
func (c *Client) MchID() string {
	if c.Signer == nil {
		return ""
	}
	return c.Signer.MchID
}

// AppID exposes the WeChat Open Platform appid to callers (e.g.
// PaymentService writes it into orders.provider_intent.appid).
// Required by the service-layer wechatClient interface. Returns ""
// when no AppID is wired so callers see a sentinel rather than a panic.
func (c *Client) AppID() string {
	return c.AppIDValue
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

	// Real mode. NATIVE requires BOTH `appid` and `mchid` (v3 protocol).
	// Body is built field-by-field from a fresh struct (rather than
	// reusing a single object literal) so future field additions stay
	// reviewable.
	//
	// Pre-flight validation: refuse to send a request whose appid or
	// mchid is empty. Without this guard a misconfigured deployment
	// (MockMode=false but AppIDValue/Signer.MchID not set) sends
	// `"appid":"" / "mchid":""` to WeChat and only surfaces the
	// misconfig at first checkout with a confusing `400 PARAM_ERROR`.
	// Failing here is a clearer operator signal.
	if c.AppIDValue == "" {
		return nil, fmt.Errorf("%w: AppID not configured", ErrWechatMisconfigured)
	}
	if c.Signer == nil || c.Signer.MchID == "" {
		return nil, fmt.Errorf("%w: Signer/MchID not configured", ErrWechatMisconfigured)
	}
	var bodyBytes unifiedOrderBody
	bodyBytes.AppID = c.AppIDValue
	bodyBytes.MchID = c.Signer.MchID
	bodyBytes.Description = req.Description
	bodyBytes.OutTradeNo = req.OutTradeNo
	bodyBytes.NotifyURL = c.NotifyURL
	bodyBytes.Amount.Total = req.Amount.Total
	bodyBytes.Amount.Currency = req.Amount.Currency
	// trade_type intentionally NOT marshalled — see unifiedOrderBody's
	// doc comment. TradeType on UnifiedOrderRequest is preserved for
	// future H5/JSAPI products where the URL differs; for the v3 NATIVE
	// path the URL alone selects the product.
	body, err := json.Marshal(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	reqPath := "/v3/pay/transactions/native"
	auth, err := c.Signer.BuildAuthHeader("POST", reqPath, body)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}

	resp, err := c.HTTPDoer.Do(ctx, &HTTPRequest{
		Method: "POST",
		URL:    c.BaseURL + reqPath,
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

	if resp.StatusCode >= 500 {
		var errEnv struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(resp.Body, &errEnv)
		return nil, fmt.Errorf("%w: %d %s: %s", ErrWeChatUpstream,
			resp.StatusCode, errEnv.Code, errEnv.Message)
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

// OrderQueryResult is the parsed shape of
// GET /v3/pay/transactions/out-trade-no/{out_trade_no}. Only the fields
// yunhou-users needs for reconciliation are decoded; WeChat returns
// more (payer, promotion_detail, ...).
type OrderQueryResult struct {
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	TradeState    string `json:"trade_state"` // SUCCESS / NOTPAY / CLOSED / REFUND / ...
	SuccessTime   string `json:"success_time"`
	Amount        struct {
		Total    int64  `json:"total"` // fen
		Currency string `json:"currency"`
	} `json:"amount"`
}

// QueryOrder fetches the current state of an order from WeChat. This is
// the ACTIVE reconciliation path: the webhook is the primary signal,
// but if a webhook is lost (or was rejected before the platform-cert
// verifier fix of 2026-07-23), the FE's order-status poll drives this
// query and the service layer flips the order paid from the answer.
//
// Mock mode returns a deterministic NOTPAY so mock-mode behaviour is
// unchanged (mock payments reconcile via the mock webhook).
func (c *Client) QueryOrder(ctx context.Context, outTradeNo string) (*OrderQueryResult, error) {
	if outTradeNo == "" {
		return nil, errors.New("outTradeNo is required")
	}
	if c.MockMode {
		return &OrderQueryResult{OutTradeNo: outTradeNo, TradeState: "NOTPAY"}, nil
	}

	// Same pre-flight as UnifiedOrder: refuse to send a request
	// without a configured Signer/MchID rather than panic on
	// `c.Signer.MchID` below. ErrWechatMisconfigured is a typed sentinel
	// the handler maps to a 4xx (misconfig is operator-fixable, not
	// transient) rather than the 502 the panic-during-NOTPAY would
	// otherwise produce.
	if c.Signer == nil || c.Signer.MchID == "" {
		return nil, fmt.Errorf("%w: Signer/MchID not configured", ErrWechatMisconfigured)
	}

	reqPath := "/v3/pay/transactions/out-trade-no/" + outTradeNo + "?mchid=" + c.Signer.MchID
	auth, err := c.Signer.BuildAuthHeader("GET", reqPath, nil)
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}

	resp, err := c.HTTPDoer.Do(ctx, &HTTPRequest{
		Method: "GET",
		URL:    c.BaseURL + reqPath,
		Headers: map[string]string{
			"Authorization": auth,
			"Accept":        "application/json",
			"User-Agent":    userAgent,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWeChatNetwork, err)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: %d", ErrWeChatUpstream, resp.StatusCode)
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

	var out OrderQueryResult
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// ErrWeChatUnifiedOrderRejected — WeChat returned a 4xx response (or a
// 200 with no code_url). This is a terminal rejection: retrying the
// same payload will fail again. Caller should surface a user-visible
// error and not auto-retry.
var ErrWeChatUnifiedOrderRejected = errors.New("wechat unified order rejected (4xx)")

// ErrWeChatUpstream — WeChat returned a 5xx response (transient upstream
// failure). The order may or may not have been processed out-of-band;
// callers MAY retry with the same OutTradeNo after a backoff. This
// package does not retry itself (see spec §"5xx retry" — deferred to v2).
var ErrWeChatUpstream = errors.New("wechat upstream 5xx")

// ErrWeChatNetwork — outbound HTTP failure (timeout, DNS, connection
// refused, ctx cancellation). Distinct from ErrWeChatUnifiedOrderRejected
// and ErrWeChatUpstream so callers can classify transient vs terminal
// failures.
var ErrWeChatNetwork = errors.New("wechat network error")

// ErrWechatMisconfigured — a real-mode Client was used without AppID,
// Signer/MchID, or both. Surfaced from the pre-flight guards in
// UnifiedOrder / QueryOrder so a misconfigured deployment (e.g.
// MockMode flipped to false in production but the upstream creds
// weren't wired) fails at request time with a clear operator-visible
// error rather than sending `appid=""` upstream and only failing
// later at the upstream's 400 PARAM_ERROR.
var ErrWechatMisconfigured = errors.New("wechat client misconfigured")
