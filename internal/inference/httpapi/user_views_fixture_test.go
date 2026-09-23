package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/quota"
)

// user_views_fixture_test.go — 七态响应 fixture（任务书：零额度、未激活、
// 耗尽、已过期、预占中、跨月、待核对）。每个 fixture 文件是
// docs/api/fixtures/ 下的真实示例响应（供 Website 对接）；测试把活响应与
// fixture 同时归一化（UUID/RFC3339 → 占位符）后深度比较 —— 形状、枚举值、
// 十进制字符串数字逐字段钉死；时间语义（如跨月裁剪）由逐条语义断言钉死。
//
// 重新生成：UPDATE_FIXTURES=1 go test ./internal/inference/httpapi/ -run Fixture

var (
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	tsRE   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
)

// normalizeVolatile replaces UUIDs and RFC3339 timestamps with placeholders
// so fixtures pin the SHAPE and the decimal-string arithmetic, not wall time.
func normalizeVolatile(s string) string {
	s = tsRE.ReplaceAllString(s, "<ts>")
	return uuidRE.ReplaceAllString(s, "<uuid>")
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "..", "docs", "api", "fixtures", name)
}

// assertFixture compares the live response body against the fixture file
// (both normalized); UPDATE_FIXTURES=1 regenerates the file verbatim.
func assertFixture(t *testing.T, name, body string) {
	t.Helper()
	path := fixturePath(name)
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		var pretty any
		if err := json.Unmarshal([]byte(body), &pretty); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		raw, err := json.MarshalIndent(pretty, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("regenerated %s", path)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture %s missing (run with UPDATE_FIXTURES=1 to create): %v", name, err)
	}
	var got, want any
	if err := json.Unmarshal([]byte(normalizeVolatile(body)), &got); err != nil {
		t.Fatalf("live response not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(normalizeVolatile(string(raw))), &want); err != nil {
		t.Fatalf("fixture %s not JSON: %v", name, err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("fixture %s mismatch:\n--- live ---\n%s\n--- fixture ---\n%s", name, g, w)
	}
}

// seedQuotaOnly seeds account + policy + entitlement WITHOUT any window row
// （从未消费）. Returns anchor for semantic assertions.
func (f *viewsFixture) seedQuotaOnly(t *testing.T, userID string, fiveHour, weekly, monthly int64, anchor time.Time, effectiveTo *time.Time, status domain.EntitlementStatus) (entID, policyID string) {
	t.Helper()
	ctx := context.Background()
	acct, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertModel(ctx, &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM 4.6", ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatal(err)
	}
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{"glm-4.6"},
		FiveHourLimit: mustMicro(fiveHour), WeeklyLimit: mustMicro(weekly),
		MonthlyLimit: mustMicro(monthly), Status: "published",
	}
	if err := f.store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{"glm-4.6"},
		PolicyVersionID: pol.ID, AnchorAt: anchor, EffectiveFrom: anchor,
		EffectiveTo: effectiveTo, Status: status,
	}
	if err := f.store.InsertEntitlement(ctx, ent); err != nil {
		t.Fatal(err)
	}
	return ent.ID, pol.ID
}

func windowsOf(t *testing.T, data map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, w := range data["windows"].([]any) {
		m := w.(map[string]any)
		out[m["kind"].(string)] = m
	}
	return out
}

func blocksOf(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, b := range data["blocked_by"].([]any) {
		out = append(out, b.(map[string]any))
	}
	return out
}

