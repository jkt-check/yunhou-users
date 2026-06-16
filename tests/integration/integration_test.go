package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/google/uuid"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

func dbURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

func newUUID() string {
	return uuid.New().String()
}

func setupDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", dbURL())
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tables := []string{"sessions", "subscriptions", "social_identities", "apps", "users"}
	for _, tbl := range tables {
		db.ExecContext(context.Background(), "DELETE FROM "+tbl)
	}
	return db
}

func setupServer(db *sqlx.DB) *httptest.Server {
	projectDir := "/home/ubuntu/code/yunhou-users"
	cfg := &config.Config{
		Port:           "8080",
		RSAPrivate:     projectDir + "/keys/private.pem",
		RSAPublic:      projectDir + "/keys/public.pem",
		JWTAccessTTL:   "15m",
		JWTRefreshTTL:  "168h",
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		panic(fmt.Sprintf("failed to init token service: %v", err))
	}
	authSvc := service.NewAuthService(userRepo, identityRepo, appRepo, subRepo, sessionRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo)
	oauth := service.NewOAuthProvider(cfg)

	engine := gin.New()
	gin.SetMode(gin.TestMode)
	router.Setup(engine, appRepo, userRepo, identityRepo, subRepo, sessionRepo, tokenSvc, authSvc, subSvc, oauth)

	return httptest.NewServer(engine)
}

func doJSON(t *testing.T, method, url string, body interface{}) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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
	b, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(b, &result)
	return result
}

