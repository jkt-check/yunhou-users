package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/repo"
)

// Dashboard 运营 admin API e2e(dashboard-admin-api spec §7 验收矩阵)。
// 覆盖:401 鉴权、metrics 口径与 tz 校验、search 各匹配路径/截断/转义/
// 上限、detail 400/404/product 隔离、VIP 11 条规则矩阵(含幂等重放)与
// audit_log 落行。

// --- shared shapes ---

type adminEnvelope struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func adminGet(t *testing.T, engine *gin.Engine, path string, headers map[string]string) (int, adminEnvelope) {
	t.Helper()
	resp := doRequest(t, engine, http.MethodGet, path, "", headers)
	var env adminEnvelope
	resp.JSON(t, &env)
	return resp.StatusCode, env
}

// --- auth matrix ---

func TestE2E_AdminOpsAuthRequired(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	uid := "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11"
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/ops/metrics"},
		{http.MethodGet, "/admin/users/search?q=x"},
		{http.MethodGet, "/admin/users/" + uid},
		{http.MethodPost, "/admin/users/" + uid + "/vip"},
	} {
		resp := doRequest(t, engine, tc.method, tc.path, `{"days":30}`, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s: got %d, want 401 (no app credentials)", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// --- metrics ---

func TestE2E_AdminOpsMetrics(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	ctx := context.Background()
	auth := appAuthHeaders(superAppID)

	// Fixture: 2 普通用户(其中 1 个有支付成功 + active monthly 订阅)、
	// 1 个 deleted 用户、1 个有支付但订阅已过期的用户、1 笔 failed 支付
	// (不进任何口径)、一单两次支付尝试(不得重复计数)。
	mkUser := func(status string) string {
		var id string
		if err := db.QueryRowxContext(ctx,
			`INSERT INTO users (status) VALUES ($1) RETURNING id`, status).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	payer := mkUser("active")
	lapsed := mkUser("active")
	freebie := mkUser("active")
	mkUser("deleted")

	mkPaidOrder := func(userID string, amount float64, paidAt time.Time) {
		var orderID string
		if err := db.QueryRowxContext(ctx, `
			INSERT INTO orders (user_id, plan_id, amount, currency, status, expires_at)
			VALUES ($1, 'monthly', $2, 'CNY', 'paid', now() + interval '1 day')
			RETURNING id
		`, userID, amount).Scan(&orderID); err != nil {
			t.Fatalf("insert order: %v", err)
		}
		// 一次 failed 尝试 + 一次成功:直接 JOIN payments 会把金额翻倍,
		// 统一事实源(每 order 一行 max(paid_at))保证只计一次。
		if _, err := db.ExecContext(ctx, `
			INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, created_at)
			VALUES ($1, 'wechat_pay', $3, $2, 'CNY', 'failed', now())
		`, orderID, amount, "txn-failed-"+orderID); err != nil {
			t.Fatalf("insert failed payment: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO payments (order_id, channel, external_txn_id, amount, currency, status, paid_at)
			VALUES ($1, 'wechat_pay', $4, $2, 'CNY', 'paid', $3)
		`, orderID, amount, paidAt, "txn-paid-"+orderID); err != nil {
			t.Fatalf("insert paid payment: %v", err)
		}
	}
	mkPaidOrder(payer, 19.9, time.Now())
	mkPaidOrder(lapsed, 199.9, time.Now().Add(-40*24*time.Hour))

	// payer:active 付费订阅;trial 订阅不计入 paidUsers.active(price=0)。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'active', now(), now() + interval '30 days', 'kaya-membership')
	`, payer); err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'trial', 'active', now(), now() + interval '7 days', 'kaya-membership')
	`, freebie); err != nil {
		t.Fatalf("insert trial sub: %v", err)
	}

	code, env := adminGet(t, engine, "/admin/ops/metrics?tz=Asia/Shanghai", auth)
	if code != http.StatusOK || env.Code != 0 {
		t.Fatalf("metrics: code=%d env=%+v", code, env)
	}
	var data struct {
		Users     struct{ Total, Today, Week, Month int64 } `json:"users"`
		PaidUsers struct {
			Cumulative struct{ Total, Today, Week, Month int64 } `json:"cumulative"`
			Active     struct{ Total, Today, Week, Month int64 } `json:"active"`
		} `json:"paidUsers"`
		Revenue struct{ Total, Today, Week, Month float64 } `json:"revenue"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse metrics: %v", err)
	}

	// users:4 个非 deleted(本用例 3 active + 之前 seed 0;deleted 不计)。
	if data.Users.Total != 3 {
		t.Fatalf("users.total = %d, want 3", data.Users.Total)
	}
	if data.Users.Today != 3 || data.Users.Week != 3 || data.Users.Month != 3 {
		t.Fatalf("users buckets: %+v", data.Users)
	}
	// paidUsers.cumulative: payer + lapsed(40 天前的支付不计入 today/week/month)。
	if data.PaidUsers.Cumulative.Total != 2 || data.PaidUsers.Cumulative.Today != 1 {
		t.Fatalf("paidUsers.cumulative: %+v", data.PaidUsers.Cumulative)
	}
	// paidUsers.active: 仅 payer(trial price=0 排除;lapsed 无 active 订阅)。
	if data.PaidUsers.Active.Total != 1 || data.PaidUsers.Active.Today != 1 {
		t.Fatalf("paidUsers.active: %+v", data.PaidUsers.Active)
	}
	// revenue: 19.9 + 199.9;today 只算 payer 的 19.9。
	if data.Revenue.Total != 219.8 {
		t.Fatalf("revenue.total = %v, want 219.8", data.Revenue.Total)
	}
	if data.Revenue.Today != 19.9 {
		t.Fatalf("revenue.today = %v, want 19.9", data.Revenue.Today)
	}

	// tz 非法 → 400。
	code, env = adminGet(t, engine, "/admin/ops/metrics?tz=Mars/Olympus", auth)
	if code != http.StatusBadRequest || env.Code != 400 {
		t.Fatalf("bad tz: code=%d env=%+v", code, env)
	}
}

// --- search ---

func adminSeedSearchUser(t *testing.T, db *sqlx.DB, nickname, provider, providerUID, email string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := db.QueryRowxContext(ctx,
		`INSERT INTO users (nickname) VALUES ($1) RETURNING id`, nickname).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if provider != "" {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO social_identities (user_id, provider, provider_uid, email)
			VALUES ($1, $2, $3, $4)
		`, id, provider, providerUID, email); err != nil {
			t.Fatalf("insert identity: %v", err)
		}
	}
	return id
}

func adminSearchIDs(t *testing.T, engine *gin.Engine, q string) []string {
	t.Helper()
	code, env := adminGet(t, engine, "/admin/users/search?q="+url.QueryEscape(q), appAuthHeaders(superAppID))
	if code != http.StatusOK {
		t.Fatalf("search %q: code=%d msg=%s", q, code, env.Message)
	}
	var data struct {
		Users []struct {
			ID         string `json:"id"`
			Identities []struct {
				Provider    string  `json:"provider"`
				ProviderUID string  `json:"providerUid"`
				Email       *string `json:"email"`
			} `json:"identities"`
		} `json:"users"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse search: %v", err)
	}
	ids := make([]string, 0, len(data.Users))
	for _, u := range data.Users {
		ids = append(ids, u.ID)
	}
	return ids
}

func TestE2E_AdminUsersSearch(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	uid := adminSeedSearchUser(t, db, "searchable-nick-xyz", "wechat", "uid-findme-123", "findme@example.com")
	// 无 identity 的用户(LEFT JOIN 路径,identities 应为空数组)。
	lonely := adminSeedSearchUser(t, db, "lonely-nick-abc", "", "", "")
	// LIKE 特殊字符用户:% 必须按字面匹配。
	percent := adminSeedSearchUser(t, db, "100% sure", "", "", "")

	// 精确 ID 匹配。
	if ids := adminSearchIDs(t, engine, uid); len(ids) != 1 || ids[0] != uid {
		t.Fatalf("exact id search: %v", ids)
	}
	// 昵称模糊。
	if ids := adminSearchIDs(t, engine, "searchable-nick"); len(ids) != 1 || ids[0] != uid {
		t.Fatalf("nickname search: %v", ids)
	}
	// identity email 模糊。
	if ids := adminSearchIDs(t, engine, "findme@example"); len(ids) != 1 || ids[0] != uid {
		t.Fatalf("email search: %v", ids)
	}
	// identity provider_uid 模糊。
	if ids := adminSearchIDs(t, engine, "uid-findme"); len(ids) != 1 || ids[0] != uid {
		t.Fatalf("provider_uid search: %v", ids)
	}
	// 无 identity 用户可被昵称找到。
	if ids := adminSearchIDs(t, engine, "lonely-nick"); len(ids) != 1 || ids[0] != lonely {
		t.Fatalf("no-identity search: %v", ids)
	}
	// LIKE 转义:"100% " 按字面匹配,% 不作通配符。
	if ids := adminSearchIDs(t, engine, "100% s"); len(ids) != 1 || ids[0] != percent {
		t.Fatalf("like escape search: %v", ids)
	}
	// 空 q → 400。
	code, _ := adminGet(t, engine, "/admin/users/search?q=", appAuthHeaders(superAppID))
	if code != http.StatusBadRequest {
		t.Fatalf("empty q: got %d, want 400", code)
	}
	code, _ = adminGet(t, engine, "/admin/users/search?q=%20%20", appAuthHeaders(superAppID))
	if code != http.StatusBadRequest {
		t.Fatalf("blank q: got %d, want 400", code)
	}
}

func TestE2E_AdminUsersSearchTruncation(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	// 64 字符昵称;查询 = 昵称 + 超出 64 的尾巴,截断后仍应命中。
	nick := strings.Repeat("n", 64)
	uid := adminSeedSearchUser(t, db, nick, "", "", "")
	if ids := adminSearchIDs(t, engine, nick+"zzz"); len(ids) != 1 || ids[0] != uid {
		t.Fatalf("truncated search: %v", ids)
	}
}

func TestE2E_AdminUsersSearchCap(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	ctx := context.Background()

	// 25 个同前缀用户:行级 LIMIT 60 / 用户级上限 20。
	for i := 0; i < 25; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (nickname) VALUES ($1)`, fmt.Sprintf("capuser-%02d", i)); err != nil {
			t.Fatalf("insert cap user: %v", err)
		}
	}
	if ids := adminSearchIDs(t, engine, "capuser"); len(ids) != 20 {
		t.Fatalf("cap: got %d users, want 20", len(ids))
	}
}

// --- detail ---

func TestE2E_AdminUserDetail(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	ctx := context.Background()
	auth := appAuthHeaders(superAppID)

	uid := adminSeedSearchUser(t, db, "detail-nick", "github", "gh-detail-1", "detail@example.com")
	exp := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'active', now(), $2, 'kaya-membership')
	`, uid, exp); err != nil {
		t.Fatalf("insert membership sub: %v", err)
	}
	// 027 后同用户可有 coding-plan active 订阅;detail 绝不能串入。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, product_code)
		VALUES ('coding-monthly', 'Coding Plan Monthly', 49.9, 30, '{}', 'CNY', 'coding-plan')
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed coding plan: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'coding-monthly', 'active', now(), $2, 'coding-plan')
	`, uid, exp); err != nil {
		t.Fatalf("insert coding sub: %v", err)
	}

	// 400:非法 UUID。
	code, _ := adminGet(t, engine, "/admin/users/not-a-uuid", auth)
	if code != http.StatusBadRequest {
		t.Fatalf("bad uuid: got %d, want 400", code)
	}
	// 404:用户不存在。
	code, env := adminGet(t, engine, "/admin/users/3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", auth)
	if code != http.StatusNotFound || env.Code != 404 {
		t.Fatalf("missing user: code=%d env=%+v", code, env)
	}

	// 200:完整结构。
	code, env = adminGet(t, engine, "/admin/users/"+uid, auth)
	if code != http.StatusOK {
		t.Fatalf("detail: code=%d msg=%s", code, env.Message)
	}
	var data struct {
		User struct {
			ID       string  `json:"id"`
			Nickname *string `json:"nickname"`
			Status   string  `json:"status"`
		} `json:"user"`
		Identities []struct {
			Provider string `json:"provider"`
			Email    string `json:"email"`
		} `json:"identities"`
		ActiveSubscription *struct {
			PlanID       string   `json:"planId"`
			ExpiresAt    *string  `json:"expiresAt"`
			PlanIsActive *bool    `json:"planIsActive"`
			Price        *float64 `json:"price"`
		} `json:"activeSubscription"`
		History []struct {
			PlanID string `json:"planId"`
			Status string `json:"status"`
		} `json:"history"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse detail: %v", err)
	}
	if data.User.ID != uid || data.User.Nickname == nil || *data.User.Nickname != "detail-nick" {
		t.Fatalf("user: %+v", data.User)
	}
	if len(data.Identities) != 1 || data.Identities[0].Email != "detail@example.com" {
		t.Fatalf("identities: %+v", data.Identities)
	}
	if data.ActiveSubscription == nil || data.ActiveSubscription.PlanID != "monthly" {
		t.Fatalf("activeSubscription: %+v (coding-plan 订阅串入或缺失)", data.ActiveSubscription)
	}
	if data.ActiveSubscription.PlanIsActive == nil || !*data.ActiveSubscription.PlanIsActive {
		t.Fatalf("planIsActive: %+v", data.ActiveSubscription.PlanIsActive)
	}
	for _, h := range data.History {
		if h.PlanID == "coding-monthly" {
			t.Fatalf("history 混入 coding-plan: %+v", data.History)
		}
	}
}

