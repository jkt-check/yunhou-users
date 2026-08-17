package handler

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// WebhookHandler receives signed channel webhooks at POST /webhooks/payment/:channel.
// The signature middleware has already verified the signature by the time
// the handler runs; here we just parse channel-specific payload structure
// and dispatch to PaymentService.OnWebhook.
//
// Channel parsing rules:
//   - Stripe  : application/json, `data.object.{id, metadata.order_id, amount, currency}`
//     amount is in cents → divide by 100 to major units (design doc §"Amount unit convention")
//   - WeChat  : application/json, AES-256-GCM-decrypt resource.ciphertext;
//     metadata.order_id = out_trade_no (set at order creation). When
//     mockWechatPay is true the body is plaintext JSON (no resource
//     block, no AES) so dev / e2e suites can drive the flow without a
//     registered merchant.
//   - Alipay  : application/x-www-form-urlencoded, out_trade_no IS the order_id,
//     total_amount is in major units (no conversion)
type WebhookHandler struct {
	svc           service.PaymentServiceInterface
	wechatKey     []byte // WECHAT_PAY_API_V3_KEY — fallback if verifier can't decrypt
	verifier      middleware.ChannelSignatureVerifier
	mockWechatPay bool
}

func NewWebhookHandler(svc service.PaymentServiceInterface, wechatAPIv3Key []byte, verifier middleware.ChannelSignatureVerifier, mockWechatPay bool) *WebhookHandler {
	return &WebhookHandler{svc: svc, wechatKey: wechatAPIv3Key, verifier: verifier, mockWechatPay: mockWechatPay}
}

// Handle is the single entrypoint for /webhooks/payment/:channel. The
// signature middleware has already validated the request; we read the
// (restored) body, dispatch by channel to the parser, then call service.
func (h *WebhookHandler) Handle(c *gin.Context) {
	channel := c.Param("channel")
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("webhook: read body: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "read body failed"})
		return
	}

	event, err := h.parseEvent(channel, raw)
	if err != nil {
		log.Printf("webhook: parse event (%s): %v", channel, err)
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "could not parse event"})
		return
	}

	// Wrap the raw payload for storage. JSONB columns require valid JSON;
	// Alipay's form-encoded body would fail a direct cast. We wrap it as a
	// JSON string so the channel-specific shape stays queryable later.
	event.RawPayload = wrapRawPayload(channel, raw)

	result, err := h.svc.OnWebhook(c.Request.Context(), *event)
	if err != nil {
		// Internal errors: 500 so the channel retries per its schedule.
		// The service layer is responsible for already-known non-actionable
		// cases (e.g. webhook_for_unknown_order) being written to audit_log
		// without surfacing as an error.
		log.Printf("webhook: handler error (%s, %s): %v", channel, event.EventType, err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "handler error"})
		return
	}

	// Always 200 on success — duplicates, uninteresting event types, and
	// real domain actions all converge to the same shape. The channel stops
	// retrying on 2xx (per its own contract).
	// Per CLAUDE.md envelope, `domain_action` and `duplicate` live INSIDE
	// `data` (not as top-level keys). Channels parse this body, not the
	// envelope itself, so they don't notice either way; the in-shape keys
	// keep consumer apps that parse `data.*` working uniformly.
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"received":      true,
			"domain_action": result.DomainAction,
			"duplicate":     result.DuplicateEvent,
		},
	})
}

// parseEvent dispatches by channel. Each branch returns a fully populated
// *service.WebhookEvent ready for OnWebhook.
func (h *WebhookHandler) parseEvent(channel string, raw []byte) (*service.WebhookEvent, error) {
	switch channel {
	case "stripe":
		return h.parseStripe(raw)
	case "wechat_pay":
		return h.parseWeChat(raw)
	case "alipay":
		return h.parseAlipay(raw)
	case "paypal":
		return h.parsePaypal(raw)
	default:
		return nil, fmt.Errorf("unsupported channel: %s", channel)
	}
}

