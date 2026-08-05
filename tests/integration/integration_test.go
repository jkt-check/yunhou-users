package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

func TestMain(m *testing.M) {
	// NOTE: /auth/login was removed (commit 5ef27ce — GitHub is the only
	// login provider now via the /auth/github/redirect → /auth/github/callback
	// flow). This file uses POST /test/login (gated by PAYPAL_L3_E2E_MODE=1,
	// set in setupServer) to mint JWTs without going through OAuth. Don't
	// reintroduce /auth/login calls — use the testLogin helper instead.
	os.Exit(m.Run())
}

func dbURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

func newUUID() string {
	return uuid.New().String()
}

// integrationAppSecret is the plaintext that matches the bcrypt hash seeded
// onto the yundian app row by setupDB. httpDo (which runs as a closure in
// each test goroutine) sets the matching X-App-Secret header on every admin
// /apps request.
const integrationAppSecret = "integration-test-app-secret"

func setupDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", dbURL())
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tables := []string{"sessions", "subscriptions", "social_identities", "plans", "apps", "users"}
	for _, tbl := range tables {
		db.ExecContext(context.Background(), "DELETE FROM "+tbl)
	}

	// Seed plans
	plans := []struct {
		id, name, currency string
		price              float64
		days               int
		apps               string
		trialDays          int
		description        string
		isListed           bool
		acceptingNew       bool
		displayOrder       int
	}{
		{"free", "免费", "CNY", 0, 0, "{yundian}", 0, "免费版（已下线）", false, false, 0},
		{"monthly", "按月订阅", "CNY", 19.9, 30, "{yundian,yundash}", 0, "按月订阅 ¥19.9，自动续费，可随时取消", true, true, 10},
		{"test_free", "Integration Free", "CNY", 0, 0, "{yundian}", 0, "Free integration fixture", false, true, 0},
	}
	for _, p := range plans {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO plans (
				id, name, price, interval_days, apps, is_listed,
				accepting_new_subscriptions, currency, trial_days, description,
				display_order
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (id) DO NOTHING
		`, p.id, p.name, p.price, p.days, p.apps, p.isListed, p.acceptingNew, p.currency, p.trialDays,
			p.description, p.displayOrder)
		if err != nil {
			t.Fatalf("seed plan %s: %v", p.id, err)
		}
	}

	// Seed super app with a bcrypt-hashed secret_hash so InternalAppAuth can
	// verify the matching X-App-Secret. The plaintext is the package-level
	// integrationAppSecret constant above.
	hash, err := util.HashSecret(integrationAppSecret)
	if err != nil {
		t.Fatalf("hash integration app secret: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active, secret_hash)
		VALUES ($1, 'E2E Test App', true, $2)
		ON CONFLICT (app_id) DO UPDATE SET secret_hash = EXCLUDED.secret_hash
	`, "yundian", hash)
	if err != nil {
		t.Fatalf("seed super app: %v", err)
	}

	return db
}

func setupServer(db *sqlx.DB) *httptest.Server {
	// Enable the dev-only /test/login endpoint (PAYPAL_L3_E2E_MODE=1). The
	// handler returns 404 unless this is set; integration tests use it to
	// mint JWTs without going through the GitHub OAuth redirect flow.
	if err := os.Setenv("PAYPAL_L3_E2E_MODE", "1"); err != nil {
		panic(fmt.Sprintf("set PAYPAL_L3_E2E_MODE: %v", err))
	}
	cfg := &config.Config{
		Port:             "8080",
		JWTAccessTTL:     15 * time.Minute,
		JWTRefreshTTL:    168 * time.Hour,
		RSAPrivate:       "../../keys/private.pem",
		RSAPublic:        "../../keys/public.pem",
		OAuthStateSecret: "e2e-test-oauth-state-secret-padded-to-32-bytes",
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	planChangeLogRepo := repo.NewPlanChangeLogRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		panic(fmt.Sprintf("failed to init token service: %v", err))
	}
	planSvc := service.NewPlanService(planRepo, appRepo, planChangeLogRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	// Webhook routes + v2 M1/M3 endpoints are not exercised by integration_test.go;
	// pass nil for the webhook verifier/wechat-key and the two new v2 services so
	// the wiring still compiles.
	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)
	router.Setup(context.Background(), engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc, nil, nil, nil, nil, nil, nil, nil, githubOAuthSvc, wechatOAuthSvc, false, false)

	return httptest.NewServer(engine)
}

