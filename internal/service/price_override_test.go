package service

import (
	"sync"
	"testing"
)

// withOverrideEnv sets PLAN_AMOUNT_OVERRIDE_JSON in the test process,
// reloads the parsed map, and registers a Cleanup that re-reads the env
// after t.Setenv's own restoration has fired — so the in-memory
// overrideMap matches the env state seen by subsequent tests in the
// same binary. Without this, t.Setenv's auto-restoration runs (in LIFO)
// AFTER our Reload, and the map keeps the test's override values into
// the next test that didn't ask for an override.
//
// Cleanup ordering matters: t.Setenv internally registers a Cleanup to
// restore the env var; we register ours FIRST so it runs SECOND, AFTER
// the restoration. Reversing the registration order would mean our
// Reload sees the test's own env value instead of the restored one —
// the bug that originally masked this pollution.
func withOverrideEnv(t *testing.T, value string) {
	t.Helper()
	t.Cleanup(func() {
		// Runs AFTER t.Setenv's restoration (LIFO: ours registered
		// first, runs last).
		ReloadOverrideFromEnv()
	})
	t.Setenv("PLAN_AMOUNT_OVERRIDE_JSON", value)
	ReloadOverrideFromEnv()
}

// TestApplyPlanAmountOverride_DefaultsToOriginal — when PLAN_AMOUNT_OVERRIDE_JSON
// is unset, ApplyPlanAmountOverride must be a transparent pass-through
// (and OverridesActive must report false). This is the prod steady state.
func TestApplyPlanAmountOverride_DefaultsToOriginal(t *testing.T) {
	withOverrideEnv(t, "")

	if OverridesActive() {
		t.Fatal("OverridesActive() = true with empty env; want false")
	}
	if got := ApplyPlanAmountOverride("monthly", 19.9); got != 19.9 {
		t.Errorf("ApplyPlanAmountOverride(monthly, 19.9) = %v; want 19.9 (no-op)", got)
	}
}

// TestApplyPlanAmountOverride_PresentEnv — when the env is set and the
// plan_id is a key in the override map, ApplyPlanAmountOverride returns
// the override value (NOT the original).
func TestApplyPlanAmountOverride_PresentEnv(t *testing.T) {
	withOverrideEnv(t, `{"monthly":0.01,"yearly":0.1}`)

	if !OverridesActive() {
		t.Fatal("OverridesActive() = false with non-empty env; want true")
	}
	if got := ApplyPlanAmountOverride("monthly", 19.9); got != 0.01 {
		t.Errorf("ApplyPlanAmountOverride(monthly, 19.9) = %v; want 0.01", got)
	}
	if got := ApplyPlanAmountOverride("yearly", 199.9); got != 0.1 {
		t.Errorf("ApplyPlanAmountOverride(yearly, 199.9) = %v; want 0.1", got)
	}
}

// TestApplyPlanAmountOverride_PresentEnvButMissingKey — the override
// map only covers certain plan_ids; other plans must fall through to
// originalPrice. This is critical — adding a new plan must NOT silently
// get a ¥0.01 charge just because the cn-staging test override is set.
func TestApplyPlanAmountOverride_PresentEnvButMissingKey(t *testing.T) {
	withOverrideEnv(t, `{"monthly":0.01}`)

	if got := ApplyPlanAmountOverride("yearly", 199.9); got != 199.9 {
		t.Errorf("ApplyPlanAmountOverride(yearly, 199.9) = %v; want 199.9 (yearly not in override map)", got)
	}
	if got := ApplyPlanAmountOverride("quarterly", 79.9); got != 79.9 {
		t.Errorf("ApplyPlanAmountOverride(quarterly, 79.9) = %v; want 79.9 (quarterly not in override map)", got)
	}
}

