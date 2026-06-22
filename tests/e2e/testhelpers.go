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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
)

const (
	defaultDBURL   = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	mockGitHubCode = "mock-github-auth-code"
	mockGitHubUID  = "999888777"
	mockGitHubUser = "e2e_test_user"
	mockGitHubMail = "e2e@test.example.com"
	superAppID     = "yundian"
)

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
	tables := []string{"sessions", "subscriptions", "social_identities", "plans", "apps", "users"}
	for _, tbl := range tables {
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s", tbl)); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedTestData(t *testing.T, db *sqlx.DB) {
	t.Helper()
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

	// Seed super app
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active)
		VALUES ($1, 'E2E Test App', true)
		ON CONFLICT (app_id) DO NOTHING
	`, superAppID)
	if err != nil {
		t.Fatalf("seed super app: %v", err)
	}
}

func setupE2EServer(t *testing.T) (*gin.Engine, *httptest.Server, *sqlx.DB) {
	t.Helper()

	// Short-circuit real OAuth provider HTTP calls — e2e drives login with
	// arbitrary provider_token strings that need stable, deterministic UIDs
	// rather than real GitHub user IDs.
	restoreVerifier := service.SetProviderVerifier(stubE2EProviderVerifier)
	t.Cleanup(restoreVerifier)

	db := connectDB(t)
	t.Cleanup(func() { db.Close() })
	cleanupDB(t, db)
	seedTestData(t, db)

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
	}

	// Repos
	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)

	// Services
	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		t.Fatalf("new token service: %v", err)
	}

	planSvc := service.NewPlanService(planRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	router.Setup(context.Background(), engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc)

	return engine, nil, db
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

func extractQuery(t *testing.T, rawURL, key string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	return u.Query().Get(key)
}

// stubE2EProviderVerifier mirrors the legacy in-process provider stub: the
// provider_token doubles as a stable user identifier so e2e tests can drive
// login flows without standing up a fake GitHub/Google HTTP server.
func stubE2EProviderVerifier(_ context.Context, provider, token string) (*service.ProviderUserInfo, error) {
	switch provider {
	case "github":
		return &service.ProviderUserInfo{
			Provider:    "github",
			ProviderUID: "github_" + token,
			Email:       fmt.Sprintf("%s@github.test", token),
			Nickname:    "GitHub " + token,
		}, nil
	case "google":
		return &service.ProviderUserInfo{
			Provider:    "google",
			ProviderUID: "google_" + token,
			Email:       fmt.Sprintf("%s@google.test", token),
			Nickname:    "Google " + token,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