func doJSON(t *testing.T, method, url string, body interface{}) *http.Response {
	t.Helper()
	return doJSONWithAuth(t, method, url, "", body)
}

// testLogin mints a JWT pair via the dev-only /test/login endpoint. The
// endpoint is gated by PAYPAL_L3_E2E_MODE=1 (set in setupServer). Each call
// uses a fresh email derived from the supplied seed so tests stay isolated.
func testLogin(t *testing.T, srv *httptest.Server, email, appID string) (access, refresh string) {
	t.Helper()
	body := map[string]interface{}{
		"email":  email,
		"app_id": appID,
	}
	resp := doJSON(t, http.MethodPost, srv.URL+"/test/login?plan_id=monthly", body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("test login: status %d, body %s", resp.StatusCode, string(b))
	}
	result := parseJSON(t, resp)
	data := result["data"].(map[string]interface{})
	return data["access_token"].(string), data["refresh_token"].(string)
}

func doJSONWithAuth(t *testing.T, method, url, auth string, body interface{}) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func parseJSON(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("parse JSON response: %v, body: %s", err, string(b))
	}
	return result
}

func doWithAppAuth(t *testing.T, method, url, appID string, body interface{}) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-App-ID", appID)
	req.Header.Set("X-App-Secret", integrationAppSecret)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func insertTestUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	id := newUUID()
	_, err := db.ExecContext(context.Background(), `INSERT INTO users (id, status) VALUES ($1, 'active')`, id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// --- Tests ---

func TestJWKSEndpoint(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodGet, srv.URL+"/.well-known/jwks.json", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("jwks: status %d", resp.StatusCode)
	}
	result := parseJSON(t, resp)
	keys := result["keys"].([]interface{})
	if len(keys) == 0 {
		t.Fatal("expected at least one key in JWKS")
	}
	jwk := keys[0].(map[string]interface{})
	if jwk["kty"] != "RSA" || jwk["alg"] != "RS256" {
		t.Errorf("unexpected JWKS key: %v", jwk)
	}
}

func TestLoginFlow(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	// Login
	accessToken, refreshToken := testLogin(t, srv, "test-user-123@yundian.test", "yundian")
	if accessToken == "" || refreshToken == "" {
		t.Fatal("tokens are empty")
	}

	// Use access token to get profile. doJSONWithAuth sets Authorization BEFORE send.
	profileResp := doJSONWithAuth(t, http.MethodGet, srv.URL+"/user/profile", "Bearer "+accessToken, nil)
	if profileResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(profileResp.Body)
		profileResp.Body.Close()
		t.Fatalf("profile: status %d, body %s", profileResp.StatusCode, string(body))
	}
	profileResult := parseJSON(t, profileResp)
	if profileResult["code"].(float64) != 0 {
		t.Errorf("profile response code: %v", profileResult["code"])
	}
}

func TestTokenRefresh(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	// Login first
	_, refreshToken := testLogin(t, srv, "refresh-test-user@yundian.test", "yundian")

	// Refresh
	refreshResp := doJSON(t, http.MethodPost, srv.URL+"/auth/refresh", map[string]interface{}{
		"refresh_token": refreshToken,
	})
	if refreshResp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status %d", refreshResp.StatusCode)
	}
	refreshResult := parseJSON(t, refreshResp)
	newData := refreshResult["data"].(map[string]interface{})
	newAccessToken := newData["access_token"].(string)
	newRefreshToken := newData["refresh_token"].(string)
	if newAccessToken == "" || newRefreshToken == "" {
		t.Fatal("new tokens are empty")
	}

	// Old refresh token should be revoked
	oldRefreshResp := doJSON(t, http.MethodPost, srv.URL+"/auth/refresh", map[string]interface{}{
		"refresh_token": refreshToken,
	})
	if oldRefreshResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for old refresh token, got %d", oldRefreshResp.StatusCode)
	}
}

