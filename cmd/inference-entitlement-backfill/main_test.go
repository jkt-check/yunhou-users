package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/migrate"
)

// backfillTestDSN points at the disposable instance (DATABASE_URL env,
// same convention as the other DB-backed suites / 与其他 DB 测试套件同约定).
func backfillTestDSN() string {
	if dsn := strings.TrimSpace(strings.TrimRight(os.Getenv("DATABASE_URL"), "\n")); dsn != "" {
		return dsn
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

// prevCatalogActive records the catalog revision that was active before a
// test published its own, so the wipe can restore it (local dev DBs carry
// state between runs / 本地库跨轮次带状态，wipe 负责还原).
var prevCatalogActive []int64

// setupBackfillDB applies all migrations and registers the wipe cleanup
// (registered FIRST so it runs LAST per test, in FK-safe order).
func setupBackfillDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", backfillTestDSN())
	if err != nil {
		t.Skipf("no postgres available: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	migs, err := migrate.LoadFiles("../../migrations")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() { wipeBackfillRows(db) })
	return db
}

// wipeBackfillRows deletes every row this suite created (bf-* namespace)
// in FK-safe order and restores the previously active catalog revision.
func wipeBackfillRows(db *sqlx.DB) {
	db.Exec(`DELETE FROM inference_entitlements WHERE source_id IN
		(SELECT id::text FROM subscriptions WHERE plan_id LIKE 'bf-%')`)
	db.Exec(`DELETE FROM inference_billing_accounts WHERE user_id IN
		(SELECT id FROM users WHERE nickname LIKE 'bf-%')`)
	db.Exec(`DELETE FROM subscriptions WHERE plan_id LIKE 'bf-%'`)
	db.Exec(`DELETE FROM plan_benefit_configs WHERE plan_id LIKE 'bf-%'`)
	db.Exec(`DELETE FROM inference_policy_versions WHERE name LIKE 'bf-%'`)
	db.Exec(`DELETE FROM plans WHERE id LIKE 'bf-%'`)
	db.Exec(`DELETE FROM users WHERE nickname LIKE 'bf-%'`)
	db.Exec(`DELETE FROM inference_config_revisions WHERE created_by = 'backfill-test'`)
	for _, id := range prevCatalogActive {
		db.Exec(`UPDATE inference_config_revisions SET is_active = true, status = 'published'
			WHERE id = $1 AND NOT EXISTS
			(SELECT 1 FROM inference_config_revisions WHERE scope = 'catalog' AND is_active)`, id)
	}
	prevCatalogActive = nil
}

// seedCatalog publishes one active catalog revision carrying activeIDs as
// lifecycle=active models and draftIDs as lifecycle=draft models (draft =
// 不可售，全集口径必须排除).
func seedCatalog(t *testing.T, db *sqlx.DB, activeIDs, draftIDs []string) {
	t.Helper()
	var prevID int64
	switch err := db.QueryRow(
		`SELECT id FROM inference_config_revisions WHERE scope = 'catalog' AND is_active`).Scan(&prevID); {
	case err == nil:
		prevCatalogActive = append(prevCatalogActive, prevID)
	case !errors.Is(err, sql.ErrNoRows):
		t.Fatalf("probe active catalog revision: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE inference_config_revisions SET is_active = false, status = 'superseded'
		 WHERE scope = 'catalog' AND is_active`); err != nil {
		t.Fatalf("supersede active catalog revision: %v", err)
	}
	models := make([]domain.Model, 0, len(activeIDs)+len(draftIDs))
	for _, id := range activeIDs {
		models = append(models, catalogModel(id, domain.LifecycleActive))
	}
	for _, id := range draftIDs {
		models = append(models, catalogModel(id, domain.LifecycleDraft))
	}
	payload := catalog.BuildCatalogPayload(models, nil, nil, nil)
	var rev int
	if err := db.QueryRow(
		`SELECT COALESCE(MAX(revision), 0) + 1 FROM inference_config_revisions WHERE scope = 'catalog'`).Scan(&rev); err != nil {
		t.Fatalf("next revision: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO inference_config_revisions (scope, revision, payload, status, is_active, published_at, created_by)
		 VALUES ('catalog', $1, $2, 'published', true, now(), 'backfill-test')`, rev, payload); err != nil {
		t.Fatalf("insert catalog revision: %v", err)
	}
}

func catalogModel(id string, lifecycle domain.Lifecycle) domain.Model {
	return domain.Model{
		ID: id, DisplayName: id, Lifecycle: lifecycle, ModelVersion: "v1",
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		ContextTokens: 8192, MaxOutputTokens: 1024,
		Protocols: []domain.Protocol{domain.ProtocolKayaChat},
	}
}

// seedPlan creates an active plan covering the yunhou-website app (kaya
// desktop reuses it, migration 019). chatModels nil = NULL allowlist
// (legacy /chat "unrestricted"). withConfig also creates the published
// policy version + plan_benefit_configs row the grant path pins; it
// returns the policy version id ("" when withConfig is false).
func seedPlan(t *testing.T, db *sqlx.DB, planID string, chatModels []string, withConfig bool) string {
	t.Helper()
	var cm interface{}
	if chatModels != nil {
		cm = pq.Array(chatModels)
	}
	if _, err := db.Exec(
		`INSERT INTO plans (id, name, price, interval_days, apps, is_active, chat_models)
		 VALUES ($1, $1, 0, 30, ARRAY['yunhou-website'], true, $2)`, planID, cm); err != nil {
		t.Fatalf("seed plan %s: %v", planID, err)
	}
	if !withConfig {
		return ""
	}
	var policyID string
	if err := db.QueryRow(
		`INSERT INTO inference_policy_versions (name, revision, model_ids, status)
		 VALUES ($1, 1, '{}', 'published') RETURNING id`, planID).Scan(&policyID); err != nil {
		t.Fatalf("seed policy for %s: %v", planID, err)
	}
	if _, err := db.Exec(
		`INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids, grant_mode)
		 VALUES ($1, $2, '{}', 'subscription')`, planID, policyID); err != nil {
		t.Fatalf("seed benefit config for %s: %v", planID, err)
	}
	return policyID
}

// seedUserSub creates one user plus an active subscription on the plan.
func seedUserSub(t *testing.T, db *sqlx.DB, planID string, expires *time.Time) (userID, subID string) {
	t.Helper()
	if err := db.QueryRow(
		`INSERT INTO users (nickname, status) VALUES ('bf-' || uuid_generate_v4()::text, 'active')
		 RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.QueryRow(
		`INSERT INTO subscriptions (user_id, plan_id, status, started_at, expires_at)
		 VALUES ($1, $2, 'active', now(), $3) RETURNING id`, userID, planID, expires).Scan(&subID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return userID, subID
}

func countWhere(t *testing.T, db *sqlx.DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := db.Get(&n, query, args...); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// --- Step 1 failing tests (TDD) -------------------------------------------

// dry_run：报告计数正确，零写库。
func TestBackfillDryRunWritesNothing(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1", "bf-m2"}, nil)
	seedPlan(t, db, "bf-dry", nil, true)
	exp := time.Now().Add(30 * 24 * time.Hour)
	userID, _ := seedUserSub(t, db, "bf-dry", &exp)

	rep, err := run(backfillTestDSN(), true, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Processed != 1 || rep.AccountsCreated != 1 || rep.EntitlementsCreated != 1 ||
		rep.Skipped != 0 || len(rep.Errors) != 0 {
		t.Fatalf("dry-run report: %+v", rep)
	}
	if !rep.DryRun {
		t.Fatal("report must carry dry_run=true")
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_billing_accounts WHERE user_id = $1`, userID); n != 0 {
		t.Fatalf("dry-run created %d billing accounts", n)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_entitlements e
		 JOIN inference_billing_accounts a ON a.id = e.billing_account_id
		 WHERE a.user_id = $1`, userID); n != 0 {
		t.Fatalf("dry-run created %d entitlements", n)
	}
}

// 实跑：NULL chat_models → entitlement model_ids = 已发布目录全集（draft
// 排除）；无 billing account → 新建。
func TestBackfillNullChatModelsGrantsFullCatalog(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1", "bf-m2"}, []string{"bf-draft"})
	seedPlan(t, db, "bf-null", nil, true)
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
	userID, subID := seedUserSub(t, db, "bf-null", &exp)

	rep, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Processed != 1 || rep.AccountsCreated != 1 || rep.EntitlementsCreated != 1 {
		t.Fatalf("report: %+v", rep)
	}
	var srcType string
	var modelIDs pq.StringArray
	var effTo *time.Time
	var revision int
	if err := db.QueryRow(
		`SELECT source_type, model_ids, effective_to, revision
		 FROM inference_entitlements WHERE source_id = $1`, subID).
		Scan(&srcType, &modelIDs, &effTo, &revision); err != nil {
		t.Fatalf("read entitlement: %v", err)
	}
	if srcType != "subscription" || revision != 1 {
		t.Fatalf("source=%s revision=%d", srcType, revision)
	}
	if len(modelIDs) != 2 || modelIDs[0] != "bf-m1" || modelIDs[1] != "bf-m2" {
		t.Fatalf("model_ids = %v, want full published active set [bf-m1 bf-m2]", modelIDs)
	}
	if effTo == nil || !effTo.Equal(exp) {
		t.Fatalf("effective_to = %v, want subscription expiry %v", effTo, exp)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_billing_accounts WHERE user_id = $1`, userID); n != 1 {
		t.Fatalf("billing accounts = %d, want 1", n)
	}
}

// 实跑：显式数组 → 子集（去重、保持数组语义，不扩成全集）。
func TestBackfillExplicitChatModelsSubset(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1", "bf-m2"}, nil)
	seedPlan(t, db, "bf-sub", []string{"bf-m2", "bf-m2"}, true)
	exp := time.Now().Add(30 * 24 * time.Hour)
	_, subID := seedUserSub(t, db, "bf-sub", &exp)

	rep, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.EntitlementsCreated != 1 {
		t.Fatalf("report: %+v", rep)
	}
	var modelIDs pq.StringArray
	if err := db.QueryRow(
		`SELECT model_ids FROM inference_entitlements WHERE source_id = $1`, subID).Scan(&modelIDs); err != nil {
		t.Fatal(err)
	}
	if len(modelIDs) != 1 || modelIDs[0] != "bf-m2" {
		t.Fatalf("model_ids = %v, want subset [bf-m2]", modelIDs)
	}
}

// 幂等：第二次实跑零增量（created=0，skipped=1，行数不变）。
func TestBackfillRerunZeroIncrement(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1"}, nil)
	seedPlan(t, db, "bf-idem", nil, true)
	exp := time.Now().Add(30 * 24 * time.Hour)
	userID, _ := seedUserSub(t, db, "bf-idem", &exp)

	rep1, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep1.AccountsCreated != 1 || rep1.EntitlementsCreated != 1 {
		t.Fatalf("first run: %+v", rep1)
	}
	for i := 0; i < 2; i++ {
		rep, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Processed != 1 || rep.AccountsCreated != 0 || rep.EntitlementsCreated != 0 ||
			rep.Skipped != 1 || len(rep.Errors) != 0 {
			t.Fatalf("re-run %d must be a zero increment: %+v", i, rep)
		}
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_billing_accounts WHERE user_id = $1`, userID); n != 1 {
		t.Fatalf("billing accounts = %d, want exactly 1", n)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_entitlements e
		 JOIN inference_billing_accounts a ON a.id = e.billing_account_id
		 WHERE a.user_id = $1`, userID); n != 1 {
		t.Fatalf("entitlements = %d, want exactly 1", n)
	}
}

// 只增不删：已有 entitlement 含目录外模型 → 保留，一行不动。
func TestBackfillAdditiveOnlyPreservesExisting(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1"}, nil)
	policyID := seedPlan(t, db, "bf-add", nil, true)
	exp := time.Now().Add(30 * 24 * time.Hour)
	userID, subID := seedUserSub(t, db, "bf-add", &exp)

	// 预置：账户 + 同来源权益，模型集合含目录外 id（如已下线但历史上授权过的模型）。
	var acctID string
	if err := db.QueryRow(
		`INSERT INTO inference_billing_accounts (user_id) VALUES ($1) RETURNING id`, userID).Scan(&acctID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO inference_entitlements
		 (billing_account_id, source_type, source_id, model_ids, policy_version_id,
		  anchor_at, effective_from, effective_to)
		 VALUES ($1, 'subscription', $2, $3, $4, now(), now(), $5)`,
		acctID, subID, pq.Array([]string{"bf-m1", "legacy-off-catalog"}), policyID, exp); err != nil {
		t.Fatal(err)
	}

	rep, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Processed != 1 || rep.AccountsCreated != 0 || rep.EntitlementsCreated != 0 || rep.Skipped != 1 {
		t.Fatalf("report: %+v", rep)
	}
	var modelIDs pq.StringArray
	if err := db.QueryRow(
		`SELECT model_ids FROM inference_entitlements WHERE source_id = $1`, subID).Scan(&modelIDs); err != nil {
		t.Fatal(err)
	}
	if len(modelIDs) != 2 || modelIDs[0] != "bf-m1" || modelIDs[1] != "legacy-off-catalog" {
		t.Fatalf("existing entitlement was rewritten: %v", modelIDs)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_entitlements WHERE source_id = $1`, subID); n != 1 {
		t.Fatalf("entitlements = %d, want exactly 1", n)
	}
}

// 失败行单列：plan 缺 benefit 配置（无 policy version 可钉）计入 errors，
// 不阻断整批；正常用户照常发放。
func TestBackfillFailureIsolationPerRow(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1"}, nil)
	seedPlan(t, db, "bf-good", nil, true)
	seedPlan(t, db, "bf-nocfg", nil, false)
	exp := time.Now().Add(30 * 24 * time.Hour)
	goodUser, _ := seedUserSub(t, db, "bf-good", &exp)
	badUser, _ := seedUserSub(t, db, "bf-nocfg", &exp)

	rep, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Processed != 2 || rep.EntitlementsCreated != 1 || len(rep.Errors) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Errors[0].UserID != badUser {
		t.Fatalf("error row user = %s, want %s", rep.Errors[0].UserID, badUser)
	}
	if !strings.Contains(rep.Errors[0].Reason, "plan_benefit_configs") {
		t.Fatalf("error reason should name the missing config: %s", rep.Errors[0].Reason)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_entitlements e
		 JOIN inference_billing_accounts a ON a.id = e.billing_account_id
		 WHERE a.user_id = $1`, goodUser); n != 1 {
		t.Fatalf("good user entitlements = %d, want 1", n)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_billing_accounts WHERE user_id = $1`, badUser); n != 0 {
		t.Fatalf("failed user must not get a half-applied account, got %d", n)
	}
}

// 过期/取消/不覆盖目标 app 的订阅不进候选集。
func TestBackfillSkipsIneligibleSubscriptions(t *testing.T) {
	db := setupBackfillDB(t)
	seedCatalog(t, db, []string{"bf-m1"}, nil)
	seedPlan(t, db, "bf-elig", nil, true)
	past := time.Now().Add(-time.Hour)
	expiredUser, _ := seedUserSub(t, db, "bf-elig", &past)
	// 未过期订阅（对照组：应被处理）。
	future := time.Now().Add(30 * 24 * time.Hour)
	liveUser, _ := seedUserSub(t, db, "bf-elig", &future)

	rep, err := run(backfillTestDSN(), false, 10, "yunhou-website", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Processed != 1 || rep.EntitlementsCreated != 1 || len(rep.Errors) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_billing_accounts WHERE user_id = $1`, expiredUser); n != 0 {
		t.Fatalf("expired subscription must not be backfilled, got %d accounts", n)
	}
	if n := countWhere(t, db,
		`SELECT COUNT(*) FROM inference_billing_accounts WHERE user_id = $1`, liveUser); n != 1 {
		t.Fatalf("live subscription must be backfilled, got %d accounts", n)
	}
}

// 入参校验：缺 DSN / 负样本数 / 空 app 快速失败。
func TestBackfillValidation(t *testing.T) {
	dsn := backfillTestDSN()
	for _, tc := range []struct {
		name    string
		dsn     string
		sample  int
		appID   string
	}{
		{"no dsn", "", 10, "yunhou-website"},
		{"negative sample", dsn, -1, "yunhou-website"},
		{"empty app", dsn, 10, ""},
	} {
		if _, err := run(tc.dsn, true, tc.sample, tc.appID, 5*time.Second); err == nil {
			t.Errorf("%s: must fail", tc.name)
		}
	}
}
