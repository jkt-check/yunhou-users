// policy_repo_test.go — 配额策略管理面(spec
// 2026-09-26-admin-quota-policies-design.md)的真实库行为:迁移 040 的
// superseded 枚举 + 每 name 单 published 部分唯一索引;repo 新方法
// (list/count/max/insert/update/publish/retire);A12 PAYG 拒绝非
// published;A13 发布新 revision 后存量权益 pin 不变。
package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

func seedPolicy(t *testing.T, s *Store, name string, rev int, status string) *PolicyVersion {
	t.Helper()
	w := domain.Microcredit(5_000_000)
	p := &PolicyVersion{
		Name: name, Revision: rev, ModelIDs: []string{"glm-4.6"},
		WeeklyLimit: &w, OveragePolicy: "reject", Status: status,
	}
	if err := s.InsertPolicyVersion(context.Background(), p); err != nil {
		t.Fatalf("insert policy %s r%d: %v", name, rev, err)
	}
	return p
}

// 迁移 040:同名第二条 published 在 DB 层被拒(部分唯一索引);superseded
// 是合法状态。
func TestPolicySupersededConstraint(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	if err := s.InsertModel(ctx, &domain.Model{ID: "glm-4.6", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1}); err != nil {
		t.Fatal(err)
	}

	seedPolicy(t, s, "dup-pub", 1, "published")
	p2 := &PolicyVersion{
		Name: "dup-pub", Revision: 2, ModelIDs: []string{"glm-4.6"}, Status: "published",
	}
	if err := s.InsertPolicyVersion(ctx, p2); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second published per name: err = %v, want conflict (partial unique index)", err)
	}
	// superseded 不受该索引限制。
	p3 := &PolicyVersion{
		Name: "dup-pub", Revision: 3, ModelIDs: []string{"glm-4.6"}, Status: "superseded",
	}
	if err := s.InsertPolicyVersion(ctx, p3); err != nil {
		t.Fatalf("superseded must be a legal status: %v", err)
	}
}

