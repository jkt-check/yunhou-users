// Package wechat provides WeChat Pay v3 client primitives — specifically
// NATIVE / H5 / JSAPI UnifiedOrder calls and the resource-block shape
// WeChat Pay v3 webhooks encrypt.
//
// The v1 surface used by the rest of the codebase is just two things:
//   - Client.UnifiedOrder  → mints a code_url (NATIVE) the BFF QR-codes
//   - Resource             → the AES-GCM-decrypted plaintext the
//     webhook handler turns into a Payment row
//
// The HTTP / signing / certificate plumbing for the real production
// call is intentionally left as a small surface (Client.UnifiedOrder's
// non-mock branch is a stub that returns errors). cn-staging / prod
// integration with WeChat Pay v3 arrives with A2.c (production
// credentials) — the schema, error types, and mock branch are the
// pieces this PR locks down.
package wechat

// TradeType selects the WeChat Pay v3 product. NATIVE produces a
// code_url the BFF turns into a QR; H5 returns an h5_url; JSAPI needs
// the openid from a prior OAuth flow and returns a package for the
// in-WeChat browser. v1 only ships NATIVE; the other two are reserved
// future values in the schema so callers don't have to special-case
// the absence of an h5_url field later.
type TradeType string

const (
	TradeTypeNative TradeType = "NATIVE"
	TradeTypeH5     TradeType = "H5"
	TradeTypeJSAPI  TradeType = "JSAPI"
)

// UnifiedOrderRequest is the v3 unified-order input. We trim the
// real-API fields to only what yunhou-users needs; expand in A2.c when
// the production client lands.
type UnifiedOrderRequest struct {
	OutTradeNo  string    // yunhou order UUID; echoed back in webhook
	Description string    // shown on the user's WeChat "merchant" line
	Amount      Amount    // CNY fen (integer)
	TradeType   TradeType // NATIVE in v1
}

// Amount mirrors WeChat Pay v3's fen-based minor unit. Total is an
// int64 so JSON round-trip never introduces float-rounding artefacts
// (a 0.01 CNY drift would silently mis-activate the wrong subscription).
type Amount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"` // "CNY" in v1
}

// UnifiedOrderResponse is the v3 unified-order success body. We keep
// only the field each TradeType consumes; WeChat returns the rest but
// yunhou-users doesn't need them.
type UnifiedOrderResponse struct {
	OutTradeNo string `json:"out_trade_no"` // echoed from request
	// NATIVE: the QR-target string. H5: mweb_url. JSAPI: nothing (the
	// SDK needs prepay_id to build the package, omitted from v1).
	CodeURL string `json:"code_url,omitempty"`
}

// Resource is the AES-GCM-decrypted plaintext of a WeChat Pay v3
// webhook payload. The handler reads TransactionID + OutTradeNo to
// match against an existing order row.
type Resource struct {
	TransactionID string `json:"transaction_id"` // WeChat's txn id
	OutTradeNo    string `json:"out_trade_no"`   // yunhou order UUID
	TradeState    string `json:"trade_state"`    // "SUCCESS" for our purposes
	SuccessTime   string `json:"success_time"`   // RFC3339 string WeChat sends
	Amount        Amount `json:"amount"`         // paid amount in fen
}