// TestApplyPlanAmountOverride_MalformedJSON — malformed env is a
// log-and-noop, NOT a panic. We verify (a) OverridesActive()=false,
// (b) ApplyPlanAmountOverride returns the original price verbatim. This
// protects prod from a typo in env if it's blindly copy-pasted.
func TestApplyPlanAmountOverride_MalformedJSON(t *testing.T) {
	withOverrideEnv(t, `not-json`)

	if OverridesActive() {
		t.Fatal("OverridesActive() = true with malformed env; want false (log-and-noop)")
	}
	if got := ApplyPlanAmountOverride("monthly", 19.9); got != 19.9 {
		t.Errorf("ApplyPlanAmountOverride(monthly, 19.9) = %v; want 19.9 (malformed env → noop)", got)
	}
}

// TestApplyPlanAmountOverride_PartialJSON — a JSON object with one
// unparseable value (string instead of number) must follow the same
// log-and-noop path. Operators who hand-craft the env shouldn't be able
// to crash the binary.
func TestApplyPlanAmountOverride_PartialJSON(t *testing.T) {
	withOverrideEnv(t, `{"monthly":"free"}`)

	if OverridesActive() {
		t.Fatal("OverridesActive() = true with type-mismatched JSON value; want false")
	}
	if got := ApplyPlanAmountOverride("monthly", 19.9); got != 19.9 {
		t.Errorf("ApplyPlanAmountOverride(monthly, 19.9) = %v; want 19.9", got)
	}
}

// TestApplyPlanAmountOverride_ReloadTransitions — confirms that
// ReloadOverrideFromEnv actually flips the active state (this isn't
// just a boot-time read; tests rely on it flipping mid-process).
// Manually flips the env because the test exercises the transition
// itself; withOverrideEnv is overkill (no cleanup needed beyond the
// final empty state, which Reload-after-Cleanup takes care of).
func TestApplyPlanAmountOverride_ReloadTransitions(t *testing.T) {
	withOverrideEnv(t, "")
	t.Setenv("PLAN_AMOUNT_OVERRIDE_JSON", "")
	ReloadOverrideFromEnv()
	if OverridesActive() {
		t.Fatal("step1: OverridesActive() = true; want false")
	}

	t.Setenv("PLAN_AMOUNT_OVERRIDE_JSON", `{"monthly":0.01}`)
	ReloadOverrideFromEnv()
	if !OverridesActive() {
		t.Fatal("step2: OverridesActive() = false; want true")
	}
	if got := ApplyPlanAmountOverride("monthly", 19.9); got != 0.01 {
		t.Errorf("step2: ApplyPlanAmountOverride = %v; want 0.01", got)
	}

	t.Setenv("PLAN_AMOUNT_OVERRIDE_JSON", "")
	ReloadOverrideFromEnv()
	if OverridesActive() {
		t.Fatal("step3: OverridesActive() = true; want false")
	}
	if got := ApplyPlanAmountOverride("monthly", 19.9); got != 19.9 {
		t.Errorf("step3: ApplyPlanAmountOverride = %v; want 19.9", got)
	}
}

// TestApplyPlanAmountOverride_ConcurrentReads — the helper is on a hot
// path (every quote + every order). 100 goroutines × 1000 reads
// confirms the RLock path doesn't deadlock or race. Run with
// `go test -race` to catch data races.
func TestApplyPlanAmountOverride_ConcurrentReads(t *testing.T) {
	withOverrideEnv(t, `{"monthly":0.01,"yearly":0.1}`)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if got := ApplyPlanAmountOverride("monthly", 19.9); got != 0.01 {
					t.Errorf("concurrent ApplyPlanAmountOverride(monthly) = %v; want 0.01", got)
					return
				}
				if got := ApplyPlanAmountOverride("yearly", 199.9); got != 0.1 {
					t.Errorf("concurrent ApplyPlanAmountOverride(yearly) = %v; want 0.1", got)
					return
				}
				if got := ApplyPlanAmountOverride("unknown", 99); got != 99 {
					t.Errorf("concurrent ApplyPlanAmountOverride(unknown) = %v; want 99 (fallthrough)", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