// parseStripe extracts the fields OnWebhook needs from a Stripe event.
// RawPayload is left nil — the handler wraps the original raw bytes via
// wrapRawPayload after this returns.
//
//	{
//	  "id":   "evt_xxx",
//	  "type": "payment_intent.succeeded",
//	  "data": { "object": {
//	    "id": "pi_xxx",
//	    "amount": 2990,                 // cents
//	    "currency": "cny",
//	    "metadata": { "order_id": "<uuid>" }
//	  }}
//	}
func (h *WebhookHandler) parseStripe(raw []byte) (*service.WebhookEvent, error) {
	var evt struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID       string `json:"id"`
				Amount   int64  `json:"amount"`
				Currency string `json:"currency"`
				Metadata struct {
					OrderID    string `json:"order_id"`
					SubExpires string `json:"sub_expires_at"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("stripe body: %w", err)
	}

	pi := evt.Data.Object
	we := &service.WebhookEvent{
		Channel:       "stripe",
		EventID:       evt.ID,
		EventType:     evt.Type,
		TransactionID: pi.ID,
		OrderID:       pi.Metadata.OrderID,
		Amount:        float64(pi.Amount) / 100, // cents → major units
		Currency:      strings.ToUpper(pi.Currency),
	}
	if pi.Metadata.SubExpires != "" {
		// RFC3339 — frontend product decision sets this from plan.interval_days
		// + business rules (rollover, grace, trial). nil/absent → never expires.
		if t, err := time.Parse(time.RFC3339, pi.Metadata.SubExpires); err == nil {
			we.SubExpiresAt = &t
		}
	}
	return we, nil
}

// parseWeChat decrypts the resource block and reads transaction_id / out_trade_no
// from the decrypted payload. WeChat v3 encloses business data inside `resource`
// with AES-256-GCM ciphertext; without decrypting, the business fields are
// unreadable.
//
// In mock mode (mockWechatPay=true) the body is plaintext JSON with the
// same field shape as the decrypted resource — no `resource` wrapper, no
// ciphertext. e2e suites drive the order-paid → subscription flow
// without registering a merchant.
func (h *WebhookHandler) parseWeChat(raw []byte) (*service.WebhookEvent, error) {
	if h.mockWechatPay {
		return h.parseWeChatMock(raw)
	}

	var evt struct {
		ID         string `json:"id"`
		CreateTime string `json:"create_time"`
		EventType  string `json:"event_type"`
		Resource   struct {
			Ciphertext     string `json:"ciphertext"`
			AssociatedData string `json:"associated_data"`
			Nonce          string `json:"nonce"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("wechat body: %w", err)
	}

	plaintext, err := h.decryptWeChatResource(evt.Resource.Ciphertext, evt.Resource.Nonce, evt.Resource.AssociatedData)
	if err != nil {
		return nil, fmt.Errorf("wechat decrypt: %w", err)
	}

	// decrypted resource shape:
	//   TRANSACTION.SUCCESS: { "transaction_id": "...", "amount": { "total": 100, ... }, "out_trade_no": "...", "sub_expires_at": "..." }
	//   TRANSACTION.REFUND:  { "transaction_id": "...", "amount": { "refund": 100, ... }, "out_trade_no": "..." }
	var resource struct {
		TransactionID string `json:"transaction_id"`
		OutTradeNo    string `json:"out_trade_no"`
		SubExpires    string `json:"sub_expires_at"`
		Amount        struct {
			Total  int64 `json:"total"`
			Refund int64 `json:"refund"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(plaintext, &resource); err != nil {
		return nil, fmt.Errorf("wechat resource json: %w", err)
	}

	we := &service.WebhookEvent{
		Channel:       "wechat_pay",
		EventID:       evt.ID,
		EventType:     evt.EventType,
		TransactionID: resource.TransactionID,
		OrderID:       resource.OutTradeNo,
		Amount:        float64(resource.Amount.Total) / 100, // fen → major units
		Currency:      "CNY",                                // WeChat is always CNY in v1
	}
	if resource.SubExpires != "" {
		if t, err := time.Parse(time.RFC3339, resource.SubExpires); err == nil {
			we.SubExpiresAt = &t
		}
	}
	if resource.Amount.Refund > 0 {
		we.RefundAmount = float64(resource.Amount.Refund) / 100
	}
	return we, nil
}

// parseWeChatMock accepts a plaintext JSON body that mirrors the
// decrypted-resource shape directly (no resource wrapper, no AES). The
// signature middleware's MockMode short-circuit has already accepted
// the headers, so we just decode and pass through to the same
// downstream WebhookEvent construction.
func (h *WebhookHandler) parseWeChatMock(raw []byte) (*service.WebhookEvent, error) {
	var evt struct {
		ID        string `json:"id"`
		EventType string `json:"event_type"`
		Resource  struct {
			TransactionID string `json:"transaction_id"`
			OutTradeNo    string `json:"out_trade_no"`
			SubExpires    string `json:"sub_expires_at"`
			Amount        struct {
				Total  int64 `json:"total"`
				Refund int64 `json:"refund"`
			} `json:"amount"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("wechat mock body: %w", err)
	}
	if evt.EventType == "" {
		return nil, fmt.Errorf("wechat mock missing event_type")
	}
	if evt.Resource.OutTradeNo == "" {
		return nil, fmt.Errorf("wechat mock missing resource.out_trade_no")
	}
	we := &service.WebhookEvent{
		Channel:       "wechat_pay",
		EventID:       evt.ID,
		EventType:     evt.EventType,
		TransactionID: evt.Resource.TransactionID,
		OrderID:       evt.Resource.OutTradeNo,
		Amount:        float64(evt.Resource.Amount.Total) / 100,
		Currency:      "CNY",
	}
	if evt.Resource.SubExpires != "" {
		if t, err := time.Parse(time.RFC3339, evt.Resource.SubExpires); err == nil {
			we.SubExpiresAt = &t
		}
	}
	if evt.Resource.Amount.Refund > 0 {
		we.RefundAmount = float64(evt.Resource.Amount.Refund) / 100
	}
	return we, nil
}

// decryptWeChatResource AES-256-GCM decrypts the resource.ciphertext.
// Delegates to the WeChatPayV3Verifier when available so the crypto
// implementation lives in exactly one place (middleware/webhook_sig.go).
// Falls back to the local implementation if the verifier isn't a
// WeChatPayV3Verifier (test stubs). The typed-nil check guards against
// a `var v *WeChatPayV3Verifier; NewWebhookHandler(..., v)` misuse — a
// typed nil passes the type assertion as `ok=true` but would panic on
// the first method call.
func (h *WebhookHandler) decryptWeChatResource(ciphertextB64, nonce, associatedData string) ([]byte, error) {
	if v, ok := h.verifier.(*middleware.WeChatPayV3Verifier); ok && v != nil {
		return v.DecryptResource(ciphertextB64, nonce, associatedData)
	}
	return localWeChatDecrypt(string(h.wechatKey), ciphertextB64, nonce, associatedData)
}

// localWeChatDecrypt is the in-package fallback used only when the
// WeChatPayV3Verifier isn't wired (handler-level tests). The production
// path goes through the verifier's DecryptResource method.
func localWeChatDecrypt(key, ciphertextB64, nonce, associatedData string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length: got %d, want %d", len(nonce), gcm.NonceSize())
	}
	return gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
}

