// user_wallet_test.go — Task 14 客户钱包与运营调整端点测试（真实库 +
// 真实 JWT 中间件 + 运营双身份授权）。断言：派生余额=账本、套餐外开关
// 校验与审计、PAYG 开启需发布配置、调整幂等、越权/未授权结构拒绝。

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

// walletFixture mounts both surfaces over one store: the JWT /user group
// and the /admin group with the two identity legs stubbed by headers (the
// same pattern as admin_auth_test.go) + the real billing:adjust authz.
type walletFixture struct {
	db     *sqlx.DB
	store  *postgres.Store
	engine *gin.Engine
	views  *viewsFixture // reuses addUser/token plumbing
}

func newWalletFixture(t *testing.T) *walletFixture {
	t.Helper()
	v := newViewsFixture(t) // wipes + JWT user group with Task 11 handlers
	// Mount the wallet surface onto the same user group.
	store := v.store
	userWallet := httpapi.NewUserWalletHandler(store, nil)
	userGroup := v.engine.Group("/userw") // separate group, same JWT middleware
	userGroup.Use(walletJWT(t, v))
	userWallet.Register(userGroup)

	// Admin surface: header-stubbed identity legs + real billing:adjust authz.
	v.engine.Use()
	admin := v.engine.Group("/adminw", func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		if a := c.GetHeader("X-Test-App"); a != "" {
			c.Set(middleware.ContextApp, &model.App{AppID: a})
			c.Set(middleware.ContextAppID, a)
		}
		c.Next()
	}, httpapi.OperatorAuthz(store, management.PermBillingAdjust))
	httpapi.NewAdminAdjustmentsHandler(store, nil).Register(admin)
	return &walletFixture{db: v.db, store: store, engine: v.engine, views: v}
}

// walletJWT re-wraps the fixture token service as a middleware factory.
func walletJWT(t *testing.T, v *viewsFixture) gin.HandlerFunc {
	t.Helper()
	return middleware.JWTAuth(v.tokenSvc)
}