func doWithAuth(t *testing.T, method, url, appID, appSecret string, body interface{}) *http.Response {
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
	req.Header.Set("X-App-Secret", appSecret)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// Bootstrap: create the first app directly in DB so we can authenticate further requests
func bootstrapApp(t *testing.T, db *sqlx.DB) (appID, plainSecret string) {
	t.Helper()
	appID = newUUID()
	plainSecret = service.GenerateRefreshToken()[:24]
	hashed, _ := util.HashSecret(plainSecret)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO apps (id, secret, name, redirect_uris, providers, default_plan)
		VALUES ($1, $2, 'bootstrap-app', ARRAY['http://localhost:3000/callback'], ARRAY['github','google','wechat'], 'free')
	`, appID, hashed)
	if err != nil {
		t.Fatalf("bootstrap app: %v", err)
	}
	return appID, plainSecret
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

func TestAppCRUD(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID, appSecret := bootstrapApp(t, db)

	// Create a second app via API
	newAppBody := map[string]interface{}{
		"name":          "second-app",
		"redirect_uris": []string{"http://localhost:3000/callback"},
		"providers":     []string{"github"},
		"default_plan":  "free",
	}
	resp := doWithAuth(t, http.MethodPost, srv.URL+"/apps", appID, appSecret, newAppBody)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create app: status %d, body %s", resp.StatusCode, string(body))
	}
	result := parseJSON(t, resp)
	data := result["data"].(map[string]interface{})
	newAppID := data["app_id"].(string)
	newAppSecret := data["app_secret"].(string)

	// Get the new app
	resp = doWithAuth(t, http.MethodGet, srv.URL+"/apps/"+newAppID, appID, appSecret, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get app: status %d", resp.StatusCode)
	}
	result = parseJSON(t, resp)
	data = result["data"].(map[string]interface{})
	if data["name"] != "second-app" {
		t.Errorf("expected name=second-app, got %v", data["name"])
	}

	// Update the app with its own credentials
	resp = doWithAuth(t, http.MethodPatch, srv.URL+"/apps/"+newAppID, newAppID, newAppSecret, map[string]interface{}{
		"name": "updated-name",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update app: status %d", resp.StatusCode)
	}
	result = parseJSON(t, resp)
	data = result["data"].(map[string]interface{})
	if data["name"] != "updated-name" {
		t.Errorf("expected name=updated-name, got %v", data["name"])
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID, appSecret := bootstrapApp(t, db)
	userID := insertTestUser(t, db)

	// Create subscription via API
	resp := doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID, appSecret, map[string]interface{}{
		"user_id": userID,
		"app_id":  appID,
		"plan":    "paid",
	})
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

	// Get subscription
	resp = doWithAuth(t, http.MethodGet, srv.URL+"/subscriptions/"+subID, appID, appSecret, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get subscription: status %d", resp.StatusCode)
	}

	// Cancel subscription
	resp = doWithAuth(t, http.MethodDelete, srv.URL+"/subscriptions/"+subID, appID, appSecret, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel subscription: status %d", resp.StatusCode)
	}

	// Verify cancelled
	db.GetContext(context.Background(), &status, `SELECT status FROM subscriptions WHERE id = $1`, subID)
	if status != "cancelled" {
		t.Errorf("expected cancelled, got %s", status)
	}
}

func TestSubscriptionWithExpiry(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID, appSecret := bootstrapApp(t, db)
	userID := insertTestUser(t, db)

	expiresAt := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339)
	resp := doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID, appSecret, map[string]interface{}{
		"user_id":    userID,
		"app_id":     appID,
		"plan":       "trial",
		"expires_at": expiresAt,
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create subscription with expiry: status %d, body %s", resp.StatusCode, string(body))
	}
	result := parseJSON(t, resp)
	data := result["data"].(map[string]interface{})
	if data["plan"] != "trial" {
		t.Errorf("expected plan=trial, got %v", data["plan"])
	}
}

func TestDuplicateSubscriptionRejected(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID, appSecret := bootstrapApp(t, db)
	userID := insertTestUser(t, db)

	// Create first subscription
	resp := doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID, appSecret, map[string]interface{}{
		"user_id": userID,
		"app_id":  appID,
		"plan":    "free",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first subscription: status %d", resp.StatusCode)
	}
	parseJSON(t, resp)

	// Try duplicate
	resp = doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID, appSecret, map[string]interface{}{
		"user_id": userID,
		"app_id":  appID,
		"plan":    "paid",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 for duplicate, got %d", resp.StatusCode)
	}
}

func TestAppAuthRequired(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodPost, srv.URL+"/apps", map[string]interface{}{
		"name":          "no-auth",
		"redirect_uris": []string{"http://localhost:3000"},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without app auth, got %d", resp.StatusCode)
	}
}

func TestInvalidAppSecretRejected(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID, _ := bootstrapApp(t, db)

	resp := doWithAuth(t, http.MethodPost, srv.URL+"/apps", appID, "wrong-secret", map[string]interface{}{
		"name":          "bad-secret",
		"redirect_uris": []string{"http://localhost:3000"},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong secret, got %d", resp.StatusCode)
	}
}

func TestExchangeTokenInvalidApp(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodPost, srv.URL+"/token", map[string]interface{}{
		"code":       "fake-code",
		"app_id":     "nonexistent",
		"app_secret": "wrong",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid app, got %d", resp.StatusCode)
	}
}

func TestUserSubscriptionCount(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID, appSecret := bootstrapApp(t, db)
	userID := insertTestUser(t, db)

	doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID, appSecret, map[string]interface{}{
		"user_id": userID,
		"app_id":  appID,
		"plan":    "paid",
	})

	var count int
	db.GetContext(context.Background(), &count, `SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, userID)
	if count != 1 {
		t.Errorf("expected 1 subscription, got %d", count)
	}
}

