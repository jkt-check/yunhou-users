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
	_ "github.com/lib/pq"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
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

	// Seed super app (idempotent on app_id PK).
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active)
		VALUES ($1, 'E2E Test App', true)
		ON CONFLICT (app_id) DO NOTHING
	`, superAppID)
	if err != nil {
		t.Fatalf("seed super app: %v", err)
	}

	_, err = db.ExecContext(context.Background(), `
		INSERT INTO apps (app_id, name, is_active)
		VALUES ('yundash', 'E2E Paid App', true)
		ON CONFLICT (app_id) DO NOTHING
	`)
	if err != nil {
		t.Fatalf("seed paid app: %v", err)
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
		JWTAccessTTL:   15 * time.Minute,
		JWTRefreshTTL: 168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:     1 * time.Minute,
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
	paymentSvc := service.NewPaymentService(
		db,
		orderRepo, paymentRepo, refundRepo,
		subRepo, planRepo, userRepo,
		webhookEventRepo, auditLogRepo,
		&stubRefundAPI{},
		cfg.OrderExpiryDuration,
	)

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	router.Setup(context.Background(), engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, &middleware.MultiChannelVerifier{}, nil)

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
// payloads. Alipay's key pair is generated in-memory per test.
func setupE2EServerWithVerifier(t *testing.T) *E2EServer {
	t.Helper()

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
		JWTAccessTTL:       15 * time.Minute,
		JWTRefreshTTL:      168 * time.Hour,
		OrderExpiryDuration: 30 * time.Minute,
		SweeperInterval:     1 * time.Minute,
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
		cfg.OrderExpiryDuration,
	)

	// Build the multi-channel verifier with all three channels configured.
	// Alipay uses a generated RSA key pair — tests sign with the private key
	// and the verifier checks with the corresponding public key.
	alipayPriv, alipayPubPEM := genAlipayRSAKeyPair(t)
	mv := &middleware.MultiChannelVerifier{
		Stripe: &middleware.StripeVerifier{Secret: []byte(e2eStripeSecret)},
		WeChat: &middleware.WeChatPayV3Verifier{APIv3Key: []byte(e2eWeChatKey)},
		Alipay: &middleware.AlipayVerifier{PublicKey: mustParseAlipayPubKey(t, alipayPubPEM)},
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	router.Setup(context.Background(), engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, mv, []byte(e2eWeChatKey))

	// Stash the private key in a closure-accessible holder so signing helpers
	// can produce valid signatures. (We don't expose the priv directly to
	// avoid leaking it through the test struct unnecessarily.)
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

// loginAndGetToken performs a login and returns the access token + user id.
// Tokens are JWTs verified by the same JWKS in production — here the server
// signs with the e2e RSA key pair so tokens round-trip.
func loginAndGetToken(t *testing.T, engine *gin.Engine, token, appID string) (string, string) {
	t.Helper()
	body := fmt.Sprintf(`{"provider":"github","provider_token":%q,"app_id":%q}`, token, appID)
	resp := doRequest(t, engine, http.MethodPost, "/auth/login", body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d %s", resp.StatusCode, string(resp.Body))
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

func authHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// --- Webhook signing helpers (Stripe / WeChat / Alipay) ---

// signStripe produces a Stripe-Signature header for body at unix ts.
func signStripe(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

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