func TestFixture_ModelQuotas_ZeroLimit(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	f.seedQuotaOnly(t, userA, 0, 0, 0, time.Now().UTC().Add(-48*time.Hour), nil, domain.EntitlementActive)

	w := f.get(t, "/user/model-quotas", tokA)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	wins := windowsOf(t, data)
	// 零额度：三窗口 remaining 全 0，blocked_by 三个 quota_exhausted；
	// 五小时未激活（无边界 + activation 提示 + 恢复时刻未知 = null）。
	for _, kind := range []string{"five_hour", "weekly", "monthly"} {
		if wins[kind]["remaining"] != "0" || wins[kind]["limit"] != "0" {
			t.Errorf("%s window = %v", kind, wins[kind])
		}
	}
	if wins["five_hour"]["window_start"] != nil || wins["five_hour"]["activation"] != "on_first_consumption" {
		t.Errorf("five_hour unactivated shape = %v", wins["five_hour"])
	}
	blocks := blocksOf(t, data)
	if len(blocks) != 3 {
		t.Fatalf("blocked_by = %v, want 3", blocks)
	}
	for i, kind := range []string{"five_hour", "weekly", "monthly"} {
		if blocks[i]["kind"] != kind || blocks[i]["reason"] != "quota_exhausted" {
			t.Errorf("block %d = %v", i, blocks[i])
		}
	}
	if blocks[0]["resets_at"] != nil {
		t.Error("unactivated zero-limit five_hour must not invent resets_at")
	}
	if blocks[1]["resets_at"] == nil || blocks[2]["resets_at"] == nil {
		t.Error("weekly/monthly blocks must carry the period end")
	}
	assertFixture(t, "model-quotas-zero-limit.json", w.Body.String())
}

func TestFixture_ModelQuotas_Unactivated(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	f.seedQuotaOnly(t, userA, 1_000_000, 10_000_000, 100_000_000,
		time.Now().UTC().Add(-48*time.Hour), nil, domain.EntitlementActive)

	w := f.get(t, "/user/model-quotas", tokA)
	data := decodeData(t, w)
	if len(blocksOf(t, data)) != 0 {
		t.Errorf("blocked_by = %v, want empty", data["blocked_by"])
	}
	wins := windowsOf(t, data)
	fh := wins["five_hour"]
	if fh["window_start"] != nil || fh["resets_at"] != nil || fh["activation"] != "on_first_consumption" {
		t.Errorf("five_hour = %v", fh)
	}
	if fh["remaining"] != "1000000" || fh["mode"] != "anchored_duration" {
		t.Errorf("five_hour remaining/mode = %v", fh)
	}
	// 前端无需计算窗口：周/月边界由服务端给出。
	for _, kind := range []string{"weekly", "monthly"} {
		if wins[kind]["window_start"] == nil || wins[kind]["resets_at"] == nil {
			t.Errorf("%s bounds missing: %v", kind, wins[kind])
		}
	}
	if wins["weekly"]["mode"] != "anchored_period_7d" || wins["monthly"]["mode"] != "anchored_calendar_month" {
		t.Errorf("modes = %v / %v", wins["weekly"]["mode"], wins["monthly"]["mode"])
	}
	assertFixture(t, "model-quotas-unactivated.json", w.Body.String())
}