func TestPlanCRUD(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID := "yundian"

	// List plans
	resp := doWithAppAuth(t, http.MethodGet, srv.URL+"/admin/plans", appID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list plans: status %d", resp.StatusCode)
	}
	result := parseJSON(t, resp)
	data := result["data"].([]interface{})
	if len(data) < 2 {
		t.Errorf("expected at least 2 plans, got %d", len(data))
	}

	// Get plan
	resp = doWithAppAuth(t, http.MethodGet, srv.URL+"/admin/plans/free", appID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get plan: status %d", resp.StatusCode)
	}
	result = parseJSON(t, resp)
	planData := result["data"].(map[string]interface{})
	if planData["id"] != "free" {
		t.Errorf("expected free plan, got %v", planData["id"])
	}

	// Create plan
	newPlan := map[string]interface{}{
		"id":            "test-plan",
		"name":          "测试计划",
		"price":         9.9,
		"interval_days": 30,
		"apps":          []string{"yundian"},
	}
	resp = doWithAppAuth(t, http.MethodPost, srv.URL+"/admin/plans", appID, newPlan)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create plan: status %d", resp.StatusCode)
	}

	// Update plan
	resp = doWithAppAuth(t, http.MethodPatch, srv.URL+"/admin/plans/test-plan", appID, map[string]interface{}{
		"price": 19.9,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update plan: status %d", resp.StatusCode)
	}

	// Delete plan
	resp = doWithAppAuth(t, http.MethodDelete, srv.URL+"/admin/plans/test-plan", appID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete plan: status %d", resp.StatusCode)
	}
}

func TestAppCRUD(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID := "yundian"

	// List apps
	resp := doWithAppAuth(t, http.MethodGet, srv.URL+"/apps", appID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list apps: status %d", resp.StatusCode)
	}
	result := parseJSON(t, resp)
	data := result["data"].([]interface{})
	if len(data) == 0 {
		t.Fatal("expected at least one app")
	}

	// Get app
	resp = doWithAppAuth(t, http.MethodGet, srv.URL+"/apps/"+appID, appID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: status %d", resp.StatusCode)
	}

	// Create app
	newApp := map[string]interface{}{
		"app_id": "test-app",
		"name":   "Test App",
	}
	resp = doWithAppAuth(t, http.MethodPost, srv.URL+"/admin/apps", appID, newApp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: status %d", resp.StatusCode)
	}

	// Update app
	resp = doWithAppAuth(t, http.MethodPatch, srv.URL+"/admin/apps/test-app", appID, map[string]interface{}{
		"name": "Updated Test App",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update app: status %d", resp.StatusCode)
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	// Login to get a JWT.
	tok, _ := testLogin(t, srv, "sub-life-user@yundian.test", "yundian")

	// Create subscription via JWT-protected API.
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/user/subscriptions",
		"Bearer "+tok, map[string]interface{}{"plan_id": "test_free"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create subscription: status %d, body %s", resp.StatusCode, string(body))
	}
	result := parseJSON(t, resp)
	data := result["data"].(map[string]interface{})
	subID := data["id"].(string)

	// Verify in DB
	var status string
	db.GetContext(context.Background(), &status, `SELECT status FROM subscriptions WHERE id = $1`, subID)
	if status != "active" {
		t.Errorf("expected active, got %s", status)
	}

	// List subscriptions
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/user/subscriptions", "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list subscriptions: status %d", resp.StatusCode)
	}

	// Cancel
	resp = doJSONWithAuth(t, http.MethodDelete, srv.URL+"/user/subscriptions/"+subID, "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel subscription: status %d", resp.StatusCode)
	}

	db.GetContext(context.Background(), &status, `SELECT status FROM subscriptions WHERE id = $1`, subID)
	if status != "cancelled" {
		t.Errorf("expected cancelled, got %s", status)
	}
}

