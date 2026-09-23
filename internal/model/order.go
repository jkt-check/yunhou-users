package model

import (
	"encoding/json"
	"time"

	"github.com/lib/pq"
)

// Order is a pre-payment intent owned by yunhou-users.
// Created when a user picks a paid plan, BEFORE the frontend opens
// the channel SDK to actually collect money.
//
// plan_id (FK RESTRICT to plans) determines the commercial product
// unambiguously (plans.product_code, migration 027), and the 027 trigger
// keeps subscriptions.product_code consistent with it. Migration 029
// additionally FREEZES the product + billing cycle + benefit spec onto
// the order row (the snapshot columns below): the payment callback path
// honors the snapshot, so later operator edits to the plan never change
// what an already-created order grants. Downstream activation/renewal
// paths resolve the product from the snapshot (or, for pre-029 rows, from
// the order's plan row) — never from "the user's current subscription".
//
// See design doc §"Order" + webhook doc §3 for lifecycle.
type Order struct {
	ID        string    `db:"id" json:"id"`
	UserID    string    `db:"user_id" json:"user_id"`
	PlanID    string    `db:"plan_id" json:"plan_id"`
	Amount    float64   `db:"amount" json:"amount"`         // major currency units (e.g. 29.90 CNY)
	Currency  string    `db:"currency" json:"currency"`     // ISO 4217 (3 chars)
	Status    string    `db:"status" json:"status"`         // pending / paid / failed / refunded / cancelled / expired
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"` // 30 min default; sweeper flips pending→expired after this
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
	// LastReconciledAt is stamped whenever GetOrder drives the active
	// channel-query reconcile path (wechat_pay, non-mock). Used as a
	// rate-limit guard so 500ms FE polls collapse to ~1 outbound call
	// per reconcileMinInterval per order. Not surfaced in JSON — it's
	// an internal reconcile timestamp, not a user-visible field.
	LastReconciledAt time.Time `db:"last_reconciled_at" json:"-"`
	// ProviderIntent holds per-channel metadata written after a
	// channel-specific pre-auth (wechat_pay → {appid, mchid, code_url,
	// out_trade_no}; paypal/alipay populate their own keys). Exposed via
	// json with omitempty so orders without a pre-auth payload don't
	// carry an empty field in the response; the BFF reads `code_url`
	// from here for the WeChat QR render.
	//
	// Pointer so a SQL NULL column (set after migration
	// 010_provider_intent_nullable) scans into a nil *json.RawMessage,
	// and omitempty on a nil pointer fires — without the pointer, sqlx
	// can't scan NULL into a []byte (would error with "unsupported
	// Scan, storing driver.Value type <nil> into type *json.RawMessage").
	ProviderIntent *json.RawMessage `db:"provider_intent" json:"provider_intent,omitempty"`

	// ---- Benefit snapshot (migration 029, Kaya Coding Plan Task 10) ----
	// Frozen at order-creation time from the plan row and its
	// plan_benefit_configs row. The payment callback path (webhook /
	// Confirm / reconcile) honors ONLY this snapshot — later operator
	// edits to the plan (price, interval, deactivation) or to the
	// benefit config never change what an already-created order grants.
	//
	// All snapshot columns are NULLable: NULL marks orders created before
	// 029 (and synthetic renewal rows) — the activation path falls back
	// to the live plan row for those, preserving the pre-029 behavior
	// byte-for-byte. Pointer fields so sqlx scans SQL NULL cleanly; use
	// the Snapshot* accessors instead of dereferencing directly.
	ProductCode      *string `db:"product_code" json:"product_code,omitempty"`
	PlanIntervalDays *int    `db:"plan_interval_days" json:"-"`
	// Benefit* describe the entitlement this order confers. All NULL for
	// products without a benefit mapping (plain kaya-membership). A set
	// BenefitPolicyVersionID always pairs with BenefitGrantMode and an
	// explicit (possibly empty) model set — enforced by the
	// orders_benefit_snapshot_consistent CHECK.
	BenefitPolicyVersionID *string        `db:"benefit_policy_version_id" json:"-"`
	BenefitModelIDs        pq.StringArray `db:"benefit_model_ids" json:"-"`
	// BenefitGrantMode: "subscription" (explicit Coding Plan entitlement)
	// or "gift" (bundle gift — non-stackable by default, design §4.2).
	BenefitGrantMode *string `db:"benefit_grant_mode" json:"-"`
	// OrderKind / UpgradeFromPlanID are the frozen upgrade-rule outcome
	// for coding-plan orders: "new" | "renewal" | "upgrade" (with the
	// superseded plan pinned in UpgradeFromPlanID). NULL on kaya orders —
	// the legacy interval-comparison rules apply there.
	OrderKind         *string `db:"order_kind" json:"-"`
	UpgradeFromPlanID *string `db:"upgrade_from_plan_id" json:"-"`
}

// SnapshotProductCode returns the frozen product ("" = pre-029 legacy row —
// callers fall back to resolving the live plan row).
func (o *Order) SnapshotProductCode() string {
	if o.ProductCode == nil {
		return ""
	}
	return *o.ProductCode
}

// SnapshotIntervalDays returns the frozen billing cycle (0 = no snapshot —
// note a lifetime plan's interval_days=0 snapshots as 0 either way, and
// both mean "open-ended" downstream, so the ambiguity is inert).
func (o *Order) SnapshotIntervalDays() int {
	if o.PlanIntervalDays == nil {
		return 0
	}
	return *o.PlanIntervalDays
}

// SnapshotOrderKind returns the frozen order kind ("" = kaya/legacy order).
func (o *Order) SnapshotOrderKind() string {
	if o.OrderKind == nil {
		return ""
	}
	return *o.OrderKind
}

// SnapshotGrantMode returns the frozen grant mode ("" = no benefit mapping).
func (o *Order) SnapshotGrantMode() string {
	if o.BenefitGrantMode == nil {
		return ""
	}
	return *o.BenefitGrantMode
}
