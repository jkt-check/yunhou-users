// Package paddle provides Paddle Billing-specific billing primitives:
// checkout-transaction creation at order time and subscription state reads
// at renewal time. Real mode wraps the official Go SDK; MockMode serves
// canned responses so dev/e2e suites can drive the full flow without a
// Paddle account.
//
// See docs/plans/2026-10-01-paddle-integration-research.md.
package paddle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	paddle "github.com/PaddleHQ/paddle-go-sdk/v5"
)

// ErrNotConfigured is returned by NewClient for an unknown environment.
var ErrNotConfigured = errors.New("paddle not configured on this deployment")

// Client wraps the Paddle Billing SDK for the two operations
// yunhou-users needs.
type Client struct {
	SDK         *paddle.SDK
	MockMode    bool
	clientToken string
}

// SetClientToken wires the Paddle.js client-side token. It is safe to hand
// to the BFF (it only authorizes opening checkouts / previewing prices);
// the payment service echoes it into orders.provider_intent.
func (c *Client) SetClientToken(tok string) { c.clientToken = tok }

// ClientToken returns the Paddle.js client-side token ("" if unset).
func (c *Client) ClientToken() string { return c.clientToken }

// IsMockMode reports mock mode (PADDLE_MOCK=1).
func (c *Client) IsMockMode() bool { return c.MockMode }

// NewClient builds a real-mode client. env selects the API base:
// "sandbox" → sandbox-api.paddle.com, "live" → api.paddle.com.
func NewClient(apiKey, env string) (*Client, error) {
	var (
		sdk *paddle.SDK
		err error
	)
	switch env {
	case "sandbox":
		sdk, err = paddle.NewSandbox(apiKey)
	case "live":
		sdk, err = paddle.New(apiKey)
	default:
		return nil, fmt.Errorf("%w: unknown PADDLE_ENV %q", ErrNotConfigured, env)
	}
	if err != nil {
		return nil, fmt.Errorf("paddle new client: %w", err)
	}
	return &Client{SDK: sdk}, nil
}

// CheckoutTransaction is the order-time artifact: the Paddle transaction
// id plus its hosted checkout URL, composed by Paddle as the account's
// default payment link + "?_ptxn=<id>".
type CheckoutTransaction struct {
	TransactionID string
	CheckoutURL   string
}

// CreateCheckoutTransaction creates an automatically-collected Paddle
// transaction for a catalog price. custom_data rides the transaction and
// is echoed verbatim on transaction.* webhooks — the order binding anchor.
// currency pins the charge currency: without it Paddle bills buyers in
// their local currency and the order-snapshot amount check would reject
// genuine payments. Empty currency lets Paddle pick (not used in
// production — PaymentService always passes the order's currency).
func (c *Client) CreateCheckoutTransaction(ctx context.Context, priceID string, customData map[string]any, currency string) (*CheckoutTransaction, error) {
	if c.MockMode {
		id := "txn_mock_" + uuid.NewString()
		return &CheckoutTransaction{
			TransactionID: id,
			CheckoutURL:   "https://checkout.paddle.com/mock?_ptxn=" + id,
		}, nil
	}
	if c.SDK == nil {
		return nil, errors.New("paddle client: SDK not wired")
	}
	items := []paddle.CreateTransactionItems{
		*paddle.NewCreateTransactionItemsTransactionItemFromCatalog(&paddle.TransactionItemFromCatalog{
			Quantity: 1,
			PriceID:  priceID,
		}),
	}
	req := &paddle.CreateTransactionRequest{
		Items:      items,
		CustomData: paddle.CustomData(customData),
	}
	if currency != "" {
		req.CurrencyCode = paddle.PtrTo(paddle.CurrencyCode(currency))
	}
	res, err := c.SDK.CreateTransaction(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("paddle create transaction: %w", err)
	}
	if res.Checkout == nil || res.Checkout.URL == nil || *res.Checkout.URL == "" {
		return nil, errors.New("paddle create transaction: response missing checkout.url (set the account default payment link in the Paddle dashboard)")
	}
	return &CheckoutTransaction{TransactionID: res.ID, CheckoutURL: *res.Checkout.URL}, nil
}

