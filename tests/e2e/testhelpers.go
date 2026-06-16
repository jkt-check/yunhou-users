package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

const (
	defaultDBURL   = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	hmacKey        = "e2e-test-hmac-key-at-least-32-characters-long"
	mockGitHubCode = "mock-github-auth-code"
	mockGitHubUID  = "999888777"
	mockGitHubUser = "e2e_test_user"
	mockGitHubMail = "e2e@test.example.com"
	superAppID     = "00000000-0000-0000-0000-000000000001"
	superAppSecret = "e2e-super-secret"
)

type testApp struct {
	ID     string
	Secret string
	Name   string
}

func connectDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dbURL := envOr("E2E_DATABASE_URL", defaultDBURL)
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		t.Fatalf("connect db: %v — ensure PostgreSQL is running and yunhou_users database exists", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Minute)
	return db
}

func cleanupDB(t *testing.T, db *sqlx.DB) {
	t.Helper()
	tables := []string{"sessions", "subscriptions", "social_identities", "apps", "users"}
	for _, tbl := range tables {
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s", tbl)); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedSuperApp(t *testing.T, db *sqlx.DB) {
	t.Helper()
	secret, err := util.HashSecret(superAppSecret)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO apps (id, secret, name, redirect_uris, providers, default_plan)
		VALUES ($1, $2, 'E2E Super App', '{}', '{github,google,wechat}', 'free')
	`, superAppID, secret)
	if err != nil {
		t.Fatalf("seed super app: %v", err)
	}
}

func setupE2EServer(t *testing.T) (*gin.Engine, *httptest.Server, *sqlx.DB) {
	t.Helper()

	db := connectDB(t)
	t.Cleanup(func() { db.Close() })
	cleanupDB(t, db)
	seedSuperApp(t, db)

	keyDir := t.TempDir()
	privPath := keyDir + "/private.pem"
	pubPath := keyDir + "/public.pem"
	genRSAKeys(t, privPath, pubPath)

	cfg := &config.Config{
		Port:               "0",
		DatabaseURL:        envOr("E2E_DATABASE_URL", defaultDBURL),
		RSAPrivate:         privPath,
		RSAPublic:          pubPath,
		GitHubClientID:     "e2e-fake-client-id",
		GitHubClientSecret: "e2e-fake-client-secret",
		JWTAccessTTL:       "15m",
		JWTRefreshTTL:      "168h",
		StateHMACKey:       hmacKey,
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)

	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		t.Fatalf("new token service: %v", err)
	}

	authSvc := service.NewAuthService(userRepo, identityRepo, appRepo, subRepo, sessionRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo)

	mockGitHub := newMockGitHubServer(t)
	t.Cleanup(func() { mockGitHub.Close() })

	oauth := service.NewOAuthProvider(cfg, appRepo)
	oauth.Client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &redirectTransport{url: mockGitHub.URL, transport: http.DefaultTransport},
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	router.Setup(engine, appRepo, userRepo, identityRepo, subRepo, sessionRepo, tokenSvc, authSvc, subSvc, oauth, hmacKey)

	return engine, mockGitHub, db
}

func newMockGitHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != mockGitHubCode {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"bad_verification_code"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"ghu_mock_token_12345","token_type":"bearer"}`)
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghu_mock_token") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%s,"login":"%s","avatar_url":"https://avatars.test/u/%s","email":"%s"}`,
			mockGitHubUID, mockGitHubUser, mockGitHubUID, mockGitHubMail)
	})

	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"email":"%s","primary":true,"verified":true}]`, mockGitHubMail)
	})

	return httptest.NewServer(mux)
}

type redirectTransport struct {
	url       string
	transport http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := t.url + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, target, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return t.transport.RoundTrip(newReq)
}

func genRSAKeys(t *testing.T, privPath, pubPath string) {
	t.Helper()
	if err := exec.Command("sh", "-c",
		fmt.Sprintf("openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out %s && openssl rsa -pubout -in %s -out %s", privPath, privPath, pubPath),
	).Run(); err != nil {
		t.Fatalf("generate RSA keys: %v", err)
	}
}

// --- HTTP helpers ---

type httpResponse struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

func doRequest(t *testing.T, engine *gin.Engine, method, path, body string, headers map[string]string) *httpResponse {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return &httpResponse{StatusCode: w.Code, Body: w.Body.Bytes(), Headers: w.Header()}
}

func (r *httpResponse) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("parse json: %v\nbody: %s", err, string(r.Body))
	}
}

func (r *httpResponse) Location() string {
	return r.Headers.Get("Location")
}

func extractQuery(t *testing.T, rawURL, key string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	return u.Query().Get(key)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func createAppViaHTTP(t *testing.T, engine *gin.Engine, name string, redirectURIs []string) testApp {
	t.Helper()
	urisJSON, _ := json.Marshal(redirectURIs)
	body := fmt.Sprintf(`{"name":"%s","redirect_uris":%s,"default_plan":"free"}`, name, urisJSON)
	headers := map[string]string{
		"X-App-ID":    superAppID,
		"X-App-Secret": superAppSecret,
	}
	resp := doRequest(t, engine, http.MethodPost, "/apps", body, headers)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: expected 201, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var result struct {
		Data struct {
			AppID     string `json:"app_id"`
			AppSecret string `json:"app_secret"`
			Name      string `json:"name"`
		} `json:"data"`
	}
	resp.JSON(t, &result)
	return testApp{ID: result.Data.AppID, Secret: result.Data.AppSecret, Name: result.Data.Name}
}

func createSubscriptionViaHTTP(t *testing.T, engine *gin.Engine, app testApp, userID, plan string) string {
	t.Helper()
	body := fmt.Sprintf(`{"user_id":"%s","app_id":"%s","plan":"%s"}`, userID, app.ID, plan)
	headers := map[string]string{
		"X-App-ID":    app.ID,
		"X-App-Secret": app.Secret,
	}
	resp := doRequest(t, engine, http.MethodPost, "/subscriptions", body, headers)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create subscription: expected 201, got %d — body: %s", resp.StatusCode, string(resp.Body))
	}
	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp.JSON(t, &result)
	return result.Data.ID
}
