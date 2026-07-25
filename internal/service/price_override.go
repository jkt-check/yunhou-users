package service

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// PlanAmountOverrideJSON is the env var name operators set to force a
// specific (plan_id → amount) map at runtime — independent of the
// canonical plans.price row in the DB.
//
// Format: JSON object — `{"monthly": 0.01, "yearly": 0.1}`.
// Amounts are in the SAME units the rest of the system uses (yuan, the
// major unit stored in plans.price and surfaced through Quote.amount and
// orders.amount). The plan.Currency row is preserved — the override
// ONLY replaces the numeric amount, never the currency.
//
// Lifecycle:
//   - Parsed once at process boot via init() — direct os.Getenv so we
//     don't have to plumb the parsed map through every constructor that
//     needs it.
//   - Empty / unset env → overrideMap stays nil → Apply() is a no-op.
//   - Malformed JSON → overrideMap stays nil + a one-shot log.Warn so
//     operators notice. We do NOT panic, because a typo in a *test* env
//     shouldn't take down prod if it gets copied there.
//
// Why a process-global and not a per-service struct: this is a per-env
// intent ("this stage is testing"), not a per-request decision. Pinning
// it at boot matches how every other env-driven knob in this codebase
// behaves (WECHAT_PAY_MOCK, JWT_ACCESS_TTL, etc.).
var (
	overrideMap    map[string]float64
	overrideMu     sync.RWMutex
	overrideWarned bool
)

func init() {
	ReloadOverrideFromEnv()
}

// ReloadOverrideFromEnv re-parses the override env var. Production code
// should not need to call this — the package init() above reads the env
// once at boot. Tests call it after flipping the env mid-suite to reset
// the parsed map.
func ReloadOverrideFromEnv() {
	raw := os.Getenv("PLAN_AMOUNT_OVERRIDE_JSON")
	overrideMu.Lock()
	defer overrideMu.Unlock()
	if raw == "" {
		overrideMap = nil
		return
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		// Malformed env — log once (not once-per-call, that would flood
		// logs on every quote/order request). We keep overrideMap nil
		// so Apply() falls back to the canonical DB price.
		if !overrideWarned {
			log.Printf("[price_override] PLAN_AMOUNT_OVERRIDE_JSON malformed (err=%v); override disabled — using DB price verbatim", err)
			overrideWarned = true
		}
		overrideMap = nil
		return
	}
	overrideMap = m
	overrideWarned = false // success → re-arm the warn-once
}

// ApplyPlanAmountOverride returns the override amount for planID if one
// is configured in PLAN_AMOUNT_OVERRIDE_JSON, or originalPrice if the
// env is unset / the plan isn't in the map / the env was malformed.
//
// Reads happen under a RLock — concurrent quote + order requests are
// fine. A mid-flight env edit (only possible in tests via Reload…) is
// not atomic w.r.t. in-flight requests, which is acceptable since this
// is a per-process config knob.
func ApplyPlanAmountOverride(planID string, originalPrice float64) float64 {
	overrideMu.RLock()
	defer overrideMu.RUnlock()
	if overrideMap == nil {
		return originalPrice
	}
	if v, ok := overrideMap[planID]; ok {
		return v
	}
	return originalPrice
}

// OverridesActive reports whether override mode is currently engaged.
// Tests use it to assert they ran Reload() correctly; production code
// shouldn't need it (every call site uses ApplyPlanAmountOverride and
// gets a fall-through if not active).
func OverridesActive() bool {
	overrideMu.RLock()
	defer overrideMu.RUnlock()
	return overrideMap != nil
}