// parseAlipay reads the form-encoded body. Alipay sends:
//
//	out_trade_no (order_id), trade_no (transaction_id), total_amount,
//	refund_amount (if refund event), notify_id (event_id), notify_type (event_type).
func (h *WebhookHandler) parseAlipay(raw []byte) (*service.WebhookEvent, error) {
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("alipay form: %w", err)
	}

	outTradeNo := values.Get("out_trade_no")
	tradeNo := values.Get("trade_no")
	totalAmount := values.Get("total_amount")
	refundAmount := values.Get("refund_amount")
	subExpires := values.Get("sub_expires_at")
	notifyID := values.Get("notify_id")
	notifyType := values.Get("notify_type")

	// Reject malformed webhooks — notify_id and notify_type are the dedupe
	// key + dispatch key. Empty values would collapse every malformed
	// notification into the same row and silently no-op the dispatch.
	if notifyID == "" {
		return nil, fmt.Errorf("alipay missing notify_id")
	}
	if notifyType == "" {
		return nil, fmt.Errorf("alipay missing notify_type")
	}

	event := &service.WebhookEvent{
		Channel:       "alipay",
		EventID:       notifyID,
		EventType:     notifyType,
		TransactionID: tradeNo,
		OrderID:       outTradeNo,
		Currency:      "CNY", // v1 assumption
	}
	if totalAmount != "" {
		v, err := strconv.ParseFloat(totalAmount, 64)
		if err != nil {
			return nil, fmt.Errorf("alipay total_amount: %w", err)
		}
		event.Amount = v
	}
	if refundAmount != "" {
		v, err := strconv.ParseFloat(refundAmount, 64)
		if err != nil {
			return nil, fmt.Errorf("alipay refund_amount: %w", err)
		}
		event.RefundAmount = v
	}
	if subExpires != "" {
		if t, err := time.Parse(time.RFC3339, subExpires); err == nil {
			event.SubExpiresAt = &t
		}
	}

	// Alipay doesn't echo its own refund ID; the service generates an
	// internal external_refund_id derived from notify_id for refund events.
	if isAlipayRefundEvent(notifyType) {
		event.ExternalRefundID = "alipay-" + notifyID
	}
	return event, nil
}

