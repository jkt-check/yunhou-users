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

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/service"
)

// WebhookHandler receives signed channel webhooks at POST /webhooks/payment/:channel.
// The signature middleware has already verified the signature by the time
// the handler runs; here we just parse channel-specific payload structure
// and dispatch to PaymentService.OnWebhook.
//
// Channel parsing rules:
//   - Stripe  : application/json, `data.object.{id, metadata.order_id, amount, currency}`
//               amount is in cents → divide by 100 to major units (design doc §"Amount unit convention")
//   - WeChat  : application/json, AES-256-GCM-decrypt resource.ciphertext;
//               metadata.order_id = out_trade_no (set at order creation)
//   - Alipay  : application/x-www-form-urlencoded, out_trade_no IS the order_id,
//               total_amount is in major units (no conversion)
type WebhookHandler struct {
	svc       service.PaymentServiceInterface
	wechatKey []byte // WECHAT_PAY_API_V3_KEY — needed for resource decryption
}

func NewWebhookHandler(svc service.PaymentServiceInterface, wechatAPIv3Key []byte) *WebhookHandler {
	return &WebhookHandler{svc: svc, wechatKey: wechatAPIv3Key}
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
	c.JSON(http.StatusOK, gin.H{
		"code":          0,
		"data":          gin.H{"received": true},
		"domain_action": result.DomainAction,
		"duplicate":     result.DuplicateEvent,
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
					OrderID string `json:"order_id"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		return nil, fmt.Errorf("stripe body: %w", err)
	}

	pi := evt.Data.Object
	return &service.WebhookEvent{
		Channel:       "stripe",
		EventID:       evt.ID,
		EventType:     evt.Type,
		TransactionID: pi.ID,
		OrderID:       pi.Metadata.OrderID,
		Amount:        float64(pi.Amount) / 100, // cents → major units
		Currency:      strings.ToUpper(pi.Currency),
	}, nil
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
	//   TRANSACTION.SUCCESS: { "transaction_id": "...", "amount": { "total": 100, ... }, "out_trade_no": "..." }
	//   TRANSACTION.REFUND:  { "transaction_id": "...", "amount": { "refund": 100, ... }, "out_trade_no": "..." }
	var resource struct {
		TransactionID string `json:"transaction_id"`
		OutTradeNo    string `json:"out_trade_no"`
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
		Currency:      "CNY",                                 // WeChat is always CNY in v1
	}
	if resource.Amount.Refund > 0 {
		we.RefundAmount = float64(resource.Amount.Refund) / 100
	}
	return we, nil
}

// decryptWeChatResource AES-256-GCM decrypts the resource.ciphertext using
// the WECHAT_PAY_API_V3_KEY. The nonce and associated_data are passed
// through to the GCM open call.
func (h *WebhookHandler) decryptWeChatResource(ciphertextB64, nonce, associatedData string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(h.wechatKey)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	// GCM requires exactly 12-byte nonce (Go stdlib hardcodes NonceSize()=12).
	// Without this guard, gcm.Open panics on wrong-length nonces — Gin
	// recovers the panic as a 500, which is a DoS vector.
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce length: got %d, want %d", len(nonce), gcm.NonceSize())
	}
	return gcm.Open(nil, []byte(nonce), ciphertext, []byte(associatedData))
}

// parseAlipay reads the form-encoded body. Alipay sends:
//   out_trade_no (order_id), trade_no (transaction_id), total_amount,
//   refund_amount (if refund event), notify_id (event_id), notify_type (event_type).
func (h *WebhookHandler) parseAlipay(raw []byte) (*service.WebhookEvent, error) {
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("alipay form: %w", err)
	}

	outTradeNo := values.Get("out_trade_no")
	tradeNo := values.Get("trade_no")
	totalAmount := values.Get("total_amount")
	refundAmount := values.Get("refund_amount")
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