package e2e

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/billing/wechat"
	"github.com/yunhou/users/internal/config"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/router"
	"github.com/yunhou/users/internal/service"
)

// setupE2EServerWithMockWeChat mirrors setupE2EServerWithMockWeChatPay but
// additionally turns on the WeChat OAUTH mock branch (router.Setup
// wechatOAuthMock=true). The pay-only helper kept wechatOAuthMock=false
// because the existing webhook tests bypass OAuth via /test/login. The
// tests in this file exercise the *real* /auth/wechat/* path under mock
// mode, which is the only way to verify the cn-staging end-to-end story
// without a WeChat Open Platform sandbox account.
//
// Both mocks wire in lockstep (router.Setup + verifier + handler) so
// there is no way to get them out of sync — same discipline as
// setupE2EServerWithMockWeChatPay.
func setupE2EServerWithMockWeChat(t *testing.T) *E2EServer {
	t.Helper()

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
	// Both mock flags true — see setupE2EServerWithMockWeChat docstring.
	mv := &middleware.MultiChannelVerifier{
		Stripe: &middleware.StripeVerifier{Secret: []byte(e2eStripeSecret)},
		WeChat: &middleware.WeChatPayV3Verifier{APIv3Key: []byte(e2eWeChatKey), MockMode: true},
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
	chatSvc := service.NewChatService("", "", "", subRepo, planRepo) // chat disabled in this e2e helper
	githubOAuthSvc := service.NewGitHubOAuthService(cfg.OAuthStateSecret)
	wechatOAuthSvc := service.NewWeChatOAuthService(cfg.OAuthStateSecret)
	setupCtx, cancelSetup := context.WithCancel(context.Background())
	t.Cleanup(cancelSetup)
	// Last two args: wechatOAuthMock=true, wechatPayMock=true.
	router.Setup(setupCtx, engine, db,
		appRepo, userRepo, identityRepo, planRepo, subRepo, sessionRepo,
		tokenSvc, authSvc, subSvc, planSvc,
		paymentSvc, mv, []byte(e2eWeChatKey),
		providerTokenSvc, quoteSvc, chatSvc, nil, githubOAuthSvc, wechatOAuthSvc, true, true)

	alipayPrivHolder.Store(alipayPriv)

	return &E2EServer{
		Engine:          engine,
		DB:              db,
		StripeSecret:    e2eStripeSecret,
		WeChatKey:       []byte(e2eWeChatKey),
		AlipayPublicPEM: alipayPubPEM,
	}
}

// TestWeChat_OAuth_MockMode_FullRoundTrip exercises the real
// /auth/wechat/* path under WECHAT_OAUTH_MOCK=1. The browser-style
// round-trip is:
//
//  1. GET /auth/wechat/redirect?app_id=...&redirect_uri=...
//     → 302 to redirect_uri with ?code=mock-code&state=<hmac> in the
//     QUERY (mirrors real WeChat, which always sends code+state in the
//     query; changed from fragment on 2026-07-22 so the SPA's
//     AuthCallbackPage handles mock and real identically)
//  2. Extract state from the query, then
//     GET /auth/wechat/callback?app_id=...&code=mock-code&state=<hmac>
//     → 302 to redirect_uri with #token=...&refresh_token=...
//  3. With the access token, GET /user/profile
//     → 200 with {user: {provider:"wechat", provider_uid:"wechat_mock-unionid-001"}, subscription:...}
//
// This is the only test in the repo that exercises /auth/wechat/redirect
// and /auth/wechat/callback together. The handler-level unit tests cover
// each endpoint in isolation; the e2e webhook tests bypass OAuth via
// /test/login. This test fills the gap that cn-staging is allowed to
// pretend to be: a browser walking the full WeChat-OAuth path against
// Yunhou with the upstream WeChat server replaced by deterministic mocks.
func TestWeChat_OAuth_MockMode_FullRoundTrip(t *testing.T) {
	srv := setupE2EServerWithMockWeChat(t)

	redirectURI := "https://staging.yunhouai.com/auth/callback"

	// 1. /auth/wechat/redirect — expect 302 to redirect_uri with
	//    ?code=mock-code&state=<hmac> in the QUERY (same contract as real
	//    WeChat's 302 to redirect_uri).
	step1 := doRequest(t, srv.Engine, http.MethodGet,
		"/auth/wechat/redirect?app_id=yundian&redirect_uri="+url.QueryEscape(redirectURI),
		"", nil)
	if step1.StatusCode != http.StatusFound {
		t.Fatalf("step1 redirect: status=%d body=%s", step1.StatusCode, string(step1.Body))
	}
	loc := step1.Headers.Get("Location")
	if !strings.HasPrefix(loc, redirectURI+"?") {
		t.Fatalf("step1 Location %q does not start with redirect_uri? (query expected)", loc)
	}
	if !strings.Contains(loc, "code=mock-code") {
		t.Fatalf("step1 Location %q missing code=mock-code", loc)
	}
	locParsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("step1 Location %q unparseable: %v", loc, err)
	}
	state := locParsed.Query().Get("state")
	if state == "" {
		t.Fatalf("step1 Location %q missing state", loc)
	}

	// 2. /auth/wechat/callback — pass the state back, expect 302 with
	//    #token=...&refresh_token=...&user_id=...&has_access=... in the
	//    fragment. The fragment contract is defined in auth_common.go
	//    (BuildAuthFragment + redirectWithFragment) and intentionally
	//    uses "token" not "access_token" — see auth_common.go:31.
	step2 := doRequest(t, srv.Engine, http.MethodGet,
		"/auth/wechat/callback?app_id=yundian&code=mock-code&state="+url.QueryEscape(state),
		"", nil)
	if step2.StatusCode != http.StatusFound {
		t.Fatalf("step2 callback: status=%d body=%s", step2.StatusCode, string(step2.Body))
	}
	loc2 := step2.Headers.Get("Location")
	accessToken := extractFragmentValue(t, loc2, "token")
	refreshToken := extractFragmentValue(t, loc2, "refresh_token")
	hasAccess := extractFragmentValue(t, loc2, "has_access")
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("step2 Location %q missing token or refresh_token", loc2)
	}
	if hasAccess != "true" {
		t.Errorf("step2 has_access=%q, want true (mock should mint a fresh user)", hasAccess)
	}

	// 3. /user/identities with the access token. Expect 200 with the
	//    wechat identity row carrying provider="wechat" and the
	//    deterministic mock unionid. (Yunhou's user model itself has no
	//    provider/provider_uid columns — those live on the social_identities
	//    table exposed via /user/identities. See router.go:80.)
	step3 := doRequest(t, srv.Engine, http.MethodGet, "/user/identities", "",
		map[string]string{"Authorization": "Bearer " + accessToken})
	if step3.StatusCode != http.StatusOK {
		t.Fatalf("step3 /user/identities: status=%d body=%s", step3.StatusCode, string(step3.Body))
	}
	var identities struct {
		Code float64 `json:"code"`
		Data []struct {
			Provider    string `json:"provider"`
			ProviderUID string `json:"provider_uid"`
		} `json:"data"`
	}
	step3.JSON(t, &identities)
	if identities.Code != 0 {
		t.Fatalf("step3 /user/identities: code=%v (expected 0)", identities.Code)
	}
	if len(identities.Data) != 1 {
		t.Fatalf("step3 /user/identities: expected 1 identity, got %d", len(identities.Data))
	}
	if identities.Data[0].Provider != "wechat" {
		t.Errorf("step3 identity.provider=%q, want wechat", identities.Data[0].Provider)
	}
	if identities.Data[0].ProviderUID != "wechat_mock-unionid-001" {
		t.Errorf("step3 identity.provider_uid=%q, want wechat_mock-unionid-001", identities.Data[0].ProviderUID)
	}

	// 4. Confirm the social_identities row landed (so a second login
	//    against the same unionid short-circuits to the existing user
	//    instead of minting a new one).
	var socialCount int
	if err := srv.DB.GetContext(context.Background(), &socialCount,
		`SELECT COUNT(*) FROM social_identities
		 WHERE provider = 'wechat' AND provider_uid = 'wechat_mock-unionid-001'`); err != nil {
		t.Fatalf("query social_identities: %v", err)
	}
	if socialCount != 1 {
		t.Errorf("expected exactly 1 social_identity row, got %d", socialCount)
	}
}