func isAlipayRefundEvent(notifyType string) bool {
	// Alipay event types: trade_status_sync (paid), trade_closed (closed/refunded).
	return notifyType == "trade_closed"
}

// parsePaypal — kept below

// parsePaypal extracts fields from a PayPal webhook. PayPal's webhook event
// shape is:
//
//	{
//	  "id":         "WH-...",
//	  "event_type": "PAYMENT.CAPTURE.COMPLETED" | ...,
//	  "resource":   { "id": "...", "custom_id": "<our order uuid>",
//	                  "amount": { "value": "29.90", "currency_code": "USD" },
//	                  "billing_agreement_id": "I-..." (subscription events),
//	                  "billing_info": { "next_billing_time": "..." } (renewal only) }
//	}
//
// Order binding uses resource.custom_id, which the frontend sets to our
// order UUID at PayPal Order / Subscription creation time. Subscription and
// renewal events also carry resource.billing_agreement_id — we map that to
// WebhookEvent.ExternalSubscriptionID for the renewal branch.
func (h *WebhookHandler) parsePaypal(raw []byte) (*service.WebhookEvent, error) {
	var evt struct {
		ID        string `json:"id"`
		EventType string `json:"event_type"`
		Resource  struct {
			ID                 string `json:"id"`
			CustomID           string `json:"custom_id"`
			BillingAgreementID string `json:"billing_agreement_id"`
			Amount             struct {
				Value        string `json:"value"`
				CurrencyCode string `json:"currency_code"`
			} `json:"amount"`
			BillingInfo *struct {
				NextBillingTime string `json:"next_billing_time"`
			} `json:"billing_info"`
			// PayPal refund events point at the parent capture via the
			// "up" relation in links[]. We need that ID (not resource.id,
			// which is the refund id) to find the original payment row.
			Links []struct {
				Href string `json:"href"`
				Rel  string `json:"rel"`
			} `json:"links"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("paypal body: %w", err)
	}
	if evt.ID == "" || evt.EventType == "" {
		return nil, fmt.Errorf("paypal missing id or event_type")
	}
	if evt.Resource.ID == "" {
		// resource.id is the channel-side transaction/subscription ID. An
		// empty value would store ExternalSubscriptionID="" or TransactionID="",
		// and the payments.external_txn_id UNIQUE constraint would then dedup
		// every malformed event onto one row.
		return nil, fmt.Errorf("paypal missing resource.id")
	}
	// resource.custom_id is required only for events that need to map back
	// to a Yunhou order row. Subscription renewals (PAYMENT.SALE.*) don't
	// carry custom_id — they identify the subscription via
	// resource.billing_agreement_id. Lifecycle events
	// (BILLING.SUBSCRIPTION.*) DO echo the custom_id the BFF set at
	// subscription creation (verified against live sandbox events
	// 2026-08-17), which is how ACTIVATED finds the order row. Requiring
	// custom_id globally would silently drop every renewal webhook,
	// leaving paid customers without an extended subscription.
	// PAYMENT.CAPTURE.COMPLETED for one-time purchases still requires
	// custom_id (it's the only link back to the order).
	needsCustomID := evt.EventType == "PAYMENT.CAPTURE.COMPLETED" ||
		evt.EventType == "PAYMENT.CAPTURE.REFUNDED"
	if needsCustomID && evt.Resource.CustomID == "" {
		return nil, fmt.Errorf("paypal missing resource.custom_id for %s", evt.EventType)
	}

	we := &service.WebhookEvent{
		Channel:       "paypal",
		EventID:       evt.ID,
		EventType:     evt.EventType,
		OrderID:       evt.Resource.CustomID,
		TransactionID: evt.Resource.ID,
		Currency:      strings.ToUpper(evt.Resource.Amount.CurrencyCode),
		// Lifecycle payloads carry no resource.amount at all — exempt them
		// from the underpayment check (see WebhookEvent.SkipAmountCheck).
		SkipAmountCheck: isPaypalLifecycleEvent(evt.EventType),
	}

	// Refund events: resource.id is the refund ID, not the capture ID we
	// stored when the original CAPTURE.COMPLETED arrived. PayPal publishes
	// the parent capture as links[rel="up"]; extract the trailing path
	// segment so the refund handler can find the original payment row.
	if isPaypalRefundEvent(evt.EventType) {
		for _, l := range evt.Resource.Links {
			if l.Rel == "up" {
				if id := lastPathSegment(l.Href); id != "" {
					we.TransactionID = id
				}
				break
			}
		}
	}

	// Subscription events: BILLING.SUBSCRIPTION.* events have their `id` AS
	// the subscription ID (`I-...`). Renewal events (PAYMENT.SALE.*) have
	// it under `billing_agreement_id`. So we pick whichever is present.
	if isPaypalSubscriptionEvent(evt.EventType) {
		we.ExternalSubscriptionID = evt.Resource.ID
	} else if evt.Resource.BillingAgreementID != "" {
		we.ExternalSubscriptionID = evt.Resource.BillingAgreementID
	} else if strings.HasPrefix(evt.EventType, "PAYMENT.CAPTURE.COMPLETED") {
		// PAYMENT.CAPTURE.COMPLETED for a subscription first charge normally
		// carries resource.billing_agreement_id. If it's missing we can't link
		// this capture to a subscription, so the upcoming PAYMENT.SALE.COMPLETED
		// renewal will hit `paypal_renewal_unknown_subscription` and silently
		// no-op. Surface this loudly so the operator can investigate PayPal's
		// delivery (or pair it with the BILLING.SUBSCRIPTION.CREATED event).
		log.Printf("paypal: PAYMENT.CAPTURE.COMPLETED %s missing resource.billing_agreement_id; "+
			"renewal lookup will fail until BILLING.SUBSCRIPTION.CREATED arrives", evt.ID)
	}

	if isPaypalLifecycleEvent(evt.EventType) {
		// BILLING.SUBSCRIPTION.CREATED / UPDATED / CANCELLED: PayPal does
		// not include resource.amount in lifecycle events. Skip parsing;
		// Amount = 0 is fine for these (no payment row is created).
	} else if evt.Resource.Amount.Value == "" {
		// For PAYMENT.* events, amount.value is mandatory; an empty value
		// is malformed and would silently propagate as a paid $0 payment.
		return nil, fmt.Errorf("paypal missing amount.value")
	} else if v, err := strconv.ParseFloat(evt.Resource.Amount.Value, 64); err != nil {
		return nil, fmt.Errorf("paypal amount.value %q: %w", evt.Resource.Amount.Value, err)
	} else {
		we.Amount = v
	}

	if evt.Resource.BillingInfo != nil && evt.Resource.BillingInfo.NextBillingTime != "" {
		if t, err := time.Parse(time.RFC3339, evt.Resource.BillingInfo.NextBillingTime); err == nil {
			we.SubExpiresAt = &t
		} else {
			// Don't fail the whole event for a malformed renewal hint —
			// the renewal handler falls back to "skip UPDATE" if SubExpiresAt
			// is nil. Surface the parse error to the operator's logs.
			log.Printf("paypal: invalid next_billing_time %q for event %s: %v",
				evt.Resource.BillingInfo.NextBillingTime, evt.ID, err)
		}
	}

	if isPaypalRefundEvent(evt.EventType) {
		we.RefundAmount = we.Amount
		we.ExternalRefundID = "paypal-" + evt.Resource.ID
	}
	return we, nil
}

func isPaypalRefundEvent(eventType string) bool {
	return eventType == "PAYMENT.CAPTURE.REFUNDED"
}

// isPaypalSubscriptionEvent is local to the handler — it covers BILLING.SUBSCRIPTION.*
// (created/updated/cancelled). The renewal predicate `isPaypalRenewal` lives
// in service/payment.go because it's part of the OnWebhook dispatch table.
func isPaypalSubscriptionEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "BILLING.SUBSCRIPTION.")
}

// isPaypalLifecycleEvent — PAYMENT.* events carry amount; BILLING.* events
// do not. Use this to drive amount-parsing strictness in parsePaypal.
func isPaypalLifecycleEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "BILLING.")
}

// lastPathSegment returns the trailing non-empty path segment of a URL.
// Used to extract the parent capture id from a PayPal refund event's
// `links[rel="up"].href` (e.g. ".../captures/<id>" → "<id>").
func lastPathSegment(rawURL string) string {
	if i := strings.LastIndex(rawURL, "/"); i >= 0 && i+1 < len(rawURL) {
		return rawURL[i+1:]
	}
	return ""
}

// wrapRawPayload produces a JSONB-safe representation of the raw webhook
// body. We always emit a JSON string (the body escaped). For channels
// whose body IS JSON (Stripe, WeChat), this means a JSON string containing
// JSON. For form-encoded channels (Alipay), it means a JSON string
// containing form-encoded text. Either way it's a valid JSONB string.
func wrapRawPayload(channel string, raw []byte) json.RawMessage {
	encoded, err := json.Marshal(string(raw))
	if err != nil {
		// Fall back to an empty JSON object — should never happen.
		return json.RawMessage(`{}`)
	}
	return encoded
}