// --- VIP ---

type adminVipData struct {
	Action string `json:"action"`
	PlanID string `json:"planId"`
	Before *struct {
		PlanID    string  `json:"planId"`
		ExpiresAt *string `json:"expiresAt"`
	} `json:"before"`
	After struct {
		PlanID    string  `json:"planId"`
		ExpiresAt *string `json:"expiresAt"`
	} `json:"after"`
}

func adminPostVip(t *testing.T, engine *gin.Engine, uid, body string, headers map[string]string) (int, adminEnvelope) {
	t.Helper()
	h := map[string]string{}
	for k, v := range appAuthHeaders(superAppID) {
		h[k] = v
	}
	for k, v := range headers {
		h[k] = v
	}
	resp := doRequest(t, engine, http.MethodPost, "/admin/users/"+uid+"/vip", body, h)
	var env adminEnvelope
	resp.JSON(t, &env)
	return resp.StatusCode, env
}

func adminVipUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	return adminSeedSearchUser(t, db, "vip-user", "", "", "")
}

func adminSeedSub(t *testing.T, db *sqlx.DB, uid, planID string, expiresAt *time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, $2, 'active', now(), $3, 'kaya-membership')
	`, uid, planID, expiresAt); err != nil {
		t.Fatalf("seed sub %s: %v", planID, err)
	}
}

func adminAuditCount(t *testing.T, db *sqlx.DB, action, uid string) int {
	t.Helper()
	var n int
	if err := db.QueryRowxContext(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target = $2`,
		action, "user:"+uid).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func adminSubExpiry(t *testing.T, db *sqlx.DB, uid string) time.Time {
	t.Helper()
	var exp time.Time
	if err := db.QueryRowxContext(context.Background(), `
		SELECT expires_at FROM subscriptions
		 WHERE user_id = $1 AND status = 'active' AND product_code = 'kaya-membership'
	`, uid).Scan(&exp); err != nil {
		t.Fatalf("read expiry: %v", err)
	}
	return exp
}

