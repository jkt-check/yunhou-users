package e2e

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

// e2eWebhookSecrets holds the in-test secrets used to sign webhook bodies.
// Defined as constants so the matching helpers (signStripe, signWeChat) can
// produce real signatures the verifier will accept.
const (
	e2eStripeSecret = "whsec_e2e_test_secret"
	e2eWeChatKey    = "01234567890123456789012345678901" // 32 bytes
	// Alipay uses RSA2 with a real key pair; generated in setupE2EServerWithVerifier.
)

// stubRefundAPI is the e2e placeholder for the channel refund API. Returns
// a deterministic external_refund_id derived from the idempotency key so
// retries don't collide on UNIQUE(channel, external_refund_id). Real
// Stripe/WeChat/Alipay HTTP clients land in v2.
type stubRefundAPI struct{}

func (stubRefundAPI) Refund(_ context.Context, _, _ string, _ float64, idempotencyKey string) (string, error) {
	return "re_e2e_" + idempotencyKey, nil
}

const (
	defaultDBURL   = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	mockGitHubCode = "mock-github-auth-code"
	mockGitHubUID  = "999888777"
	mockGitHubUser = "e2e_test_user"
	mockGitHubMail = "e2e@test.example.com"
	superAppID     = "yundian"
	// e2eAppSecret is the plaintext secret every seeded app uses. The
	// bcrypt-hashed form lives in apps.secret_hash; tests set the matching
	// X-App-Secret header via appAuthHeaders to authenticate against the
	// InternalAppAuth middleware.
	e2eAppSecret = "e2e-test-app-secret-do-not-use-in-prod"
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
	// Order matters: child tables first.
	tables := []string{
		"refunds",
		"payments",
		"webhook_events",
		"audit_log",
		"orders",
		"sessions",
		"subscriptions",
		"social_identities",
		"plans",
		"apps",
		"users",
	}
	for _, tbl := range tables {
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s", tbl)); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedTestData(t *testing.T, db *sqlx.DB) {
	t.Helper()
	// Plans use a partial unique index `plans_one_default` (one is_default=true
	// per table). With multiple tests running in parallel, ON CONFLICT(id) DO NOTHING
	// doesn't help — that handles the PK conflict, not the partial unique. So
	// we first clear is_default on every row, then upsert the seed plans.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE plans SET is_default = false`); err != nil {
		t.Fatalf("clear is_default: %v", err)
	}

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
			ON CONFLICT (id) DO UPDATE SET
				name = EXCLUDED.name,
				price = EXCLUDED.price,
				interval_days = EXCLUDED.interval_days,
				apps = EXCLUDED.apps,
				is_default = EXCLUDED.is_default
		`, p.id, p.name, p.price, p.days, p.apps, p.isDef)
		if err != nil {
			t.Fatalf("seed plan %s: %v", p.id, err)
		}
	}

	// Seed super app (idempotent on app_id PK). Apps carry a bcrypt-hashed
	// secret_hash so InternalAppAuth can verify X-App-Secret. The plaintext
	// (e2eAppSecret) lives at the top of this file; tests authenticate via
	// appAuthHeaders(appID). The config JSONB carries per-app OAuth
	// provider settings — we populate the WeChat OAuth block here so the
	// /auth/wechat/* e2e tests (wechat_mock_test.go) can run without
	// first having to mutate the DB. Real-mode flow uses AppID/AppSecret
	// from this config; mock mode just needs callback_urls whitelisted.
	secretHash, err := util.HashSecret(e2eAppSecret)
	if err != nil {
		t.Fatalf("hash e2e app secret: %v", err)
	}
	wechatOAuthConfig := `{
		"oauth_providers": {
			"wechat": {
				"app_id": "wxe2e0000000000",
				"app_secret": "e2e-test-app-secret-padded-to-32",
				"callback_urls": [
					"https://staging.yunhouai.com/auth/callback",
					"https://bff.example.com/auth/wechat-callback"
				]
			}
		}
	}`
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active, secret_hash, config)
		VALUES ($1, 'E2E Test App', true, $2, $3::jsonb)
		ON CONFLICT (app_id) DO UPDATE SET
			secret_hash = EXCLUDED.secret_hash,
			config = EXCLUDED.config
	`, superAppID, secretHash, wechatOAuthConfig)
	if err != nil {
		t.Fatalf("seed super app: %v", err)
	}

	_, err = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active, secret_hash, config)
		VALUES ('yundash', 'E2E Paid App', true, $1, $2::jsonb)
		ON CONFLICT (app_id) DO UPDATE SET
			secret_hash = EXCLUDED.secret_hash,
			config = EXCLUDED.config
	`, secretHash, wechatOAuthConfig)
	if err != nil {
		t.Fatalf("seed paid app: %v", err)
	}
}

func setupE2EServer(t *testing.T) (*gin.Engine, *httptest.Server, *sqlx.DB) {
	t.Helper()

	// Enable the dev-only /test/login endpoint (PAYPAL_L3_E2E_MODE=1) so the
	// e2e suite can mint JWTs without going through the GitHub OAuth redirect
	// flow. The handler returns 404 unless this is set.
	if err := os.Setenv("PAYPAL_L3_E2E_MODE", "1"); err != nil {
		t.Fatalf("set PAYPAL_L3_E2E_MODE: %v", err)
	}

	db := connectDB(t)
	t.Cleanup(func() { db.Close() })
	cleanupDB(t, db)
	seedTestData(t, db)

	keyDir := t.TempDir()
	privPath := keyDir + "/private.pem"
	pubPath := keyDir + "/public.pem"
	genRSAKeys(t, privPath, pubPath)

	cfg := &config.Config{
		Port:                "0",
		DatabaseURL:         envOr("E2E_DATABASE_URL", defaultDBURL),
		RSAPrivate:          privPath,
		RSAPublic:           pubPath,
		GitHubClientID:      "e2e-fake-client-id",
		GitHubClientSecret:  "e2e-fake-client-secret",
		JWTAccessTTL:        15 * time.Minute,
		JWTRefreshTTL:       168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:     1 * time.Minute,
		OAuthStateSecret:    "e2e-test-oauth-state-secret-padded-to-32-bytes",
	}

	// Repos
	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
	appRepo := repo.NewAppRepo(db)
	subRepo := repo.NewSubscriptionRepo(db)
	sessionRepo := repo.NewSessionRepo(db)
	orderRepo := repo.NewOrderRepo(db)
	paymentRepo := repo.NewPaymentRepo(db)
	refundRepo := repo.NewRefundRepo(db)
	webhookEventRepo := repo.NewWebhookEventRepo(db)
	auditLogRepo := repo.NewAuditLogRepo(db)

	// Services
	tokenSvc, err := service.NewTokenService(cfg, sessionRepo, subRepo)
	if err != nil {
		t.Fatalf("new token service: %v", err)
	}

	planSvc := service.NewPlanService(planRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)

	// Payment service with stub refund API — e2e tests for /refunds land in M5.
	// wechat_pay mock client lets e2e tests create wechat_pay orders without
	// hitting api.mch.weixin.qq.com — the mock client mints a deterministic
	// code_url from out_trade_no. Without this, PaymentService.CreateOrder
	// returns ErrWechatPayNotConfigured (because no real cert + key are
	// wired) and WeChat order tests fail at the order-creation step.
	paymentSvc := service.NewPaymentService(
		db,
		orderRepo, paymentRepo, refundRepo,
		subRepo, planRepo, userRepo,
		webhookEventRepo, auditLogRepo,
		&stubRefundAPI{},
		&wechat.Client{MockMode: true},
		cfg.OrderExpiryDuration,
	)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	// providerTokenSvc is wired with nil paypal fetcher; the e2e tests for
	// /provider-token in provider_token_test.go don't currently exercise the
	// paypal upstream path. Adding paypal e2e later requires wiring a stub
	// upstream.
	providerTokenSvc := service.NewProviderTokenService(appRepo, nil)
	quoteSvc := service.NewQuoteService(planRepo, appRepo)
	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)
	// Cancellable context so rate-limit cleanup goroutines die at test end
	// (see setupE2EServerWithVerifier for the full rationale).
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	t.Cleanup(cancelSetup)
	router.Setup(setupCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, &middleware.MultiChannelVerifier{}, nil,
		providerTokenSvc, quoteSvc, githubOAuthSvc, wechatOAuthSvc, false, false)

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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// E2EServer is the configured engine + DB + verifier setup that payment
// and webhook tests share. setupE2EServer (no verifier) is for the auth
// tests that don't exercise webhooks.
type E2EServer struct {
	Engine *gin.Engine
	DB     *sqlx.DB
	// Verifier config so test code can sign webhooks.
	StripeSecret string
	WeChatKey    []byte
	// AlipayPublicKey is exposed so tests can verify they emit only
	// payloads whose signature the verifier would accept.
	AlipayPublicPEM []byte
}

// setupE2EServerWithVerifier returns a server wired with real signature
// verifiers (Stripe + WeChat + Alipay) so webhook tests can sign their own
// payloads. Alipay's key pair is generated in-memory per test. WeChat
// pay is in REAL mode — pass wechatPayMock=true via
// setupE2EServerWithMockWeChatPay to flip the mock branch on.
func setupE2EServerWithVerifier(t *testing.T) *E2EServer {
	return setupE2EServerWithVerifierOpts(t, false /* wechatPayMock */)
}

// setupE2EServerWithMockWeChatPay mirrors setupE2EServerWithVerifier but
// turns on the wechat_pay mock branch end-to-end:
//   - WeChatPayV3Verifier.MockMode = true (skip HMAC match)
//   - WebhookHandler.mockWechatPay = true (skip AES-GCM decrypt, accept plaintext)
//   - router.Setup(wechatPayMock=true)
//
// The Stripe / Alipay / PayPal verifiers stay at their default (real)
// configs so non-wechat webhook tests continue to work unmodified. The
// mock branch only affects wechat_pay.
func setupE2EServerWithMockWeChatPay(t *testing.T) *E2EServer {
	return setupE2EServerWithVerifierOpts(t, true /* wechatPayMock */)
}

// setupE2EServerWithVerifierOpts is the shared implementation behind
// setupE2EServerWithVerifier (mock off) and setupE2EServerWithMockWeChatPay
// (mock on). Passing wechatPayMock=true wires the WeChatPayV3Verifier's
// MockMode, the handler's mockWechatPay, and router.Setup's wechatPayMock
// in lockstep — there's no way to get them out of sync.
func setupE2EServerWithVerifierOpts(t *testing.T, wechatPayMock bool) *E2EServer {
	t.Helper()

	// Mirror setupE2EServer's env gate: /test/login is the path loginAndGetToken
	// uses, and without this env the handler returns 404 regardless of which
	// helper set up the server. Idempotent across calls.
	if err := os.Setenv("PAYPAL_L3_E2E_MODE", "1"); err != nil {
		t.Fatalf("set PAYPAL_L3_E2E_MODE: %v", err)
	}

	middleware.ClearPaypalVerifyCache()

	db := connectDB(t)
	t.Cleanup(func() { db.Close() })
	cleanupDB(t, db)
	seedTestData(t, db)

	keyDir := t.TempDir()
	privPath := keyDir + "/private.pem"
	pubPath := keyDir + "/public.pem"
	genRSAKeys(t, privPath, pubPath)

	cfg := &config.Config{
		Port:                "0",
		DatabaseURL:         envOr("E2E_DATABASE_URL", defaultDBURL),
		RSAPrivate:          privPath,
		RSAPublic:           pubPath,
		GitHubClientID:      "e2e-fake-client-id",
		GitHubClientSecret:  "e2e-fake-fake-client-secret",
		JWTAccessTTL:        15 * time.Minute,
		JWTRefreshTTL:       168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:     1 * time.Minute,
		OAuthStateSecret:    "e2e-test-oauth-state-secret-padded-to-32-bytes",
	}

	userRepo := repo.NewUserRepo(db)
	identityRepo := repo.NewSocialIdentityRepo(db)
	planRepo := repo.NewPlanRepo(db)
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
	planSvc := service.NewPlanService(planRepo)
	authSvc := service.NewAuthService(userRepo, identityRepo, planRepo, subRepo, sessionRepo, appRepo, tokenSvc)
	subSvc := service.NewSubscriptionService(subRepo, planSvc)

	paymentSvc := service.NewPaymentService(
		db,
		orderRepo, paymentRepo, refundRepo,
		subRepo, planRepo, userRepo,
		webhookEventRepo, auditLogRepo,
		&stubRefundAPI{},
		&wechat.Client{MockMode: true},
		cfg.OrderExpiryDuration,
	)

	alipayPriv, alipayPubPEM := genAlipayRSAKeyPair(t)
	paypalVerifySrv := newMockPaypalVerifyServer(t)
	cfg.PaypalEnv = "sandbox"
	cfg.PaypalWebhookIDSandbox = "wbh_e2e_paypal"
	cfg.PaypalAPIBaseSandbox = paypalVerifySrv.URL
	cfg.PaypalWebhookIDLive = ""
	cfg.PaypalAPIBaseLive = ""
	mv := &middleware.MultiChannelVerifier{
		Stripe: &middleware.StripeVerifier{Secret: []byte(e2eStripeSecret)},
		WeChat: &middleware.WeChatPayV3Verifier{APIv3Key: []byte(e2eWeChatKey), MockMode: wechatPayMock},
		Alipay: &middleware.AlipayVerifier{PublicKey: mustParseAlipayPubKey(t, alipayPubPEM)},
		Paypal: &middleware.PaypalVerifier{
			HTTPClient:       &http.Client{Timeout: 2 * time.Second},
			SandboxWebhookID: cfg.PaypalWebhookIDSandbox,
			LiveWebhookID:    cfg.PaypalWebhookIDLive,
			SandboxAPIBase:   cfg.PaypalAPIBaseSandbox,
			LiveAPIBase:      cfg.PaypalAPIBaseLive,
			Env:              cfg.PaypalEnv,
		},
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	providerTokenSvc := service.NewProviderTokenService(appRepo, nil)
	quoteSvc := service.NewQuoteService(planRepo, appRepo)
	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	t.Cleanup(cancelSetup)
	router.Setup(setupCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, mv, []byte(e2eWeChatKey),
		providerTokenSvc, quoteSvc, githubOAuthSvc, wechatOAuthSvc, false, wechatPayMock)

	alipayPrivHolder.Store(alipayPriv)

	return &E2EServer{
		Engine:          engine,
		DB:              db,
		StripeSecret:    e2eStripeSecret,
		WeChatKey:       []byte(e2eWeChatKey),
		AlipayPublicPEM: alipayPubPEM,
	}
}

// alipayPrivHolder holds the Alipay private key across tests so signing
// helpers can produce signatures the verifier accepts. Tests don't reach
// into this directly; they call signAlipayForTest.
var alipayPrivHolder atomic.Value // *rsa.PrivateKey

func genAlipayRSAKeyPair(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return priv, pemBytes
}

func mustParseAlipayPubKey(t *testing.T, pemBytes []byte) *rsa.PublicKey {
	t.Helper()
	pub, err := middleware.LoadAlipayPublicKeyFromPEM(pemBytes)
	if err != nil {
		t.Fatalf("parse alipay pub key: %v", err)
	}
	return pub
}

// loginAndGetToken mints a JWT pair via the dev-only /test/login endpoint
// (gated by PAYPAL_L3_E2E_MODE=1, set by TestMain) and returns the access
// token + user id. The legacy /auth/login direct-token path was removed
// by commit 5ef27ce; tests that need a JWT should call this helper.
//
// `token` is reused as the user email (with "@e2e.test" appended) so tests
// can mint distinct users per call while keeping the call-site signature
// close to the legacy helper.
func loginAndGetToken(t *testing.T, engine *gin.Engine, token, appID string) (string, string) {
	t.Helper()
	email := token + "@e2e.test"
	body := fmt.Sprintf(`{"email":%q,"app_id":%q}`, email, appID)
	resp := doRequest(t, engine, http.MethodPost, "/test/login", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test login failed: %d %s", resp.StatusCode, string(resp.Body))
	}
	var lr struct {
		Data struct {
			AccessToken string `json:"access_token"`
			User        struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	resp.JSON(t, &lr)
	return lr.Data.AccessToken, lr.Data.User.ID
}

// loginAndGetRefresh mints a JWT pair via /test/login and returns both
// access and refresh tokens. Used by tests that exercise the refresh-rotation
// contract.
func loginAndGetRefresh(t *testing.T, engine *gin.Engine, token, appID string) (access, refresh string) {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"app_id":%q}`, token+"@e2e.test", appID)
	resp := doRequest(t, engine, http.MethodPost, "/test/login", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test login failed: %d %s", resp.StatusCode, string(resp.Body))
	}
	var lr struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	resp.JSON(t, &lr)
	return lr.Data.AccessToken, lr.Data.RefreshToken
}

func authHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// appAuthHeaders returns the X-App-ID + X-App-Secret pair required to pass
// InternalAppAuth on the /apps/*, /admin/*, and rotate-secret routes. The
// secret must match the bcrypt hash seeded by seedTestData.
func appAuthHeaders(appID string) map[string]string {
	return map[string]string{
		"X-App-ID":     appID,
		"X-App-Secret": e2eAppSecret,
	}
}

// --- Webhook signing helpers (Stripe / WeChat / Alipay / LemonSqueezy) ---

// signStripe produces a Stripe-Signature header for body at unix ts.
func signStripe(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// signLS removed — LemonSqueezy channel was deleted in commit d8f333d
// and the helper is no longer used. (signStripe / signAlipay / signWeChat
// below remain active.)

// signWeChat produces the four headers WeChat sends, given the body.
func signWeChat(key []byte, ts int64, nonce, body string) (timestamp, nonceStr, signature string) {
	tsStr := fmt.Sprintf("%d", ts)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(tsStr + "\n" + nonce + "\n" + body + "\n"))
	return tsStr, nonce, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signAlipay produces a complete signed form-encoded body ready to POST
// to /webhooks/payment/alipay. The params map must NOT include `sign` or
// `sign_type`; those are added by this helper.
func signAlipay(t *testing.T, params map[string]string) string {
	t.Helper()
	privAny := alipayPrivHolder.Load()
	if privAny == nil {
		t.Fatal("alipayPrivHolder not initialized — call setupE2EServerWithVerifier first")
	}
	priv := privAny.(*rsa.PrivateKey)

	// Build canonical string (matching the verifier's algorithm).
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, alipayURLEncodeForTest(k)+"="+alipayURLEncodeForTest(params[k]))
	}
	canonical := strings.Join(parts, "&")

	hashed := sha256.Sum256([]byte(canonical))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("alipay sign: %v", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// Build the full body with sign + sign_type appended (URL-encoded so
	// url.ParseQuery on the receiving side correctly decodes them).
	allParams := make(map[string]string, len(params)+2)
	for k, v := range params {
		allParams[k] = v
	}
	allParams["sign"] = sigB64
	allParams["sign_type"] = "RSA2"

	// Render in original-key order is irrelevant for verification — the
	// verifier re-sorts. We render in insertion order for readability.
	bodyKeys := make([]string, 0, len(allParams))
	for k := range params {
		bodyKeys = append(bodyKeys, k)
	}
	bodyKeys = append(bodyKeys, "sign", "sign_type")
	out := make([]string, 0, len(allParams))
	for _, k := range bodyKeys {
		out = append(out, alipayURLEncodeForTest(k)+"="+alipayURLEncodeForTest(allParams[k]))
	}
	return strings.Join(out, "&")
}

