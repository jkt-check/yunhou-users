package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_LoginNoSubscription_HasAccessFalse covers spec §10.2
// "Login without any sub → response subscription.plan_id=nil,
// has_access=false, JWT scope empty" and the §8.1 decision-matrix row
// "Unauthenticated-equivalent (no sub)". Phase 2 retired the `free`
// default-plan fallback; a user with no subscription row must now
// receive:
//   - subscription.plan_id="" (no chosenPlan)
//   - subscription.has_access=false
//   - subscription.is_accepting_new=false (chosenPlan is nil)
//   - JWT scope=[] (chosenPlan is nil → scopeForTokenIssuance fails closed)
//
// Approach: log in via the dev-only /test/login to create the user
// (TestLogin does NOT insert a subscriptions row), then call
// /auth/refresh which re-evaluates resolvePlanForTokenIssuance WITHOUT
// the /test/login requestedPlan override — so peekSubscription returns
// (nil, false, nil) and the response reflects the no-subscription
// shape.
func TestE2E_LoginNoSubscription_HasAccessFalse(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	// 1. Create a brand-new user via /test/login?plan_id=monthly. This
	//    inserts a users row + a github social_identities row but NOT a
	//    subscriptions row (TestLogin only mints a token, it never
	//    creates a subscription). The response carries monthly's apps
	//    in scope, but we ignore that — we want the no-subscription
	//    view, which /auth/refresh produces by re-evaluating without
	//    the requestedPlan.
	login := loginAndGetTokens(t, engine, "phase2-no-sub-"+randomSuffix(), "yundian")
	if login.AccessToken == "" {
		t.Fatal("/test/login must succeed (creates user even when no subscription exists)")
	}

	// 2. /auth/refresh → resolves against the user's actual subscription
	//    state (no requestedPlan this time). peekSubscription returns
	//    (nil, false, nil) → chosenPlan=nil, surfacePlanID="", hasAccess=false.
	resp := doRequest(t, engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+login.RefreshToken+`","app_id":"yundian"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status = %d, body = %s", resp.StatusCode, string(resp.Body))
	}

	var refreshed struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Subscription *struct {
				PlanID         string `json:"plan_id"`
				PlanName       string `json:"plan_name"`
				HasAccess      bool   `json:"has_access"`
				IsAcceptingNew bool   `json:"is_accepting_new"`
			} `json:"subscription"`
		} `json:"data"`
	}
	resp.JSON(t, &refreshed)
	if refreshed.Data.AccessToken == "" {
		t.Fatal("refresh must mint a fresh access token even when user has no subscription")
	}
	if refreshed.Data.Subscription == nil {
		t.Fatal("refresh response must include subscription view")
	}

	// 3. Assert the no-subscription response shape.
	if refreshed.Data.Subscription.PlanID != "" {
		t.Errorf("Subscription.PlanID = %q, want empty (no chosen plan)", refreshed.Data.Subscription.PlanID)
	}
	if refreshed.Data.Subscription.PlanName != "" {
		t.Errorf("Subscription.PlanName = %q, want empty (no chosen plan)", refreshed.Data.Subscription.PlanName)
	}
	if refreshed.Data.Subscription.HasAccess {
		t.Error("Subscription.HasAccess must be false for user with no subscription (Phase 2 default-plan removal)")
	}
	if refreshed.Data.Subscription.IsAcceptingNew {
		t.Error("Subscription.IsAcceptingNew must be false when chosenPlan is nil")
	}

	// 4. Decode the JWT and assert scope is empty. scopeForTokenIssuance
	//    returns []string{} when chosenPlan is nil; with the omitempty
	//    tag the JSON layer encodes that as `"scope":[]` (the slice is
	//    non-nil but length 0, which the omitempty rule keeps in the
	//    payload). We accept either absent or [] as "empty".
	payload := decodeJWTPayload(t, refreshed.Data.AccessToken)
	if v, present := payload["scope"]; present {
		arr, ok := v.([]interface{})
		if !ok {
			t.Errorf("JWT scope type = %T, want []interface{} (or absent)", v)
		}
		if len(arr) != 0 {
			t.Errorf("JWT scope = %v, want []", arr)
		}
	}
}

// TestE2E_LoginExpiredSubscription_PreservesPlanID covers spec §10.2
// "Expired sub user → response plan_id=<historical>, has_access=false,
// JWT scope empty" and the §8.1 decision-matrix row "Expired sub
// (any plan)". Phase 2 split the response from the token scope:
//
//   - The response's subscription.plan_id MUST preserve the historical
//     plan id so the BFF can render the renewal CTA ("renew your
//     quarterly plan") instead of misreading as "downgraded to free".
//   - subscription.has_access MUST be false.
//   - subscription.is_accepting_new MUST be false (quarterly is
//     accepting_new_subscriptions=false in the seeded fixture).
//   - JWT scope MUST be [] — the security invariant of Phase 2: a
//     15-minute access token must not carry the previous plan's apps
//     once the subscription has lapsed.
//
// Setup reuses e2eMustSeedExpiredSubUser from extra_test.go (seeds
// user + github identity + quarterly plan + status='active' row whose
// expires_at is one hour in the past). loginAndGetTokens looks the
// seeded user up by email, calls peekSubscription which returns the
// expired row, and resolvePlanForTokenIssuanceWithPlan honours the
// subscription over the requestedPlan (requestedPlan is only used when
// sub is nil).
func TestE2E_LoginExpiredSubscription_PreservesPlanID(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	// Seed user + identity + 'quarterly' plan + active-but-past sub.
	e2eMustSeedExpiredSubUser(t, db)

	// Log in. The seeded user already has an identity bound to the
	// email `expired-sub-user@e2e.test` so TestLogin reuses the user
	// rather than creating a new one. The expired sub row is the
	// observed subscription state.
	login := loginAndGetTokens(t, engine, "expired-sub-user", "yundian")
	if login.AccessToken == "" {
		t.Fatal("login must succeed for expired-but-active subscription (cn-staging 2026-07-23 invariant; Phase 2 preserved)")
	}
	if login.Subscription == nil {
		t.Fatal("login response must include subscription view")
	}

	// Response invariants per spec §8.1 / §10.2.
	if login.Subscription.PlanID != "quarterly" {
		t.Errorf("Subscription.PlanID = %q, want %q (preserved for renewal CTA)",
			login.Subscription.PlanID, "quarterly")
	}
	if login.Subscription.PlanName != "按季订阅" {
		t.Errorf("Subscription.PlanName = %q, want %q", login.Subscription.PlanName, "按季订阅")
	}
	if login.Subscription.HasAccess {
		t.Error("Subscription.HasAccess must be false for an expired subscription")
	}

	// Decode the JWT — scope must be []. scopeForTokenIssuance returns
	// []string{} when expiresAt.Before(now).
	payload := decodeJWTPayload(t, login.AccessToken)
	if v, present := payload["scope"]; present {
		arr, ok := v.([]interface{})
		if !ok {
			t.Errorf("JWT scope type = %T, want []interface{} (or absent)", v)
		}
		if len(arr) != 0 {
			t.Errorf("JWT scope = %v, want [] (Phase 2 security invariant: expired sub must not grant previous plan apps)", arr)
		}
	}

	// Refresh must also produce scope=[] (same invariant; the historical
	// bug returned ErrSubscriptionExpired here).
	refreshResp := doRequest(t, engine, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+login.RefreshToken+`","app_id":"yundian"}`, nil)
	if refreshResp.StatusCode != http.StatusOK {
		t.Fatalf("refresh with expired sub: status = %d, body = %s", refreshResp.StatusCode, string(refreshResp.Body))
	}
	var refreshed struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			Subscription *struct {
				PlanID         string `json:"plan_id"`
				PlanName       string `json:"plan_name"`
				HasAccess      bool   `json:"has_access"`
				IsAcceptingNew bool   `json:"is_accepting_new"`
			} `json:"subscription"`
		} `json:"data"`
	}
	refreshResp.JSON(t, &refreshed)
	if refreshed.Data.AccessToken == "" {
		t.Fatal("refresh must mint a new access token for an expired sub (cn-staging 2026-07-23 invariant)")
	}
	if refreshed.Data.Subscription == nil {
		t.Fatal("refresh response must include subscription view")
	}
	if refreshed.Data.Subscription.PlanID != "quarterly" {
		t.Errorf("refresh Subscription.PlanID = %q, want quarterly (must remain preserved on refresh)",
			refreshed.Data.Subscription.PlanID)
	}
	if refreshed.Data.Subscription.HasAccess {
		t.Error("refresh Subscription.HasAccess must be false for expired sub")
	}
	if refreshed.Data.Subscription.IsAcceptingNew {
		t.Error("refresh Subscription.IsAcceptingNew must be false (quarterly has accepting_new_subscriptions=false)")
	}

	// Refreshed JWT also carries scope=[].
	refreshPayload := decodeJWTPayload(t, refreshed.Data.AccessToken)
	if v, present := refreshPayload["scope"]; present {
		arr, ok := v.([]interface{})
		if !ok {
			t.Errorf("refresh JWT scope type = %T, want []interface{} (or absent)", v)
		}
		if len(arr) != 0 {
			t.Errorf("refresh JWT scope = %v, want []", arr)
		}
	}
}

// decodeJWTPayload extracts the payload (second segment) of a JWT and
// unmarshals it as JSON. No signature verification — the test trusts
// the local issuer and only inspects the unverified claims to assert
// `scope`. Using base64.RawURLEncoding mirrors the JWT spec (RFC 7515
// §2) which uses URL-safe base64 without padding.
func decodeJWTPayload(t *testing.T, token string) map[string]interface{} {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed JWT (expected 3 segments): %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode jwt payload: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse jwt payload: %v\nraw: %s", err, string(raw))
	}
	return out
}