func TestE2E_AdminVipGrant(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)

	code, env := adminPostVip(t, engine, uid, `{"days":30}`, nil)
	if code != http.StatusOK || env.Code != 0 {
		t.Fatalf("grant: code=%d env=%+v", code, env)
	}
	var data adminVipData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if data.Action != "granted" || data.PlanID != "monthly" || data.Before != nil {
		t.Fatalf("grant data: %+v", data)
	}
	if data.After.ExpiresAt == nil {
		t.Fatal("after.expiresAt missing")
	}
	exp, err := time.Parse(time.RFC3339, *data.After.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expiresAt: %v", err)
	}
	// ≈ now+30d(容差 1 小时)。
	want := time.Now().Add(30 * 24 * time.Hour)
	if exp.Before(want.Add(-time.Hour)) || exp.After(want.Add(time.Hour)) {
		t.Fatalf("expiresAt %s, want ≈ %s", exp, want)
	}
	// DB 落行 + product_code 正确。DB 读回带微秒,响应按 spec §1.3 是
	// 秒级,按 Unix 秒比较。
	if got := adminSubExpiry(t, db, uid); got.Unix() != exp.Unix() {
		t.Fatalf("db expiry %s != response %s", got, exp)
	}
	if n := adminAuditCount(t, db, "vip.grant", uid); n != 1 {
		t.Fatalf("vip.grant audit rows = %d, want 1", n)
	}
}