func TestMultipleAppsShareSameUser(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	appID1, appSecret1 := bootstrapApp(t, db)

	// Create second app
	newAppBody := map[string]interface{}{
		"name":          "app-two",
		"redirect_uris": []string{"http://localhost:4000/callback"},
		"providers":     []string{"github"},
		"default_plan":  "paid",
	}
	resp := doWithAuth(t, http.MethodPost, srv.URL+"/apps", appID1, appSecret1, newAppBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create second app: status %d", resp.StatusCode)
	}
	result := parseJSON(t, resp)
	data := result["data"].(map[string]interface{})
	appID2 := data["app_id"].(string)
	appSecret2 := data["app_secret"].(string)

	// Create shared user
	userID := insertTestUser(t, db)

	// Subscribe to both apps
	resp = doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID1, appSecret1, map[string]interface{}{
		"user_id": userID,
		"app_id":  appID1,
		"plan":    "free",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("subscribe app1: status %d", resp.StatusCode)
	}
	parseJSON(t, resp)

	resp = doWithAuth(t, http.MethodPost, srv.URL+"/subscriptions", appID2, appSecret2, map[string]interface{}{
		"user_id": userID,
		"app_id":  appID2,
		"plan":    "paid",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("subscribe app2: status %d", resp.StatusCode)
	}
	parseJSON(t, resp)

	var count int
	db.GetContext(context.Background(), &count, `SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, userID)
	if count != 2 {
		t.Errorf("expected 2 subscriptions, got %d", count)
	}

	// Cancel app1 subscription, app2 should remain active
	var subID string
	db.GetContext(context.Background(), &subID, `SELECT id FROM subscriptions WHERE user_id = $1 AND app_id = $2`, userID, appID1)
	db.ExecContext(context.Background(), `UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, subID)

	subSvc := service.NewSubscriptionService(repo.NewSubscriptionRepo(db))
	active, _ := subSvc.CheckActive(context.Background(), userID, appID1)
	if active {
		t.Error("cancelled subscription should not be active")
	}
	active, _ = subSvc.CheckActive(context.Background(), userID, appID2)
	if !active {
		t.Error("other app subscription should still be active")
	}
}

func TestAuthorizeRedirectsToGitHub(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodGet, srv.URL+"/authorize?app_id=any&provider=github&redirect_uri=http://localhost:3000/cb&state=test", nil)
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("expected redirect location")
	}
	if !bytes.Contains([]byte(loc), []byte("github.com/login/oauth/authorize")) {
		t.Errorf("expected GitHub OAuth URL, got %s", loc)
	}
}

func TestAuthorizeRejectsUnsupportedProvider(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodGet, srv.URL+"/authorize?app_id=any&provider=facebook&redirect_uri=http://localhost:3000/cb&state=test", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported provider, got %d", resp.StatusCode)
	}
}

func TestAuthorizeMissingParams(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodGet, srv.URL+"/authorize?provider=github", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing params, got %d", resp.StatusCode)
	}
}

func TestTokenRefreshWithInvalidToken(t *testing.T) {
	db := setupDB(t)
	srv := setupServer(db)
	defer srv.Close()

	resp := doJSON(t, http.MethodPost, srv.URL+"/token/refresh", map[string]interface{}{
		"refresh_token": "totally-invalid",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid refresh, got %d", resp.StatusCode)
	}
}

func TestExpiredSubscriptionDetected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	tables := []string{"sessions", "subscriptions", "social_identities", "apps", "users"}
	for _, tbl := range tables {
		db.ExecContext(context.Background(), "DELETE FROM "+tbl)
	}

	appID, _ := bootstrapApp(t, db)
	userID := insertTestUser(t, db)

	// Insert already-expired subscription
	subID := newUUID()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, user_id, app_id, plan, status, expires_at)
		VALUES ($1, $2, $3, 'paid', 'active', now() - interval '1 day')
	`, subID, userID, appID)
	if err != nil {
		t.Fatalf("insert expired sub: %v", err)
	}

	subSvc := service.NewSubscriptionService(repo.NewSubscriptionRepo(db))
	active, _ := subSvc.CheckActive(context.Background(), userID, appID)
	if active {
		t.Error("expired subscription should not be active")
	}

	var status string
	db.GetContext(context.Background(), &status, `SELECT status FROM subscriptions WHERE id = $1`, subID)
	if status != "expired" {
		t.Errorf("expected expired status, got %s", status)
	}
}