// list:过滤(name 精确 / status)、排序(name ASC, revision DESC)、分页。
func TestListQuotaPolicies_FilterSortPage(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	if err := s.InsertModel(ctx, &domain.Model{ID: "glm-4.6", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	seedPolicy(t, s, "alpha", 1, "draft")
	seedPolicy(t, s, "alpha", 2, "draft")
	seedPolicy(t, s, "beta", 1, "published")

	items, err := s.ListQuotaPolicies(ctx, management.QuotaPolicyFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if items[0].Name != "alpha" || items[0].Revision != 2 ||
		items[1].Name != "alpha" || items[1].Revision != 1 ||
		items[2].Name != "beta" {
		t.Fatalf("sort = %s r%d, %s r%d, %s — want alpha r2, alpha r1, beta",
			items[0].Name, items[0].Revision, items[1].Name, items[1].Revision, items[2].Name)
	}
	items, err = s.ListQuotaPolicies(ctx, management.QuotaPolicyFilter{Name: "beta", Status: "published", Limit: 100})
	if err != nil || len(items) != 1 || items[0].Name != "beta" {
		t.Fatalf("filtered = %+v err=%v, want just beta", items, err)
	}
	items, err = s.ListQuotaPolicies(ctx, management.QuotaPolicyFilter{Limit: 2, Offset: 2})
	if err != nil || len(items) != 1 {
		t.Fatalf("paged = %d err=%v, want 1", len(items), err)
	}
}

// count:active 权益窗口内计数;过期/其他状态不计。
func TestCountActiveEntitlementsByPolicy(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true) // 权益 status='active', effective_to 为 NULL

	n, err := s.CountActiveEntitlementsByPolicy(ctx, f.policyID)
	if err != nil || n != 1 {
		t.Fatalf("count = %d err=%v, want 1", n, err)
	}
	// 过期窗口不计。
	if _, err := s.db.Exec(
		`UPDATE inference_entitlements SET effective_to = now() - interval '1 hour' WHERE id = $1`, f.entID); err != nil {
		t.Fatal(err)
	}
	n, err = s.CountActiveEntitlementsByPolicy(ctx, f.policyID)
	if err != nil || n != 0 {
		t.Fatalf("expired count = %d err=%v, want 0", n, err)
	}
}

// publish tx:同名旧 published 转 superseded + 目标 draft 转 published 原子;
// 非 draft 目标 0 行 → conflict;published_at 记录。
func TestPublishQuotaPolicyTx_SupersedesAtomically(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	if err := s.InsertModel(ctx, &domain.Model{ID: "glm-4.6", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	r1 := seedPolicy(t, s, "kaya-gift", 1, "published")
	r2 := seedPolicy(t, s, "kaya-gift", 2, "draft")

	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishQuotaPolicyTx(ctx, uow, r2.ID, r2.Name, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got1, _ := s.GetPolicyVersion(ctx, r1.ID)
	got2, _ := s.GetPolicyVersion(ctx, r2.ID)
	if got1.Status != "superseded" || got2.Status != "published" {
		t.Fatalf("r1/r2 = %s/%s, want superseded/published", got1.Status, got2.Status)
	}
	if got2.PublishedAt == nil {
		t.Fatal("published_at not recorded")
	}

	// 对 superseded 目标 publish → 0 行 → conflict。
	uow2, _ := s.Begin(ctx)
	err = s.PublishQuotaPolicyTx(ctx, uow2, r1.ID, r1.Name, time.Now().UTC())
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("publish superseded: code = %v, want conflict", domain.CodeOf(err))
	}
}

// retire tx:draft/published/superseded → retired;已 retired → conflict。
func TestRetireQuotaPolicyTx(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	if err := s.InsertModel(ctx, &domain.Model{ID: "glm-4.6", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	p := seedPolicy(t, s, "kaya-gift", 1, "draft")

	uow, _ := s.Begin(ctx)
	if err := s.RetireQuotaPolicyTx(ctx, uow, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPolicyVersion(ctx, p.ID)
	if got.Status != "retired" {
		t.Fatalf("status = %s, want retired", got.Status)
	}

	uow2, _ := s.Begin(ctx)
	err := s.RetireQuotaPolicyTx(ctx, uow2, p.ID)
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("retire retired: code = %v, want conflict", domain.CodeOf(err))
	}
}

// draft 编辑 tx:仅 model_ids/limits/overage 可变;非 draft 0 行 → conflict。
func TestUpdateDraftQuotaPolicyTx(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	if err := s.InsertModel(ctx, &domain.Model{ID: "glm-4.6", DisplayName: "x", ContextTokens: 1, MaxOutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	p := seedPolicy(t, s, "kaya-gift", 1, "draft")

	w := int64(9_000_000)
	info := &management.QuotaPolicyInfo{
		ID: p.ID, ModelIDs: p.ModelIDs, WeeklyLimit: &w, OveragePolicy: "allow_overage",
	}
	uow, _ := s.Begin(ctx)
	if err := s.UpdateDraftQuotaPolicyTx(ctx, uow, info); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetPolicyVersion(ctx, p.ID)
	if got.WeeklyLimit == nil || *got.WeeklyLimit != 9_000_000 || got.OveragePolicy != "allow_overage" {
		t.Fatalf("updated = %+v", got)
	}
	// 未提及的 limit 被显式清空(draft 全量替换语义)。
	if got.FiveHourLimit != nil || got.MonthlyLimit != nil || got.RPMLimit != nil {
		t.Fatalf("unspecified limits must be cleared: %+v", got)
	}

	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'published' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	uow2, _ := s.Begin(ctx)
	err := s.UpdateDraftQuotaPolicyTx(ctx, uow2, info)
	_ = uow2.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("update published: code = %v, want conflict", domain.CodeOf(err))
	}
}

// A13:发布新 revision 后,存量权益 pin 不变,旧版本内容原样可读(网关
// 按 id 加载不受影响)。
func TestPolicyPinStabilityAcrossPublish(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true) // 权益 pin 在 f.policyID(draft 状态)

	// 发布 r1,再发 r2(r1 转 superseded)。
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'published', published_at = now() WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	r2 := &PolicyVersion{
		Name: "coding-plan-" + f.modelID, Revision: 2, ModelIDs: []string{f.modelID},
		Status: "draft",
	}
	if err := s.InsertPolicyVersion(ctx, r2); err != nil {
		t.Fatal(err)
	}
	uow, _ := s.Begin(ctx)
	if err := s.PublishQuotaPolicyTx(ctx, uow, r2.ID, r2.Name, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 存量权益仍 pin r1;网关按 id 读到的内容(限额/模型集)原封不动。
	var pinned string
	if err := s.db.QueryRow(`SELECT policy_version_id FROM inference_entitlements WHERE id = $1`, f.entID).Scan(&pinned); err != nil || pinned != f.policyID {
		t.Fatalf("pin = %s err=%v, want unchanged %s", pinned, err, f.policyID)
	}
	got, err := s.GetPolicyVersion(ctx, f.policyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "superseded" {
		t.Fatalf("r1 status = %s, want superseded", got.Status)
	}
	if got.FiveHourLimit == nil || *got.FiveHourLimit != 1_000_000 || len(got.ModelIDs) != 1 {
		t.Fatalf("r1 content mutated: %+v", got)
	}
}

// A12:PAYG 配置拒绝 superseded/retired 版本(draft 拒绝已有既有测试)。
func TestPutPAYGConfig_RejectsSupersededAndRetired(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	for _, st := range []string{"superseded", "retired"} {
		if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = $1 WHERE id = $2`, st, f.policyID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutPAYGConfig(ctx, f.policyID, []string{f.modelID}, "user:ops@app:test"); domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Fatalf("payg config with %s policy: code = %v, want invalid_input", st, domain.CodeOf(err))
		}
	}
}

// 防止 management.QuotaPolicyFilter 与本文件未使用变量告警的编译锚点。
var _ = uuid.NewString

// grant 侧守卫(评审轮2 finding 1):retired 策略版本拒绝新发放引用;
// superseded/draft 放行(支付链路兼容,见 lockPolicyForGrantTx 注释)。
func TestGrantRejectsRetiredPolicy(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'retired' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err := s.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceGrant,
		SourceID: "grant-retired-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: now, EffectiveFrom: now,
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("grant on retired policy: code = %v, want conflict", domain.CodeOf(err))
	}

	// 同事务路径(InsertEntitlementTx)同样拒绝。
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'draft' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	uow, _ := s.Begin(ctx)
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'retired' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	err = s.InsertEntitlementTx(ctx, uow, &domain.Entitlement{
		BillingAccountID: f.accountID, SourceType: domain.SourceGrant,
		SourceID: "grant-retired-tx-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: now, EffectiveFrom: now,
	})
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("tx grant on retired policy: code = %v, want conflict", domain.CodeOf(err))
	}
}

// PAYG 开启路径:配置引用的策略被退役后,新开启被拒(既有权益不受影响)。
func TestEnsurePAYGEntitlement_RejectsRetiredPolicy(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)

	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'published' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPAYGConfig(ctx, f.policyID, []string{f.modelID}, "user:ops@app:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE inference_policy_versions SET status = 'retired' WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	uow, _ := s.Begin(ctx)
	_, err := s.EnsurePAYGEntitlementTx(ctx, uow, f.accountID, time.Now().UTC())
	_ = uow.Rollback(ctx)
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("payg enable on retired policy: code = %v, want conflict", domain.CodeOf(err))
	}
}