func TestE2E_AdminVipExtend(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)
	old := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	adminSeedSub(t, db, uid, "monthly", &old)

	code, env := adminPostVip(t, engine, uid, `{"days":30}`, nil)
	if code != http.StatusOK {
		t.Fatalf("extend: code=%d msg=%s", code, env.Message)
	}
	var data adminVipData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if data.Action != "extended" || data.Before == nil || data.Before.ExpiresAt == nil {
		t.Fatalf("extend data: %+v", data)
	}
	// 未过期:从原到期时间累加(容差 1 分钟,GREATEST 语义下原值未过
	// now 时应精确为 old+30d)。
	newExp, err := time.Parse(time.RFC3339, *data.After.ExpiresAt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := old.Add(30 * 24 * time.Hour)
	if newExp.Before(want.Add(-time.Minute)) || newExp.After(want.Add(time.Minute)) {
		t.Fatalf("expiresAt %s, want ≈ %s", newExp, want)
	}
	if n := adminAuditCount(t, db, "vip.extend", uid); n != 1 {
		t.Fatalf("vip.extend audit rows = %d, want 1", n)
	}
}

func TestE2E_AdminVipExtendExpiredActiveRow(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)
	past := time.Now().Add(-5 * 24 * time.Hour).Truncate(time.Second)
	adminSeedSub(t, db, uid, "monthly", &past)

	code, env := adminPostVip(t, engine, uid, `{"days":30}`, nil)
	if code != http.StatusOK {
		t.Fatalf("code=%d msg=%s", code, env.Message)
	}
	var data adminVipData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 已过期:GREATEST(expires_at, now()) 语义 = 从 now 起算 30 天。
	newExp, _ := time.Parse(time.RFC3339, *data.After.ExpiresAt)
	want := time.Now().Add(30 * 24 * time.Hour)
	if newExp.Before(want.Add(-time.Hour)) || newExp.After(want.Add(time.Hour)) {
		t.Fatalf("expiresAt %s, want ≈ %s (已过期应从 now 起算)", newExp, want)
	}
}

