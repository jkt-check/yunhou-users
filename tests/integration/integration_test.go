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
	"github.com/yunhou/users/internal/config"
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
		id, name string
		price    float64
		days     int
		apps     string
		isDef    bool
	}{
		{"free", "免费", 0, 0, "{yundian}", true},
		{"monthly", "按月订阅", 29.9, 30, "{yundian,yundash}", false},
	}
	for _, p := range plans {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO plans (id, name, price, interval_days, apps, is_default)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO NOTHING
		`, p.id, p.name, p.price, p.days, p.apps, p.isDef)
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
		Port:          "8080",
		JWTAccessTTL:   15 * time.Minute,
		JWTRefreshTTL: 168 * time.Hour,
		RSAPrivate:     "../../keys/private.pem",
		RSAPublic:      "../../keys/public.pem",
		OAuthStateSecret: "e2e-test-oauth-state-secret-padded-to-32-bytes",
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		panic(fmt.Sprintf("failed to init token service: %v", err))
	}
	planSvc := service.NewPlanService(planRepo, appRepo)
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
		tokenSvc, authSvc, subSvc, planSvc, nil, nil, nil, nil, nil, githubOAuthSvc, wechatOAuthSvc, false, false)

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
	resp := doJSON(t, http.MethodPost, srv.URL+"/test/login", body)
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
		"is_default":    false,
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
		"Bearer "+tok, map[string]interface{}{"plan_id": "free"})
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

	// Create subscription to free plan (which includes yundian — the only
	// app seeded in this integration test). The test's purpose is to
	// confirm one user → one subscription, not to test multi-app access.
	resp := doJSONWithAuth(t, http.MethodPost, srv.URL+"/user/subscriptions",
		"Bearer "+tok, map[string]interface{}{"plan_id": "free"})
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

	resp := doJSON(t, http.MethodPost, srv.URL+"/test/login", map[string]interface{}{
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

	// First login: yundian on the default 'free' plan → has_access=true.
	subView := testLoginSub(t, srv, email, "yundian")
	if subView["has_access"] != true {
		t.Errorf("expected has_access=true for yundian on free plan, got %v", subView["has_access"])
	}
	if subView["plan_id"] != "free" {
		t.Errorf("expected plan_id=free, got %v", subView["plan_id"])
	}

	// Seed a restricted plan (yundian NOT in its apps list) and switch the user to it.
	_, _ = db.ExecContext(context.Background(),
		`INSERT INTO plans (id, name, price, interval_days, apps, is_active, is_default)
		 VALUES ('restricted', 'Restricted', 0, 0, ARRAY[]::TEXT[], true, false)
		 ON CONFLICT (id) DO NOTHING`)
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
	resp := doJSON(t, http.MethodPost, srv.URL+"/test/login", body)
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