// sortStrings sorts a string slice in place.
func sortStrings(s []string) {
	sort.Strings(s)
}

// alipayURLEncodeForTest mirrors middleware.alipayURLEncode — kept in
// sync with that helper. Alipay's URL encoding is more aggressive than
// net/url's QueryEscape: it percent-encodes EVERY non-alphanumeric character
// including `_`, `-`, `.`, while QueryEscape only encodes characters that
// genuinely need encoding (and uses `+` for space instead of `%20`).
//
// The verifier (production) uses alipayURLEncode; this test signer must
// match it exactly, or signatures don't verify.
func alipayURLEncodeForTest(s string) string {
	const hexChars = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hexChars[c>>4])
			sb.WriteByte(hexChars[c&0xF])
		}
	}
	return sb.String()
}
func encryptForWeChat(t *testing.T, key []byte, plaintext []byte) (ciphertextB64, nonceStr, aad string) {
	t.Helper()
	nonce := "n12byte_test" // GCM requires exactly 12 bytes
	aadStr := "transaction_event"

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	ciphertext := gcm.Seal(nil, []byte(nonce), plaintext, []byte(aadStr))
	return base64.StdEncoding.EncodeToString(ciphertext), nonce, aadStr
}

// ============================================================================
// PayPal webhook harness
// ============================================================================