func TestE2E_AdminVipLifetimeRejected(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)
	adminSeedSub(t, db, uid, "monthly", nil) // expires_at NULL = 终身 VIP

	code, env := adminPostVip(t, engine, uid, `{"days":30}`, nil)
	if code != http.StatusConflict || env.Code != 409 {
		t.Fatalf("code=%d env=%+v", code, env)
	}
	if !strings.Contains(env.Message, "终身 VIP") {
		t.Fatalf("message: %q", env.Message)
	}
	if n := adminAuditCount(t, db, "vip.reject", uid); n != 1 {
		t.Fatalf("vip.reject audit rows = %d, want 1", n)
	}
}

func TestE2E_AdminVipRetiredPlanRejected(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	ctx := context.Background()

	// quarterly:016 已退役(is_active=false);即使 is_active=true 也在
	// RETIRED_PLANS 名单。这里直接造 is_active=true 的 quarterly 钉住
	// 名单语义,再造一个 is_active=false 的普通套餐钉住退役语义。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, is_active)
		VALUES ('quarterly', '按季订阅', 49.9, 90, '{yundian}', 'CNY', true)
		ON CONFLICT (id) DO UPDATE SET is_active = true
	`); err != nil {
		t.Fatalf("seed quarterly: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, name, price, interval_days, apps, currency, is_active)
		VALUES ('old-yearly', '旧版年付', 99.9, 365, '{yundian}', 'CNY', false)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		t.Fatalf("seed old-yearly: %v", err)
	}

	for _, planID := range []string{"quarterly", "old-yearly"} {
		uid := adminVipUser(t, db)
		exp := time.Now().Add(10 * 24 * time.Hour)
		adminSeedSub(t, db, uid, planID, &exp)
		code, env := adminPostVip(t, engine, uid, `{"days":30}`, nil)
		if code != http.StatusConflict {
			t.Fatalf("plan %s: code=%d, want 409", planID, code)
		}
		if !strings.Contains(env.Message, "已退役套餐（"+planID+"）") {
			t.Fatalf("plan %s message: %q", planID, env.Message)
		}
	}
}

func TestE2E_AdminVipExtendTrial(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)
	trialExp := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	adminSeedSub(t, db, uid, "trial", &trialExp)

	code, env := adminPostVip(t, engine, uid, `{"days":3}`, nil)
	if code != http.StatusOK {
		t.Fatalf("trial extend: code=%d msg=%s", code, env.Message)
	}
	var data adminVipData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if data.Action != "extended" || data.PlanID != "trial" {
		t.Fatalf("trial extend data: %+v", data)
	}
}

func TestE2E_AdminVipValidation(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)

	// days 边界。
	for _, body := range []string{`{"days":0}`, `{"days":3651}`, `{"days":-5}`, `{"days":2.5}`, `{"days":30,"hack":1}`, `{}`} {
		code, _ := adminPostVip(t, engine, uid, body, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("body %s: got %d, want 400", body, code)
		}
	}
	// UUID 400 / 用户 404。
	if code, _ := adminPostVip(t, engine, "nope", `{"days":30}`, nil); code != http.StatusBadRequest {
		t.Fatalf("bad uuid: got %d", code)
	}
	if code, _ := adminPostVip(t, engine, "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", `{"days":30}`, nil); code != http.StatusNotFound {
		t.Fatalf("missing user: got %d", code)
	}
	// Idempotency-Key 超长 → 400。
	if code, _ := adminPostVip(t, engine, uid, `{"days":30}`,
		map[string]string{"Idempotency-Key": strings.Repeat("k", 129)}); code != http.StatusBadRequest {
		t.Fatalf("long idem key: got %d", code)
	}
	// days 边界值可接受。
	for _, body := range []string{`{"days":1}`, `{"days":3650}`} {
		u := adminVipUser(t, db)
		if code, env := adminPostVip(t, engine, u, body, nil); code != http.StatusOK {
			t.Fatalf("body %s: got %d msg=%s", body, code, env.Message)
		}
	}
}

func TestE2E_AdminVipIdempotentReplay(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)
	old := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	adminSeedSub(t, db, uid, "monthly", &old)

	headers := map[string]string{"Idempotency-Key": "dash-replay-1"}
	code, env1 := adminPostVip(t, engine, uid, `{"days":30}`, headers)
	if code != http.StatusOK {
		t.Fatalf("first: code=%d msg=%s", code, env1.Message)
	}
	code, env2 := adminPostVip(t, engine, uid, `{"days":30}`, headers)
	if code != http.StatusOK {
		t.Fatalf("replay: code=%d msg=%s", code, env2.Message)
	}
	// 重放原样返回首次 response。
	if string(env1.Data) != string(env2.Data) {
		t.Fatalf("replay mismatch:\nfirst:  %s\nsecond: %s", env1.Data, env2.Data)
	}
	// 只延长一次,不写第二条审计。
	var first adminVipData
	if err := json.Unmarshal(env1.Data, &first); err != nil {
		t.Fatalf("parse: %v", err)
	}
	exp, _ := time.Parse(time.RFC3339, *first.After.ExpiresAt)
	if got := adminSubExpiry(t, db, uid); got.Unix() != exp.Unix() {
		t.Fatalf("db expiry %s != first response %s(重放不得二次写入)", got, exp)
	}
	if n := adminAuditCount(t, db, "vip.extend", uid); n != 1 {
		t.Fatalf("vip.extend audit rows = %d, want 1(重放不得二次审计)", n)
	}
}

func TestE2E_AdminVipIdempotencyKeyScopedByApp(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	uid := adminVipUser(t, db)

	// 同一 key 不同 app:互不干扰,各自执行一次。
	code, _ := adminPostVip(t, engine, uid, `{"days":30}`, map[string]string{"Idempotency-Key": "shared-key"})
	if code != http.StatusOK {
		t.Fatalf("app1: %d", code)
	}
	headers2 := appAuthHeadersWithSecret("yundash", e2eAppSecret)
	headers2["Idempotency-Key"] = "shared-key"
	resp := doRequest(t, engine, http.MethodPost, "/admin/users/"+uid+"/vip", `{"days":30}`, headers2)
	var env adminEnvelope
	resp.JSON(t, &env)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app2: %d %s", resp.StatusCode, env.Message)
	}
	// 两个 app 各执行一次 → 共延长 60 天。
	exp := adminSubExpiry(t, db, uid)
	want := time.Now().Add(60 * 24 * time.Hour)
	if exp.Before(want.Add(-2*time.Hour)) || exp.After(want.Add(2*time.Hour)) {
		t.Fatalf("expiry %s, want ≈ %s", exp, want)
	}
}

func TestE2E_AdminVipCancelledRowUntouched(t *testing.T) {
	// 规则 1:只动 active 行或 INSERT 新行,绝不触碰 cancelled/expired 行。
	// 用户只有一条 cancelled 订阅 → 调 VIP 应 granted 插入新 monthly 行,
	// 旧 cancelled 行的 expires_at 字节不变。
	engine, _, db := setupE2EServer(t)
	ctx := context.Background()
	uid := adminVipUser(t, db)

	// PG timestamptz 为微秒精度,截断种子时间以便与 DB 读回值比较。
	cancelledExp := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'cancelled', now() - interval '20 days', $2, 'kaya-membership')
	`, uid, cancelledExp); err != nil {
		t.Fatalf("seed cancelled sub: %v", err)
	}

	code, env := adminPostVip(t, engine, uid, `{"days":30}`, nil)
	if code != http.StatusOK {
		t.Fatalf("grant over cancelled: code=%d msg=%s", code, env.Message)
	}
	var data adminVipData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if data.Action != "granted" || data.PlanID != "monthly" {
		t.Fatalf("data: %+v", data)
	}

	// 旧 cancelled 行的 expires_at 微秒级不变。
	var gotCancelled time.Time
	if err := db.QueryRowxContext(ctx, `
		SELECT expires_at FROM subscriptions
		 WHERE user_id = $1 AND status = 'cancelled' AND product_code = 'kaya-membership'
	`, uid).Scan(&gotCancelled); err != nil {
		t.Fatalf("read cancelled row: %v", err)
	}
	if gotCancelled.UnixMicro() != cancelledExp.UnixMicro() {
		t.Fatalf("cancelled row touched: expires_at %s, want %s", gotCancelled, cancelledExp)
	}
	// 新 active 行存在且到期时间在未来。
	if got := adminSubExpiry(t, db, uid); got.Before(time.Now()) {
		t.Fatalf("new active row expiry in the past: %s", got)
	}
}

