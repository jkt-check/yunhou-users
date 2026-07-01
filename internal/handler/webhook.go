package handler

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
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
//     metadata.order_id = out_trade_no (set at order creation)
//   - Alipay  : application/x-www-form-urlencoded, out_trade_no IS the order_id,
//     total_amount is in major units (no conversion)
type WebhookHandler struct {
	svc       service.PaymentServiceInterface
	wechatKey []byte // WECHAT_PAY_API_V3_KEY — fallback if verifier can't decrypt
	verifier  middleware.ChannelSignatureVerifier
}

func NewWebhookHandler(svc service.PaymentServiceInterface, wechatAPIv3Key []byte, verifier middleware.ChannelSignatureVerifier) *WebhookHandler {
	return &WebhookHandler{svc: svc, wechatKey: wechatAPIv3Key, verifier: verifier}
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
	case "lemonsqueezy":
		return h.parseLemonsqueezy(raw)
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
func (h *WebhookHandler) parseWeChat(raw []byte) (*service.WebhookEvent, error) {
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

// parseLemonsqueezy extracts fields from a LemonSqueezy JSON:API webhook.
//
// LS payloads carry no top-level unique event ID, so we synthesize one as
// `<event_name>:<data.id>`. For subscription-invoice events (refunds, payment
// success/failed) the resource is the invoice itself — `data.id` is the
// invoice ID, not the subscription ID. Two distinct renewal invoices on the
// same subscription thus dedupe independently, which is what we want.
//
//	{
//	  "meta": { "event_name": "order_created", "custom_data": { "order_id": "<uuid>", "sub_expires_at": "..." } },
//	  "data": { "type": "orders|subscriptions|subscription-invoices", "id": "1", "attributes": { "total": 2990, "currency": "usd", ... } }
//	}
//
// custom_data is ABSENT on subscription-invoice events (per LS docs). For
// those, the refund path looks up the payment by (channel, external_txn_id),
// which we set to data.attributes.subscription_id (matches the originating
// subscription_created payment row).
func (h *WebhookHandler) parseLemonsqueezy(raw []byte) (*service.WebhookEvent, error) {
	var evt struct {
		Meta struct {
			EventName  string                 `json:"event_name"`
			CustomData map[string]interface{} `json:"custom_data"`
		} `json:"meta"`
		Data struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			Attributes struct {
				Total          json.Number `json:"total"`
				RefundedAmount json.Number `json:"refunded_amount"`
				Currency       string      `json:"currency"`
				SubscriptionID string      `json:"subscription_id"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("lemonsqueezy body: %w", err)
	}
	if evt.Meta.EventName == "" || evt.Data.ID == "" {
		return nil, fmt.Errorf("lemonsqueezy missing meta.event_name or data.id")
	}

	we := &service.WebhookEvent{
		Channel:   "lemonsqueezy",
		EventID:   evt.Meta.EventName + ":" + evt.Data.ID,
		EventType: evt.Meta.EventName,
	}

	// external_txn_id mapping — invoice events use the parent subscription id
	// (so the refund's lookup matches the originating subscription_created
	// payment row); order/subscription events use the resource's own id.
	if evt.Data.Type == "subscription-invoices" && evt.Data.Attributes.SubscriptionID != "" {
		we.TransactionID = evt.Data.Attributes.SubscriptionID
	} else {
		we.TransactionID = evt.Data.ID
	}

	// custom_data absent on subscription-invoice events; that's fine.
	if cd := evt.Meta.CustomData; cd != nil {
		if v, ok := cd["order_id"].(string); ok {
			we.OrderID = v
		}
		if v, ok := cd["sub_expires_at"].(string); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				we.SubExpiresAt = &t
			}
		}
	}

	// LS amounts may be decimal cents (e.g. 1499.985). json.Number preserves
	// precision; Round to 2dp before converting to float64 major units.
	if evt.Data.Attributes.Total != "" {
		if cents, err := evt.Data.Attributes.Total.Float64(); err == nil {
			we.Amount = math.Round(cents) / 100
		}
	}
	if evt.Data.Attributes.RefundedAmount != "" {
		if cents, err := evt.Data.Attributes.RefundedAmount.Float64(); err == nil {
			we.RefundAmount = math.Round(cents) / 100
		}
	}
	we.Currency = strings.ToUpper(evt.Data.Attributes.Currency)

	if isLSRefundEvent(evt.Meta.EventName) {
		we.ExternalRefundID = "ls-" + evt.Data.ID
	}
	return we, nil
}

func isLSRefundEvent(eventName string) bool {
	return eventName == "order_refunded" || eventName == "subscription_payment_refunded"
}

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
		} `json:"resource"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("paypal body: %w", err)
	}
	if evt.ID == "" || evt.EventType == "" {
		return nil, fmt.Errorf("paypal missing id or event_type")
	}
	if evt.Resource.CustomID == "" {
		return nil, fmt.Errorf("paypal missing resource.custom_id")
	}

	we := &service.WebhookEvent{
		Channel:       "paypal",
		EventID:       evt.ID,
		EventType:     evt.EventType,
		OrderID:       evt.Resource.CustomID,
		TransactionID: evt.Resource.ID,
		Currency:      strings.ToUpper(evt.Resource.Amount.CurrencyCode),
	}

	// Subscription events: BILLING.SUBSCRIPTION.* events have their `id` AS
	// the subscription ID (`I-...`). Renewal events (PAYMENT.SALE.*) have
	// it under `billing_agreement_id`. So we pick whichever is present.
	if isPaypalSubscriptionEvent(evt.EventType) {
		we.ExternalSubscriptionID = evt.Resource.ID
	} else if evt.Resource.BillingAgreementID != "" {
		we.ExternalSubscriptionID = evt.Resource.BillingAgreementID
	}

	if v, err := strconv.ParseFloat(evt.Resource.Amount.Value, 64); err == nil {
		we.Amount = v
	}

	if evt.Resource.BillingInfo != nil && evt.Resource.BillingInfo.NextBillingTime != "" {
		if t, err := time.Parse(time.RFC3339, evt.Resource.BillingInfo.NextBillingTime); err == nil {
			we.SubExpiresAt = &t
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

func isPaypalRenewal(eventType string) bool {
	return eventType == "PAYMENT.SALE.COMPLETED"
}

func isPaypalSubscriptionEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "BILLING.SUBSCRIPTION.")
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