func TestMultipleAppsShareSameUser(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	// Login to get a JWT and reuse the user across two app logins.
	tok, _ := testLogin(t, srv, "multi-app-user@yundian.test", "yundian")
	profileResp := doJSONWithAuth(t, http.MethodGet, srv.URL+"/user/profile", "Bearer "+tok, nil)
	profileResult := parseJSON(t, profileResp)
	userID := profileResult["data"].(map[string]interface{})["id"].(string)

	// Create subscription to the zero-price integration plan (which includes
	// yundian — the only app seeded in this integration test). The test's purpose is to
	// confirm one user → one subscription, not to test multi-app access.
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/user/subscriptions",
		"Bearer "+tok, map[string]interface{}{"plan_id": "test_free"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create subscription: status %d, body %s", resp.StatusCode, string(body))
	}

	var count int
	db.GetContext(context.Background(), &count, `SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, userID)
	if count != 1 {
		t.Errorf("expected 1 subscription, got %d", count)
	}
}

func TestUnauthorizedAccess(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	// No auth header
	resp := doJSON(t, http.MethodGet, srv.URL+"/user/profile", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}

	// Invalid token
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/user/profile", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	client := &http.Client{}
	resp, _ = client.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", resp.StatusCode)
	}
}

// TestTestLoginMalformedBody covers the dev-only /test/login endpoint's
// input validation. The "unsupported provider" test that lived here was
// retired when /auth/login was removed (commit 5ef27ce); /test/login has
// no provider concept — it mints a JWT directly from email + app_id.
func TestTestLoginMalformedBody(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodPost, srv.URL+"/test/login?plan_id=monthly", map[string]interface{}{
		// missing required "email" field
		"app_id": "yundian",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing email, got %d", resp.StatusCode)
	}
}

func TestLoginHasAccessField(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	const email = "user-with-free-plan@yundian.test"

	// First login: yundian on the explicitly requested monthly plan → has_access=true.
	subView := testLoginSub(t, srv, email, "yundian")
	if subView["has_access"] != true {
		t.Errorf("expected has_access=true for yundian on monthly plan, got %v", subView["has_access"])
	}
	if subView["plan_id"] != "monthly" {
		t.Errorf("expected plan_id=monthly, got %v", subView["plan_id"])
	}

	// Seed a restricted plan (yundian NOT in its apps list) and switch the user to it.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plans (
			id, name, price, interval_days, apps, is_active,
			is_listed, accepting_new_subscriptions, currency, trial_days,
			description, display_order
		) VALUES (
			'restricted', 'Restricted', 0, 0, ARRAY[]::TEXT[], true,
			true, true, 'CNY', 0, 'Restricted integration fixture', 0
		)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed restricted: %v", err)
	}
	tok, _ := testLogin(t, srv, email, "yundian")
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/user/subscriptions",
		"Bearer "+tok, map[string]interface{}{"plan_id": "restricted"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("subscribe restricted: %d", resp.StatusCode)
	}

	// Second login: same user, now on the restricted plan → has_access=false.
	subView2 := testLoginSub(t, srv, email, "yundian")
	if subView2["has_access"] != false {
		t.Errorf("expected has_access=false for yundian on restricted plan, got %v", subView2["has_access"])
	}
}

// testLoginSub calls /test/login and returns the embedded subscription view
// (plan_id, plan_name, has_access) from the response.
func testLoginSub(t *testing.T, srv *httptest.Server, email, appID string) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{"email": email, "app_id": appID}
	resp := doJSON(t, http.MethodPost, srv.URL+"/test/login?plan_id=monthly", body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("test login: status %d, body %s", resp.StatusCode, string(b))
	}
	result := parseJSON(t, resp)
	data := result["data"].(map[string]interface{})
	sub, ok := data["subscription"].(map[string]interface{})
	if !ok {
		t.Fatalf("test login response missing subscription field: %v", data)
	}
	return sub
}

// stubRefundAPI is a no-op refund client so PaymentService can be wired
// without a real PayPal/WeChat refund backend (refund flows are covered
// at the unit level).
type stubRefundAPI struct{}

func (stubRefundAPI) Refund(_ context.Context, _, _ string, _ float64, idempotencyKey string) (string, error) {
	return "re_integration_" + idempotencyKey, nil
}

// setupFullServer wires the COMPLETE router — payment service, webhook
// verifier and the wechat_pay mock branch — so integration tests can
// drive the order → webhook → subscription-activation chain over HTTP.
// The plain setupServer deliberately leaves those services nil (its
// tests only exercise auth/profile/plan/app surfaces).
func setupFullServer(t *testing.T, db *sqlx.DB) *httptest.Server {
	t.Helper()
	if err := os.Setenv("PAYPAL_L3_E2E_MODE", "1"); err != nil {
		t.Fatalf("set PAYPAL_L3_E2E_MODE: %v", err)
	}

	cfg := &config.Config{
		Port:                "8080",
		DatabaseURL:         dbURL(),
		RSAPrivate:          "../../keys/private.pem",
		RSAPublic:           "../../keys/public.pem",
		JWTAccessTTL:        15 * time.Minute,
		JWTRefreshTTL:       168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		OAuthStateSecret:    "e2e-test-oauth-state-secret-padded-to-32-bytes",
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	planChangeLogRepo := repo.NewPlanChangeLogRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)
	orderRepo := repo.NewOrderRepo(db)
	paymentRepo := repo.NewPaymentRepo(db)
	refundRepo := repo.NewRefundRepo(db)
	webhookEventRepo := repo.NewWebhookEventRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		t.Fatalf("new token service: %v", err)
	}
	planSvc := service.NewPlanService(planRepo, appRepo, planChangeLogRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)

	// wechat.Client{MockMode: true} lets CreateOrder mint an order with a
	// deterministic code_url instead of calling api.mch.weixin.qq.com;
	// without it CreateOrder returns ErrWechatPayNotConfigured.
	paymentSvc := service.NewPaymentService(
		db,
		orderRepo, paymentRepo, refundRepo,
		subRepo, planRepo, userRepo,
		webhookEventRepo, auditLogRepo,
		&stubRefundAPI{},
		&wechat.Client{MockMode: true},
		cfg.OrderExpiryDuration,
	)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	providerTokenSvc := service.NewProviderTokenService(appRepo, nil)
	quoteSvc := service.NewQuoteService(planRepo, appRepo)
	chatSvc := service.NewChatService("", "", "", subRepo, planRepo)
	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)
	setupCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// MultiChannelVerifier must carry a WeChat verifier (mock mode) or the
	// wechat_pay webhook route 404s with "unknown channel" (nil verifier).
	mv := &middleware.MultiChannelVerifier{
		WeChat: &middleware.WeChatPayV3Verifier{
			APIv3Key: []byte("integration-test-api-v3-key-1234567890"),
			MockMode: true,
		},
	}
	// wechatPayMock=true flips the webhook verifier + handler mock branches
	// in lockstep (plaintext body accepted, HMAC bypassed).
	router.Setup(setupCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, mv, nil,
		providerTokenSvc, quoteSvc, chatSvc, nil, githubOAuthSvc, wechatOAuthSvc, false, true)

	return httptest.NewServer(engine)
}

// mockWechatPayWebhook fires a wechat_pay webhook event for the given
// order in mock mode: plaintext body shaped like the AES-decrypted
// resource, with the three Wechatpay-* headers present (HMAC bypassed
// when the mock branch is on, but absent headers still 400).
func mockWechatPayWebhook(t *testing.T, srv *httptest.Server, orderID string) *http.Response {
	t.Helper()
	subExpiresAt := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := map[string]interface{}{
		"id":         "evt-integration-" + orderID,
		"event_type": "TRANSACTION.SUCCESS",
		"resource": map[string]interface{}{
			"transaction_id": "tx-integration-" + orderID,
			"out_trade_no":   orderID,
			"amount":         map[string]interface{}{"total": 1, "refund": 0},
			"sub_expires_at": subExpiresAt,
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal webhook body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/payment/wechat_pay", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Signature", "mock-sig-"+orderID)
	req.Header.Set("Wechatpay-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("Wechatpay-Nonce", "mock-nonce-"+orderID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fire webhook: %v", err)
	}
	return resp
}

// TestCreateOrderWebhookActivatesSubscription is the HTTP-level
// end-to-end chain: login → create wechat_pay order → mock webhook →
// subscription activates. This is the integration-layer counterpart of
// the staging smoke step 9 and the e2e wechat_mock suite.
func TestCreateOrderWebhookActivatesSubscription(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "pay-webhook-user@yundian.test", "yundian")

	// Create the order for the monthly plan via wechat_pay.
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create order: status %d, body %s", resp.StatusCode, string(body))
	}
	result := parseJSON(t, resp)
	orderID, ok := result["data"].(map[string]interface{})["id"].(string)
	if !ok || orderID == "" {
		t.Fatalf("create order returned no id: %v", result)
	}

	// Fire the mock webhook → expect 200.
	whResp := mockWechatPayWebhook(t, srv, orderID)
	if whResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(whResp.Body)
		whResp.Body.Close()
		t.Fatalf("webhook: status %d, body %s", whResp.StatusCode, string(body))
	}
	whResp.Body.Close()

	// Subscription must now be active on monthly.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/user/subscriptions", "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list subscriptions: status %d", resp.StatusCode)
	}
	subs := parseJSON(t, resp)["data"].([]interface{})
	if len(subs) == 0 {
		t.Fatal("no subscription after webhook")
	}
	first := subs[0].(map[string]interface{})
	if first["plan_id"] != "monthly" {
		t.Errorf("subscription plan = %v, want monthly", first["plan_id"])
	}
	if first["status"] != "active" {
		t.Errorf("subscription status = %v, want active", first["status"])
	}
}

// TestWebhookDoubleDeliveryIdempotent re-fires the same webhook event;
// the second delivery must be a no-op (HTTP 200, no duplicate
// subscription rows).
func TestWebhookDoubleDeliveryIdempotent(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "pay-idempotent-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create order: status %d, body %s", resp.StatusCode, string(body))
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)

	for i := 0; i < 2; i++ {
		whResp := mockWechatPayWebhook(t, srv, orderID)
		if whResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(whResp.Body)
			whResp.Body.Close()
			t.Fatalf("webhook delivery %d: status %d, body %s", i+1, whResp.StatusCode, string(body))
		}
		whResp.Body.Close()
	}

	var count int
	if err := db.GetContext(context.Background(), &count,
		`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`,
		mustUserID(t, db, "pay-idempotent-user@yundian.test")); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if count != 1 {
		t.Errorf("subscription rows = %d, want 1 (double webhook must be idempotent)", count)
	}
}

// mustUserID resolves the user id for an email. Emails live on
// social_identities, not on users (see repo.go:204).
func mustUserID(t *testing.T, db *sqlx.DB, email string) string {
	t.Helper()
	var id string
	// GetContext(ctx, dest, query, args...) — &id is the scan destination.
	if err := db.GetContext(context.Background(), &id,
		`SELECT user_id FROM social_identities WHERE email = $1 LIMIT 1`, email); err != nil {
		t.Fatalf("find user by email %q: %v", email, err)
	}
	return id
}

// TestConfirmOrderActivatesSubscription drives the BFF-confirm path:
// create order → POST /payments/orders/:id/confirm → subscription
// activates. This covers PaymentService.Confirm — the alternative
// activation path to the webhook (covered by TestCreateOrderWebhook*).
// A second confirm with the same external_txn_id must be idempotent.
func TestConfirmOrderActivatesSubscription(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "confirm-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)

	for i := 0; i < 2; i++ {
		resp = doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders/"+orderID+"/confirm",
			"Bearer "+tok, map[string]interface{}{
				"channel": "wechat_pay", "external_txn_id": "txn-confirm-" + orderID,
			})
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("confirm delivery %d: status %d, body %s", i+1, resp.StatusCode, string(body))
		}
		resp.Body.Close()
	}

	// Subscription activated on monthly.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/user/subscriptions", "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list subscriptions: %d", resp.StatusCode)
	}
	subs := parseJSON(t, resp)["data"].([]interface{})
	if len(subs) == 0 || subs[0].(map[string]interface{})["plan_id"] != "monthly" {
		t.Fatalf("subscription not activated via confirm: %v", subs)
	}
}

// TestOrderLifecycleQueryCancel covers the order query + cancel
// endpoints: list → get → cancel → status flips to cancelled.
func TestOrderLifecycleQueryCancel(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "order-lifecycle-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)

	// List orders.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/payments/orders", "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list orders: %d", resp.StatusCode)
	}
	list := parseJSON(t, resp)["data"].([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["id"] != orderID {
		t.Fatalf("list orders mismatch: %v", list)
	}

	// Get order.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/payments/orders/"+orderID, "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get order: %d", resp.StatusCode)
	}
	if parseJSON(t, resp)["data"].(map[string]interface{})["status"] != "pending" {
		t.Fatalf("order not pending: %v", parseJSON(t, resp))
	}

	// Cancel.
	resp = doJSONWithAuth(t, http.MethodDelete, srv.URL+"/payments/orders/"+orderID, "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel order: %d", resp.StatusCode)
	}
	resp.Body.Close()
	var status string
	if err := db.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatalf("read order status: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("order status = %q, want cancelled", status)
	}
}

// TestRefundFlow confirms a payment, then refunds it through the
// idempotency-keyed refund endpoint and reads it back.
func TestRefundFlow(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "refund-user@yundian.test", "yundian")

	// Create + confirm → paid payment row.
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)

	resp = doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders/"+orderID+"/confirm",
		"Bearer "+tok, map[string]interface{}{"channel": "wechat_pay", "external_txn_id": "txn-refund-" + orderID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d", resp.StatusCode)
	}
	paymentID := parseJSON(t, resp)["data"].(map[string]interface{})["payment_id"].(string)
	resp.Body.Close()

	// Refund with an idempotency key.
	body, _ := json.Marshal(map[string]interface{}{"payment_id": paymentID, "amount": 19.9})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/refunds", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build refund request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "integration-refund-"+orderID)
	req.Header.Set("Authorization", "Bearer "+tok)
	refundResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fire refund: %v", err)
	}
	if refundResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(refundResp.Body)
		refundResp.Body.Close()
		t.Fatalf("refund: status %d, body %s", refundResp.StatusCode, string(b))
	}
	refundID := parseJSON(t, refundResp)["data"].(map[string]interface{})["id"].(string)

	// Read it back via GET /refunds/:id.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/refunds/"+refundID, "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get refund: %d", resp.StatusCode)
	}
	refundView := parseJSON(t, resp)["data"].(map[string]interface{})
	if refundView["id"] != refundID {
		t.Fatalf("get refund id mismatch: %v", refundView)
	}

	// And via the payment's refund list.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/payments/"+paymentID+"/refunds", "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list payment refunds: %d", resp.StatusCode)
	}
	list := parseJSON(t, resp)["data"].([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["id"] != refundID {
		t.Fatalf("payment refunds mismatch: %v", list)
	}
}

// TestPaymentQueryEndpoints covers the payments read surfaces after a
// confirmed payment: GET /payments and GET /payments/:id.
func TestPaymentQueryEndpoints(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "payments-query-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)

	resp = doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders/"+orderID+"/confirm",
		"Bearer "+tok, map[string]interface{}{"channel": "wechat_pay", "external_txn_id": "txn-payquery-" + orderID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d", resp.StatusCode)
	}
	paymentID := parseJSON(t, resp)["data"].(map[string]interface{})["payment_id"].(string)
	resp.Body.Close()

	// GET /payments list.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/payments", "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list payments: %d", resp.StatusCode)
	}
	payments := parseJSON(t, resp)["data"].([]interface{})
	if len(payments) != 1 || payments[0].(map[string]interface{})["id"] != paymentID {
		t.Fatalf("payments list mismatch: %v", payments)
	}

	// GET /payments/:id.
	resp = doJSONWithAuth(t, http.MethodGet, srv.URL+"/payments/"+paymentID, "Bearer "+tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get payment: %d", resp.StatusCode)
	}
	paymentView := parseJSON(t, resp)["data"].(map[string]interface{})
	if paymentView["id"] != paymentID {
		t.Fatalf("get payment id mismatch: %v", paymentView)
	}
}

// TestCreateOrderErrorPaths covers the CreateOrder failure branches:
// unknown plan, unsupported channel, missing auth.
func TestCreateOrderErrorPaths(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "order-error-user@yundian.test", "yundian")

	// Unknown plan.
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "no-such-plan", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("unknown plan: status %d, body %s", resp.StatusCode, string(body))
	}
	resp.Body.Close()

	// Missing auth.
	resp = doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"", map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth: status %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestWebhookRefundEvent drives the TRANSACTION.REFUND webhook path:
// after a confirmed payment, a refund event marks the refund and
// records a refunds row.
func TestWebhookRefundEvent(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "refund-webhook-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)
	txnID := "txn-refundevt-" + orderID

	resp = doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders/"+orderID+"/confirm",
		"Bearer "+tok, map[string]interface{}{"channel": "wechat_pay", "external_txn_id": txnID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Fire a refund event for the same transaction.
	body := map[string]interface{}{
		"id":         "evt-refund-" + orderID,
		"event_type": "TRANSACTION.REFUND",
		"resource": map[string]interface{}{
			"transaction_id": txnID,
			"out_trade_no":   orderID,
			"amount":         map[string]interface{}{"total": 1, "refund": 1},
		},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/payment/wechat_pay", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build refund webhook: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Signature", "mock-sig-"+orderID)
	req.Header.Set("Wechatpay-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("Wechatpay-Nonce", "mock-nonce-"+orderID)
	whResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fire refund webhook: %v", err)
	}
	if whResp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(whResp.Body)
		whResp.Body.Close()
		t.Fatalf("refund webhook: status %d, body %s", whResp.StatusCode, string(bb))
	}
	whResp.Body.Close()

	// Refunds row recorded for the payment.
	var refundCount int
	if err := db.GetContext(context.Background(), &refundCount,
		`SELECT COUNT(*) FROM refunds WHERE payment_id IN (SELECT id FROM payments WHERE order_id = $1)`, orderID); err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	if refundCount == 0 {
		t.Error("expected a refunds row after TRANSACTION.REFUND webhook")
	}
}

// TestWebhookPaymentFailed drives the TRANSACTION.PAY_FAILED webhook
// path: a failed event on a pending order marks the payment failed.
func TestWebhookPaymentFailed(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "pay-failed-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)

	body := map[string]interface{}{
		"id":         "evt-payfail-" + orderID,
		"event_type": "TRANSACTION.PAY_FAILED",
		"resource": map[string]interface{}{
			"transaction_id": "txn-payfail-" + orderID,
			"out_trade_no":   orderID,
			"amount":         map[string]interface{}{"total": 1, "refund": 0},
		},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/payment/wechat_pay", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build pay-failed webhook: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Signature", "mock-sig-"+orderID)
	req.Header.Set("Wechatpay-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("Wechatpay-Nonce", "mock-nonce-"+orderID)
	whResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fire pay-failed webhook: %v", err)
	}
	if whResp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(whResp.Body)
		whResp.Body.Close()
		t.Fatalf("pay-failed webhook: status %d, body %s", whResp.StatusCode, string(bb))
	}
	whResp.Body.Close()

	var failedCount int
	if err := db.GetContext(context.Background(), &failedCount,
		`SELECT COUNT(*) FROM payments WHERE order_id = $1 AND status = 'failed'`, orderID); err != nil {
		t.Fatalf("count failed payments: %v", err)
	}
	if failedCount == 0 {
		t.Error("expected a failed payment row after TRANSACTION.PAY_FAILED webhook")
	}
}

// TestWebhookDisputeEvent drives charge.dispute.created: a confirmed
// payment gets flagged disputed.
func TestWebhookDisputeEvent(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	tok, _ := testLogin(t, srv, "dispute-user@yundian.test", "yundian")

	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders",
		"Bearer "+tok, map[string]interface{}{"plan_id": "monthly", "channel": "wechat_pay"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: %d", resp.StatusCode)
	}
	orderID := parseJSON(t, resp)["data"].(map[string]interface{})["id"].(string)
	txnID := "txn-dispute-" + orderID

	resp = doJSONWithAuth(t, http.MethodPost, srv.URL+"/payments/orders/"+orderID+"/confirm",
		"Bearer "+tok, map[string]interface{}{"channel": "wechat_pay", "external_txn_id": txnID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: %d", resp.StatusCode)
	}
	resp.Body.Close()

	body := map[string]interface{}{
		"id":         "evt-dispute-" + orderID,
		"event_type": "charge.dispute.created",
		"resource": map[string]interface{}{
			"transaction_id": txnID,
			"out_trade_no":   orderID,
			"amount":         map[string]interface{}{"total": 1, "refund": 0},
		},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/payment/wechat_pay", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build dispute webhook: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Signature", "mock-sig-"+orderID)
	req.Header.Set("Wechatpay-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("Wechatpay-Nonce", "mock-nonce-"+orderID)
	whResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fire dispute webhook: %v", err)
	}
	if whResp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(whResp.Body)
		whResp.Body.Close()
		t.Fatalf("dispute webhook: status %d, body %s", whResp.StatusCode, string(bb))
	}
	whResp.Body.Close()

	var disputed bool
	if err := db.GetContext(context.Background(), &disputed,
		`SELECT disputed FROM payments WHERE order_id = $1`, orderID); err != nil {
		t.Fatalf("read disputed flag: %v", err)
	}
	if !disputed {
		t.Error("expected payment flagged disputed after charge.dispute.created")
	}
}

// TestWeChatRedirectNoConfig covers the wechat OAuth redirect shape
// when the app has no wechat oauth block configured: it must fail with
// a clean 4xx (not a 500).
func TestWeChatRedirectNoConfig(t *testing.T) {
	db := setupDB(t)
	srv := setupFullServer(t, db)
	defer srv.Close()

	resp := doJSONWithAuth(t, http.MethodGet,
		srv.URL+"/auth/wechat/redirect?app_id=yundian&redirect_uri=https://example.com/callback",
		"", nil)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("wechat redirect: status %d, body %s", resp.StatusCode, string(body))
	}
	resp.Body.Close()
}
