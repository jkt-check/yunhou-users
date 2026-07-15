package wechat

import (
	"context"
	"errors"
	"fmt"
)

// Client is the WeChat Pay v3 entry point. Two modes:
//
//   - MockMode=true: UnifiedOrder returns a deterministic code_url
//     derived from OutTradeNo so a BFF can render a "fake" QR. No
//     outbound HTTP call.
//
//   - MockMode=false: the real v3 client (lands with A2.c). v1 returns
//     an explicit ErrUnimplemented so production-like callers get a
//     loud failure instead of a silently missing order.
type Client struct {
	MockMode bool
	// Real-mode fields (used when MockMode=false). Wired by cmd/server
	// from cfg in A2.c. Left as zero-value stubs in v1 because the
	// real signing / certificate plumbing is A2.c's scope.
	MchID       string
	APIv3Key    []byte
	CertPath    string
	KeyPath     string
	NotifyURL   string
	BaseURL     string // https://api.mch.weixin.qq.com
	HTTPDoer    HTTPDoer
}

// HTTPDoer is the minimal HTTP interface the real Client needs. Pulled
// out so tests can inject a stub without dragging in a full transport
// (and so cmd/server can wire *http.Client via a one-line adapter).
type HTTPDoer interface {
	Do(req *HTTPRequest) (*HTTPResponse, error)
}

// HTTPRequest / HTTPResponse are the minimal shapes the real-mode
// UnifiedOrder path will need. Kept here as opaque structs to avoid
// pulling net/http into the package's public surface — the A2.c
// landing will replace them with concrete types or remove them if a
// direct net/http dependency is acceptable.
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

// ErrUnimplemented signals the real WeChat Pay v3 client hasn't landed
// yet (scheduled for A2.c — production credentials). Production callers
// that hit this path get a loud 500 with a clear next-action message
// instead of a silently broken flow.
var ErrUnimplemented = errors.New("wechat pay v3 real client not yet implemented (A2.c)")

// UnifiedOrder mints a code_url for the given request. In mock mode
// the URL is deterministic from OutTradeNo so tests can assert exact
// values without flakiness; in real mode it returns ErrUnimplemented
// until A2.c lands.
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
	return nil, ErrUnimplemented
}

// IsMockMode is a small accessor for handlers that need to know whether
// to skip AES-GCM resource decryption (mock payloads are plaintext
// JSON, no resource block).
func (c *Client) IsMockMode() bool { return c.MockMode }