func TestFixture_ModelQuotas_Exhausted(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	ctx := context.Background()
	anchor := time.Now().UTC().Add(-48 * time.Hour)
	entID, _ := f.seedQuotaOnly(t, userA, 100, 500, 9000, anchor, nil, domain.EntitlementActive)
	now := time.Now().UTC()
	weekIv, _, err := quota.WeeklyWindowAt(anchor, now)
	if err != nil {
		t.Fatal(err)
	}
	monthIv, _, err := quota.MonthlyWindowAt(anchor, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []*domain.QuotaWindow{
		{EntitlementID: entID, Kind: domain.WindowFiveHour,
			Start: now.Add(-time.Hour), End: now.Add(4 * time.Hour), Limit: 100, Used: 100},
		{EntitlementID: entID, Kind: domain.WindowWeekly,
			Start: weekIv.Start, End: weekIv.End, Limit: 500, Used: 500},
		{EntitlementID: entID, Kind: domain.WindowMonthly,
			Start: monthIv.Start, End: monthIv.End, Limit: 9000, Used: 10},
	} {
		if err := f.store.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	w := f.get(t, "/user/model-quotas", tokA)
	data := decodeData(t, w)
	blocks := blocksOf(t, data)
	if len(blocks) != 2 || blocks[0]["kind"] != "five_hour" || blocks[1]["kind"] != "weekly" {
		t.Fatalf("blocked_by = %v, want five_hour+weekly", blocks)
	}
	// 多窗口阻断各自带准确重置时刻与计数快照。
	if blocks[0]["resets_at"] == nil || blocks[1]["resets_at"] == nil {
		t.Errorf("blocks missing resets_at: %v", blocks)
	}
	wins := windowsOf(t, data)
	if wins["five_hour"]["remaining"] != "0" || wins["weekly"]["remaining"] != "0" ||
		wins["monthly"]["remaining"] != "8990" {
		t.Errorf("remaining = %v / %v / %v",
			wins["five_hour"]["remaining"], wins["weekly"]["remaining"], wins["monthly"]["remaining"])
	}
	assertFixture(t, "model-quotas-exhausted.json", w.Body.String())
}

func TestFixture_ModelQuotas_Expired(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	ctx := context.Background()
	now := time.Now().UTC()
	anchor := now.Add(-48 * time.Hour)
	expiredTo := now.Add(-time.Hour)
	entID, _ := f.seedQuotaOnly(t, userA, 1_000_000, 10_000_000, 100_000_000,
		anchor, &expiredTo, domain.EntitlementExpired)
	// 最后一周窗口仍在当前周期内（anchor 2 天前 → 周窗 [anchor, +7d)）：
	// 过期后已用额度如实保留可见。
	weekIv, _, err := quota.WeeklyWindowAt(anchor, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertQuotaWindow(ctx, &domain.QuotaWindow{
		EntitlementID: entID, Kind: domain.WindowWeekly,
		Start: weekIv.Start, End: weekIv.End, Limit: 10_000_000, Used: 42,
	}); err != nil {
		t.Fatal(err)
	}

	w := f.get(t, "/user/model-quotas", tokA)
	data := decodeData(t, w)
	blocks := blocksOf(t, data)
	if len(blocks) != 1 || blocks[0]["kind"] != "entitlement" || blocks[0]["reason"] != "entitlement_expired" {
		t.Fatalf("blocked_by = %v", blocks)
	}
	if blocks[0]["expired_at"] == nil {
		t.Error("entitlement_expired must carry expired_at (effective_to)")
	}
	ent := data["entitlement"].(map[string]any)
	if ent["status"] != "expired" || ent["effective_to"] == nil {
		t.Errorf("entitlement = %v", ent)
	}
	wins := windowsOf(t, data)
	if wins["weekly"]["used"] != "42" {
		t.Errorf("expired weekly used = %v (已用不隐藏)", wins["weekly"]["used"])
	}
	if wins["five_hour"]["activation"] != "on_first_consumption" {
		t.Errorf("expired five_hour = %v", wins["five_hour"])
	}
	assertFixture(t, "model-quotas-expired.json", w.Body.String())
}

func TestFixture_ModelQuotas_Reserved(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	ctx := context.Background()
	now := time.Now().UTC()
	anchor := now.Add(-48 * time.Hour)
	entID, polID := f.seedQuotaOnly(t, userA, 1_000_000, 10_000_000, 100_000_000,
		anchor, nil, domain.EntitlementActive)
	acct, err := f.store.GetBillingAccountByUser(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}

	// 真实在途预占（预占中 fixture）：预占提交、未结算。
	start := now.Add(-time.Hour)
	winIDs := map[domain.WindowKind]string{}
	for _, spec := range []struct {
		kind  domain.WindowKind
		end   time.Time
		limit domain.Microcredit
	}{
		{domain.WindowFiveHour, start.Add(5 * time.Hour), 1_000_000},
		{domain.WindowWeekly, start.Add(7 * 24 * time.Hour), 10_000_000},
		{domain.WindowMonthly, start.AddDate(0, 1, 0), 100_000_000},
	} {
		w := &domain.QuotaWindow{EntitlementID: entID, Kind: spec.kind, Start: start, End: spec.end, Limit: spec.limit}
		if err := f.store.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatal(err)
		}
		winIDs[spec.kind] = w.ID
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hold := domain.Microcredit(7_500)
	w5, ww, wm := winIDs[domain.WindowFiveHour], winIDs[domain.WindowWeekly], winIDs[domain.WindowMonthly]
	_, err = f.store.Reserve(ctx, uow, domain.ReserveCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: acct.ID, EntitlementID: entID,
			ModelID: "glm-4.6", Protocol: domain.ProtocolOpenAIChat, PolicyVersionID: polID,
		},
		Holds: []domain.HoldSpec{
			{TargetKind: domain.TargetWindowFiveHour, WindowID: &w5, Amount: hold},
			{TargetKind: domain.TargetWindowWeekly, WindowID: &ww, Amount: hold},
			{TargetKind: domain.TargetWindowMonthly, WindowID: &wm, Amount: hold},
		},
		AdmittedAt: now,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	w := f.get(t, "/user/model-quotas", tokA)
	data := decodeData(t, w)
	if len(blocksOf(t, data)) != 0 {
		t.Errorf("reserved-but-available must not block: %v", data["blocked_by"])
	}
	wins := windowsOf(t, data)
	if wins["five_hour"]["reserved"] != "7500" || wins["five_hour"]["remaining"] != "992500" {
		t.Errorf("five_hour reserved view = %v", wins["five_hour"])
	}
	assertFixture(t, "model-quotas-reserved.json", w.Body.String())
}

func TestFixture_ModelQuotas_CrossMonth(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	// 1 月 31 日锚点：2 月裁剪到月末、3 月恢复 31 日，不永久漂移（§6）。
	anchor := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	far := time.Date(2027, 1, 31, 10, 0, 0, 0, time.UTC)
	f.seedQuotaOnly(t, userA, 1_000_000, 10_000_000, 100_000_000,
		anchor, &far, domain.EntitlementActive)

	w := f.get(t, "/user/model-quotas", tokA)
	data := decodeData(t, w)
	wins := windowsOf(t, data)
	// 语义断言：月窗边界 = 以 as_of 重算的锚定公历月周期（含裁剪）。
	asOf, err := time.Parse(time.RFC3339, data["as_of"].(string))
	if err != nil {
		t.Fatal(err)
	}
	iv, _, err := quota.MonthlyWindowAt(anchor, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if wins["monthly"]["window_start"] != iv.Start.Format(time.RFC3339) ||
		wins["monthly"]["resets_at"] != iv.End.Format(time.RFC3339) {
		t.Errorf("monthly bounds = %v..%v, want %v..%v",
			wins["monthly"]["window_start"], wins["monthly"]["resets_at"], iv.Start, iv.End)
	}
	// 锚点日裁剪钉死：当前为 2026-09 → 周期 [Aug 31 10:00, Sep 30 10:00)。
	if iv.Start.Day() != 31 || iv.Start.Month() != time.August {
		t.Errorf("clamped start = %v, want Aug 31", iv.Start)
	}
	if iv.End.Day() != 30 || iv.End.Month() != time.September {
		t.Errorf("clamped end = %v, want Sep 30 (Sep has 30 days)", iv.End)
	}
	assertFixture(t, "model-quotas-cross-month.json", w.Body.String())
}

func TestFixture_ModelUsage_RequestsReconciliation(t *testing.T) {
	f := newViewsFixture(t)
	userA, tokA := f.addUser(t)
	g := f.seedViewGraph(t, userA)

	w := f.get(t, "/user/model-usage/requests?limit=50", tokA)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	data := decodeData(t, w)
	items := data["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (settled + parked)", len(items))
	}
	var parked, settled map[string]any
	for _, it := range items {
		m := it.(map[string]any)
		switch m["request_id"] {
		case g.reqParked:
			parked = m
		case g.reqSettled:
			settled = m
		}
	}
	// 待核对：状态如实、无计量事实（tokens=null）、预占保留可见、未结算
	// charge 为 null —— 未知不记 0。
	if parked == nil || parked["status"] != "reconciliation_required" || parked["usage_status"] != "unknown" {
		t.Fatalf("parked row = %v", parked)
	}
	if parked["tokens"] != nil || parked["charge_micros"] != nil || parked["net_micros"] != nil {
		t.Errorf("parked amounts must be null: %v", parked)
	}
	if parked["reserved_micros"] != "9999" {
		t.Errorf("parked reserved = %v", parked["reserved_micros"])
	}
	if settled == nil || settled["usage_status"] != "reported" || settled["charge_micros"] != "60000" {
		t.Errorf("settled row = %v", settled)
	}
	assertFixture(t, "model-usage-requests-reconciliation.json", w.Body.String())
}