// CancelSubscription cancels the channel-side subscription with
// effective_from=next_billing_period (sent explicitly, though the Paddle
// API also defaults to it for active subscriptions): the buyer keeps access
// until the paid period ends and is never charged again. Paddle applies
// it as a scheduled_change and fires subscription.canceled when the
// cancellation takes effect — the local status flip hangs off that
// webhook (service.onPaddleSubscriptionCancelled), so access and billing
// end at the same moment.
func (c *Client) CancelSubscription(ctx context.Context, subscriptionID string) error {
	if c.MockMode {
		return nil
	}
	if c.SDK == nil {
		return errors.New("paddle client: SDK not wired")
	}
	if _, err := c.SDK.CancelSubscription(ctx, &paddle.CancelSubscriptionRequest{
		SubscriptionID: subscriptionID,
		EffectiveFrom:  paddle.PtrTo(paddle.EffectiveFromNextBillingPeriod),
	}); err != nil {
		return fmt.Errorf("paddle cancel subscription: %w", err)
	}
	return nil
}

// UpdateSubscriptionPrice swaps the subscription's catalog price (e.g.
// monthly → yearly) with proration_billing_mode=prorated_immediately:
// Paddle charges the prorated difference NOW and re-anchors billing on
// the new price's cycle. Returns the new next_billed_at (nil when the
// response doesn't carry one — callers fall back to the plan interval).
func (c *Client) UpdateSubscriptionPrice(ctx context.Context, subscriptionID, priceID string) (*time.Time, error) {
	if c.MockMode {
		t := time.Now().AddDate(1, 0, 0).UTC()
		return &t, nil
	}
	if c.SDK == nil {
		return nil, errors.New("paddle client: SDK not wired")
	}
	res, err := c.SDK.UpdateSubscription(ctx, &paddle.UpdateSubscriptionRequest{
		SubscriptionID: subscriptionID,
		Items: paddle.NewPatchField([]paddle.UpdateSubscriptionItems{
			{
				SubscriptionUpdateItemFromCatalog: &paddle.SubscriptionUpdateItemFromCatalog{
					PriceID:  priceID,
					Quantity: 1,
				},
			},
		}),
		ProrationBillingMode: paddle.NewPatchField(paddle.ProrationBillingModeProratedImmediately),
	})
	if err != nil {
		return nil, fmt.Errorf("paddle update subscription: %w", err)
	}
	if res.NextBilledAt == nil || *res.NextBilledAt == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *res.NextBilledAt)
	if err != nil {
		return nil, fmt.Errorf("paddle next_billed_at %q: %w", *res.NextBilledAt, err)
	}
	return &t, nil
}

// GetSubscriptionNextBilledAt returns the subscription's next_billed_at.
// transaction.completed webhooks don't carry the next billing date (it lives
// on the subscription object), so the renewal path resolves it here.
// nil = the subscription has no next billing date (e.g. canceled).
func (c *Client) GetSubscriptionNextBilledAt(ctx context.Context, subscriptionID string) (*time.Time, error) {
	if c.MockMode {
		t := time.Now().AddDate(0, 1, 0).UTC()
		return &t, nil
	}
	if c.SDK == nil {
		return nil, errors.New("paddle client: SDK not wired")
	}
	res, err := c.SDK.GetSubscription(ctx, &paddle.GetSubscriptionRequest{SubscriptionID: subscriptionID})
	if err != nil {
		return nil, fmt.Errorf("paddle get subscription: %w", err)
	}
	if res.NextBilledAt == nil || *res.NextBilledAt == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *res.NextBilledAt)
	if err != nil {
		return nil, fmt.Errorf("paddle next_billed_at %q: %w", *res.NextBilledAt, err)
	}
	return &t, nil
}

// IsSubscriptionGoneError reports whether err is Paddle's "channel-side
// billing relationship is already over" class: the subscription does not
// exist (`not_found`, 404) or the requested action is invalid because the
// subscription is already canceled (`subscription_is_canceled_action_invalid`).
// Detection uses the SDK's typed sentinels (paddle-go-sdk v5:
// *paddleerr.Error matches on Type+Code via errors.Is); the client's own
// %w wrapping preserves the chain. Callers treat this class as a heal
// signal (align local state), never as a retryable failure.
func IsSubscriptionGoneError(err error) bool {
	return errors.Is(err, paddle.ErrNotFound) ||
		errors.Is(err, paddle.ErrSubscriptionIsCanceledActionInvalid)
}