// newMockPaypalVerifyServer spins up a local httptest server that mimics
// PayPal's verify-webhook-signature endpoint. We can't unit-test against
// PayPal's real API; the verifier only cares that the response payload
// parses as JSON with verification_status=SUCCESS, so a stub is enough.
func newMockPaypalVerifyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		t.Logf("mock-paypal-verify hit: %s %s body=%d bytes", r.Method, r.URL.Path, len(body))
		// Drain body so the request is fully consumed before returning —
		// matches PayPal's behavior and lets httptest close cleanly.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"verification_status":"SUCCESS"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// paypalHeaders builds the 5 signature headers PayPal would have sent.
// The verifier only checks for non-empty values; actual signature
// validation is delegated to PayPal's verify endpoint (mocked above).
func paypalHeaders(transmissionID, sigStub string) map[string]string {
	return map[string]string{
		"PAYPAL-AUTH-ALGO":         "SHA256withRSA",
		"PAYPAL-CERT-URL":          "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-test",
		"PAYPAL-TRANSMISSION-ID":   transmissionID,
		"PAYPAL-TRANSMISSION-SIG":  sigStub,
		"PAYPAL-TRANSMISSION-TIME": time.Now().UTC().Format(time.RFC3339),
	}
}

// paypalCaptureCompletedBody builds a typical PAYMENT.CAPTURE.COMPLETED
// JSON body for a one-time capture.
func paypalCaptureCompletedBody(eventID, captureID, customID string, amount string) []byte {
	return []byte(fmt.Sprintf(`{
		"id": %q,
		"event_type": "PAYMENT.CAPTURE.COMPLETED",
		"resource": {
			"id": %q,
			"custom_id": %q,
			"amount": {"value": %q, "currency_code": "USD"}
		}
	}`, eventID, captureID, customID, amount))
}

// paypalSaleCompletedBody builds a PAYMENT.SALE.COMPLETED renewal body.
func paypalSaleCompletedBody(eventID, saleID, billingAgreementID, customID, nextBillingTime string) []byte {
	return []byte(fmt.Sprintf(`{
		"id": %q,
		"event_type": "PAYMENT.SALE.COMPLETED",
		"resource": {
			"id": %q,
			"billing_agreement_id": %q,
			"custom_id": %q,
			"amount": {"value": "9.99", "currency_code": "USD"},
			"billing_info": {"next_billing_time": %q}
		}
	}`, eventID, saleID, billingAgreementID, customID, nextBillingTime))
}