func (f *walletFixture) do(t *testing.T, method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// grantBillingAdmin makes userID an operator with the admin role
// (billing:adjust 含于 admin, management/operators.go).
func (f *walletFixture) grantBillingAdmin(t *testing.T, userID string) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO users (id) VALUES ($1) ON CONFLICT DO NOTHING`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GrantRole(context.Background(), userID, management.RoleAdmin, nil, "test"); err != nil {
		t.Fatalf("grant role: %v", err)
	}
}

// seedCustomerWallet funds the user's CNY wallet with cash + bonus.
func (f *walletFixture) seedCustomerWallet(t *testing.T, userID string, cash, bonus int64) {
	t.Helper()
	ctx := context.Background()
	acct, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cash > 0 {
		if err := f.store.CreditTopupTx(ctx, uow, postgres.WalletTopupCommand{
			AccountID: acct.ID, Currency: "CNY", AmountMicros: cash,
			PaymentID: "pay-" + uuid.NewString(), OrderID: uuid.NewString(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if bonus > 0 {
		if _, err := f.store.ApplyWalletAdjustmentTx(ctx, uow, postgres.WalletAdjustmentCommand{
			AccountID: acct.ID, Currency: "CNY",
			Source: accounting.WalletBonus, Direction: accounting.DirCredit,
			AmountMicros: bonus, Reason: "gift",
			OperatorSubject: "user:ops@app:test", IdempotencyKey: "gift-" + uuid.NewString(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestUserWallet_BalanceEqualsLedger: 客户展示 = 账本派生（裁决 3）；
// 跨客户读取在结构上不可能（B 看不到 A 的钱包）。
func TestUserWallet_BalanceEqualsLedger(t *testing.T) {
	f := newWalletFixture(t)
	userA, tokA := f.views.addUser(t)
	userB, tokB := f.views.addUser(t)
	f.seedCustomerWallet(t, userA, 50_000_000, 10_000_000)

	w := f.do(t, http.MethodGet, "/userw/wallet", tokA, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET wallet: %d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	wallets, _ := data["wallets"].([]any)
	if len(wallets) != 1 {
		t.Fatalf("wallets = %v", data["wallets"])
	}
	w0 := wallets[0].(map[string]any)
	if w0["cash_available_micros"] != "50000000" || w0["bonus_available_micros"] != "10000000" {
		t.Fatalf("wallet view = %v", w0)
	}
	if w0["overage_enabled"] != false {
		t.Fatalf("overage must default off: %v", w0)
	}
	payg, _ := data["payg"].(map[string]any)
	if payg["enabled"] != false {
		t.Fatalf("payg default off: %v", payg)
	}

	// B（无钱包）→ 零状态，且响应不含 A 的任何标识。
	w = f.do(t, http.MethodGet, "/userw/wallet", tokB, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET wallet B: %d", w.Code)
	}
	dataB := decodeData(t, w)
	if wb, _ := dataB["wallets"].([]any); len(wb) != 0 {
		t.Fatalf("B must see zero state, got %v", wb)
	}
	if bytes.Contains(w.Body.Bytes(), []byte(userA)) {
		t.Fatal("B's response must not contain A's identity")
	}
	_ = userB
}

// TestUserWallet_OverageOptIn: 开启必须带上限（400）；开启/改限/关闭全
// 部写审计；未开启时余额不经 wallet 路径动用（网关级见 gateway 包）。
func TestUserWallet_OverageOptIn(t *testing.T) {
	f := newWalletFixture(t)
	userID, tok := f.views.addUser(t)
	f.seedCustomerWallet(t, userID, 5_000_000, 0)

	// 开启不带上限 → 400。
	w := f.do(t, http.MethodPut, "/userw/wallet/overage", tok, map[string]any{
		"currency": "CNY", "enabled": true,
	}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("enable without limit: %d %s", w.Code, w.Body.String())
	}
	// 开启 + 上限 → 200，视图回读 enabled/limit。
	w = f.do(t, http.MethodPut, "/userw/wallet/overage", tok, map[string]any{
		"currency": "CNY", "enabled": true, "monthly_spend_limit_micros": "7000000",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	if data["overage_enabled"] != true || data["monthly_spend_limit_micros"] != "7000000" {
		t.Fatalf("view = %v", data)
	}
	// 审计行：enable + set_limit。
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM inference_wallet_audits a
		JOIN inference_wallets wl ON wl.id = a.wallet_id
		JOIN inference_billing_accounts ba ON ba.id = wl.billing_account_id
		WHERE ba.user_id = $1 AND a.action IN ('enable_overage','set_spend_limit')`, userID); err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("audit rows = %d, want >= 2", n)
	}
	// 关闭 → disable 审计。
	w = f.do(t, http.MethodPut, "/userw/wallet/overage", tok, map[string]any{
		"currency": "CNY", "enabled": false, "monthly_spend_limit_micros": "7000000",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM inference_wallet_audits a
		JOIN inference_wallets wl ON wl.id = a.wallet_id
		JOIN inference_billing_accounts ba ON ba.id = wl.billing_account_id
		WHERE ba.user_id = $1 AND a.action = 'disable_overage'`, userID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("disable audit = %d", n)
	}
}

// TestUserWallet_PaygEnable: 未发布配置 → 404；发布后开启 → 显式权益记
// 录；重复开启幂等。
func TestUserWallet_PaygEnable(t *testing.T) {
	f := newWalletFixture(t)
	userID, tok := f.views.addUser(t)

	w := f.do(t, http.MethodPost, "/userw/wallet/payg", tok, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unconfigured payg: %d %s", w.Code, w.Body.String())
	}

	// 运营发布 PAYG 配置（published 策略 + 显式模型集合）。
	pol := &postgres.PolicyVersion{
		Name: "payg-default", Revision: 1, ModelIDs: []string{"glm-4.6"}, Status: "published",
	}
	if err := f.store.InsertPolicyVersion(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PutPAYGConfig(context.Background(), pol.ID, []string{"glm-4.6"}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}

	w = f.do(t, http.MethodPost, "/userw/wallet/payg", tok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enable payg: %d %s", w.Code, w.Body.String())
	}
	first := decodeData(t, w)
	if first["enabled"] != true || first["entitlement_id"] == "" {
		t.Fatalf("payg = %v", first)
	}
	// 重复开启 → 同一记录。
	w = f.do(t, http.MethodPost, "/userw/wallet/payg", tok, nil, nil)
	second := decodeData(t, w)
	if second["entitlement_id"] != first["entitlement_id"] {
		t.Fatalf("re-enable must be idempotent: %v vs %v", first, second)
	}
	// GET 视图反映 PAYG 已开启。
	w = f.do(t, http.MethodGet, "/userw/wallet", tok, nil, nil)
	data := decodeData(t, w)
	payg, _ := data["payg"].(map[string]any)
	if payg["enabled"] != true || payg["entitlement_id"] != first["entitlement_id"] {
		t.Fatalf("payg view = %v", payg)
	}
	_ = userID
}

// TestAdminWallet_AdjustIdempotent: 运营赠送入账 + 幂等键重放只生效一次；
// 无 billing:adjust 权限 → 403。
func TestAdminWallet_AdjustIdempotent(t *testing.T) {
	f := newWalletFixture(t)
	customerID, _ := f.views.addUser(t)
	adminID := uuid.NewString()
	f.grantBillingAdmin(t, adminID)
	headers := map[string]string{"X-Test-User": adminID, "X-Test-App": "ops-console"}

	body := map[string]any{
		"user_id": customerID, "currency": "CNY", "source": "bonus", "direction": "credit",
		"amount_micros": "2000000", "reason": "compensation", "idempotency_key": "ops-" + uuid.NewString(),
	}
	w := f.do(t, http.MethodPost, "/adminw/wallet/adjustments", "", body, headers)
	if w.Code != http.StatusCreated {
		t.Fatalf("adjust: %d %s", w.Code, w.Body.String())
	}
	first := decodeData(t, w)
	if first["applied"] != true {
		t.Fatalf("first apply = %v", first)
	}
	// 幂等键重放 → applied=false，同一 adjustment。
	w = f.do(t, http.MethodPost, "/adminw/wallet/adjustments", "", body, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	second := decodeData(t, w)
	if second["applied"] != false ||
		second["adjustment"].(map[string]any)["id"] != first["adjustment"].(map[string]any)["id"] {
		t.Fatalf("replay = %v, want applied=false same id", second)
	}
	// 客户视图可见赠送余额（账本派生一致）。
	acct, err := f.store.GetBillingAccountByUser(context.Background(), customerID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.store.WalletBalance(context.Background(), acct.ID, "CNY", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if view.Balance.BonusAvailable != 2_000_000 {
		t.Fatalf("bonus = %d, want 2 CNY once", view.Balance.BonusAvailable)
	}

	// 无权限操作者 → 403（授权中间件 fail-closed）。
	plainID := uuid.NewString()
	if _, err := f.db.Exec(`INSERT INTO users (id) VALUES ($1)`, plainID); err != nil {
		t.Fatal(err)
	}
	w = f.do(t, http.MethodPost, "/adminw/wallet/adjustments", "", body,
		map[string]string{"X-Test-User": plainID, "X-Test-App": "ops-console"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("unauthorized adjust: %d %s", w.Code, w.Body.String())
	}
	// 缺幂等键 → 400。
	w = f.do(t, http.MethodPost, "/adminw/wallet/adjustments", "", map[string]any{
		"user_id": customerID, "currency": "CNY", "source": "cash", "direction": "debit",
		"amount_micros": "1", "reason": "x",
	}, headers)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key: %d", w.Code)
	}
}

// TestUserWalletEntries_Pagination: 流水按 id 倒序 keyset 分页。
func TestUserWalletEntries_Pagination(t *testing.T) {
	f := newWalletFixture(t)
	userID, tok := f.views.addUser(t)
	f.seedCustomerWallet(t, userID, 3_000_000, 2_000_000)

	w := f.do(t, http.MethodGet, "/userw/wallet/entries?currency=CNY&limit=1", tok, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("entries: %d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	entries, _ := data["entries"].([]any)
	if len(entries) != 1 || data["next_cursor"] == nil {
		t.Fatalf("page 1 = %v", data)
	}
	cursor := data["next_cursor"].(string)
	w = f.do(t, http.MethodGet, "/userw/wallet/entries?currency=CNY&limit=50&cursor="+cursor, tok, nil, nil)
	data = decodeData(t, w)
	rest, _ := data["entries"].([]any)
	if len(rest) != 1 { // 共两条（topup + bonus）
		t.Fatalf("page 2 = %v", data)
	}
	// 缺币种 → 400。
	w = f.do(t, http.MethodGet, "/userw/wallet/entries", tok, nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing currency: %d", w.Code)
	}
	// 非法游标 → 400。
	w = f.do(t, http.MethodGet, "/userw/wallet/entries?currency=CNY&cursor=abc", tok, nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: %d", w.Code)
	}
}