func TestE2E_AdminExtendMembershipSubNoActiveRow(t *testing.T) {
	// 矩阵第 10 条的 repo 真库分支:ExtendMembershipSub 的 UPDATE 匹配 0 行
	// (预读的 active 行被并发取消)→ updated=false、无 error。HTTP 层无法
	// 确定性复现并发取消,直接驱动 repo 层。
	_, _, db := setupE2EServer(t)
	ctx := context.Background()
	r := repo.NewAdminUsersRepo(db)

	// 用户只有 cancelled 行。
	uid := adminVipUser(t, db)
	cancelledExp := time.Now().Add(-10 * 24 * time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at, product_code)
		VALUES ($1, 'monthly', 'cancelled', now() - interval '20 days', $2, 'kaya-membership')
	`, uid, cancelledExp); err != nil {
		t.Fatalf("seed cancelled sub: %v", err)
	}

	var updated bool
	err := r.WithTx(ctx, func(tx repo.AdminUsersTx) error {
		var err error
		updated, _, _, err = tx.ExtendMembershipSub(ctx, uid, 30)
		return err
	})
	if err != nil {
		t.Fatalf("extend cancelled-only user: %v", err)
	}
	if updated {
		t.Fatal("extend on cancelled-only user must report updated=false")
	}

	// 完全无订阅行的用户同样 updated=false。
	uid2 := adminVipUser(t, db)
	err = r.WithTx(ctx, func(tx repo.AdminUsersTx) error {
		var err error
		updated, _, _, err = tx.ExtendMembershipSub(ctx, uid2, 30)
		return err
	})
	if err != nil {
		t.Fatalf("extend no-sub user: %v", err)
	}
	if updated {
		t.Fatal("extend on no-sub user must report updated=false")
	}
}