// TestWeChat_Pay_MockMode_FullFlow_LoginBuySubscribe walks the entire
// WeChat mock story end-to-end:
//
//	OAuth round-trip  →  POST /payments/orders  →  POST webhook mock body
//	→  GET /user/subscriptions shows the active subscription.
//
// The existing TestWebhook_WeChat_MockMode_OrderPaid_SubscriptionActivated
// in webhooks_test.go covers the order + webhook + subscription but
// bypasses OAuth via /test/login. This test exercises the complete flow
// including OAuth, which is the path cn-staging will actually take in
// production (just with WECHAT_*_MOCK=1). It also pins that
// /user/subscriptions surfaces the subscription after the webhook fires —
// the FE polls /user/subscriptions on /console mount, so this is the
// user-visible signal that "payment activated".
func TestWeChat_Pay_MockMode_FullFlow_LoginBuySubscribe(t *testing.T) {
	srv := setupE2EServerWithMockWeChat(t)

	redirectURI := "https://staging.yunhouai.com/auth/callback"

	// OAuth round-trip.
	step1 := doRequest(t, srv.Engine, http.MethodGet,
		"/auth/wechat/redirect?app_id=yundian&redirect_uri="+url.QueryEscape(redirectURI),
		"", nil)
	if step1.StatusCode != http.StatusFound {
		t.Fatalf("oauth redirect: status=%d", step1.StatusCode)
	}
	// Mock redirect now carries code+state in the QUERY (mirrors real
	// WeChat), not the fragment — see TestWeChat_OAuth_MockMode_FullRoundTrip.
	step1URL, err := url.Parse(step1.Headers.Get("Location"))
	if err != nil {
		t.Fatalf("oauth redirect Location unparseable: %v", err)
	}
	state := step1URL.Query().Get("state")
	step2 := doRequest(t, srv.Engine, http.MethodGet,
		"/auth/wechat/callback?app_id=yundian&code=mock-code&state="+url.QueryEscape(state),
		"", nil)
	if step2.StatusCode != http.StatusFound {
		t.Fatalf("oauth callback: status=%d", step2.StatusCode)
	}
	accessToken := extractFragmentValue(t, step2.Headers.Get("Location"), "token")
	if accessToken == "" {
		t.Fatalf("oauth callback fragment missing token")
	}

	// Create a wechat_pay order on the monthly plan. handler/payment.go:71
	// returns 201 Created for new orders (not 200) — adjust accordingly.
	create := doRequest(t, srv.Engine, http.MethodPost, "/payments/orders",
		`{"plan_id":"monthly","channel":"wechat_pay"}`,
		authHeader(accessToken))
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create order: status=%d body=%s", create.StatusCode, string(create.Body))
	}
	var created struct {
		Data struct {
			ID             string `json:"id"`
			ProviderIntent struct {
				CodeURL    string `json:"code_url"`
				OutTradeNo string `json:"out_trade_no"`
			} `json:"provider_intent"`
		} `json:"data"`
	}
	create.JSON(t, &created)
	orderID := created.Data.ID
	outTradeNo := created.Data.ProviderIntent.OutTradeNo
	if !strings.HasPrefix(created.Data.ProviderIntent.CodeURL, "weixin://wxpay/bizpayurl?pr=mock_") {
		t.Errorf("provider_intent.code_url=%q, want wechat-mock prefix", created.Data.ProviderIntent.CodeURL)
	}
	if len(outTradeNo) != 32 {
		t.Fatalf("expected 32-char out_trade_no, got %q (len=%d)", outTradeNo, len(outTradeNo))
	}

	// Fire the mock webhook (plaintext body, no signature — MockMode on the
	// verifier is the production fix that ships in this commit). out_trade_no
	// echoes the 32-char hex from provider_intent — same shape as a real
	// WeChat callback. A real WeChat echo would land on the JSONB fallback
	// in onPaymentSucceeded; an in-test UUID echo would also pass (id lookup
	// hits first), but using the 32-char form exercises the real code path.
	webhookBody := []byte(`{
		"id":"evt_fullflow_` + outTradeNo + `",
		"event_type":"TRANSACTION.SUCCESS",
		"resource":{
			"transaction_id":"wx_fullflow_` + outTradeNo + `",
			"out_trade_no":"` + outTradeNo + `",
			"amount":{"total":2990},
			"sub_expires_at":"2030-01-01T00:00:00Z"
		}
	}`)
	ts := time.Now().Unix()
	wh := doRequest(t, srv.Engine, http.MethodPost, "/webhooks/payment/wechat_pay",
		string(webhookBody),
		map[string]string{
			"Wechatpay-Signature": "mock-bypass-not-validated",
			"Wechatpay-Timestamp": strconv.FormatInt(ts, 10),
			"Wechatpay-Nonce":     "fullflowmock",
			"Content-Type":        "application/json",
		})
	if wh.StatusCode != http.StatusOK {
		t.Fatalf("webhook: status=%d body=%s", wh.StatusCode, string(wh.Body))
	}

	// Order should be paid.
	var status string
	if err := srv.DB.GetContext(context.Background(), &status,
		`SELECT status FROM orders WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	if status != "paid" {
		t.Errorf("orders.status=%q, want paid", status)
	}

	// /user/subscriptions should now show the active subscription — this
	// is the signal the FE polls on /console to flip the UI to
	// "subscribed". (Same auth flow; identity is the wechat user minted
	// in the OAuth round-trip above.)
	me := doRequest(t, srv.Engine, http.MethodGet, "/user/subscriptions", "",
		map[string]string{"Authorization": "Bearer " + accessToken})
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/user/subscriptions after webhook: status=%d body=%s", me.StatusCode, string(me.Body))
	}
	var meBody struct {
		Data []struct {
			PlanID string `json:"plan_id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	me.JSON(t, &meBody)
	if len(meBody.Data) != 1 {
		t.Fatalf("/user/subscriptions after webhook: expected 1 subscription, got %d", len(meBody.Data))
	}
	if meBody.Data[0].PlanID != "monthly" {
		t.Errorf("subscription.plan_id=%q, want monthly", meBody.Data[0].PlanID)
	}
	if meBody.Data[0].Status != "active" {
		t.Errorf("subscription.status=%q, want active", meBody.Data[0].Status)
	}
}

// extractFragmentValue parses "scheme://path#key1=v1&key2=v2" and
// returns the value for key. Returns "" if the key is not present.
// Unlike url.URL.Fragment, this is order-agnostic and url-decodes.
func extractFragmentValue(t *testing.T, rawURL, key string) string {
	t.Helper()
	idx := strings.Index(rawURL, "#")
	if idx < 0 {
		return ""
	}
	fragment := rawURL[idx+1:]
	for _, kv := range strings.Split(fragment, "&") {
		eq := strings.Index(kv, "=")
		if eq < 0 {
			continue
		}
		k, _ := url.QueryUnescape(kv[:eq])
		if k != key {
			continue
		}
		v, _ := url.QueryUnescape(kv[eq+1:])
		return v
	}
	return ""
}
