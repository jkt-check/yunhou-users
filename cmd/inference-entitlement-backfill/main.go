// cmd/inference-entitlement-backfill is the OFFLINE operator backfill
// command (R7-N6): legacy /chat subscriptions (subscriptions +
// plans.chat_models) are converged into inference billing accounts +
// explicit-model entitlements, so the kaya chat gateway switch can be
// flipped without locking out existing subscribers.
// （R7-N6 迁移工具：把存量 /chat 订阅幂等回填为 inference 计费账户 +
// 显式模型权益，供网关开关翻转前执行。）
//
// Semantics（口径）:
//   - Candidate: subscriptions.status='active' AND not expired
//     (expires_at NULL = never expires) AND plans.is_active AND
//     plans.apps covers -app (kaya 桌面端复用 app_id=yunhou-website,
//     migration 019).
//   - Model set: plans.chat_models NULL or empty = the legacy /chat
//     "unrestricted" semantics (service/chat.go checkAccess only filters
//     when the list is non-empty) → materialized as every ACTIVE-lifecycle
//     model of the published catalog revision; an explicit array is the
//     granted subset as-is (inside inference there is no NULL-means-all —
//     the set is always explicit, 设计 §4.2).
//   - Policy version: pinned from plan_benefit_configs.policy_version_id —
//     the SAME source the purchase grant path snapshots onto orders
//     (migration 029). A plan without a benefit-config row cannot be
//     granted and lands in errors[] (无配置 = 商品未发布权益口径);
//     a retired policy version rejects new grants (mirrors
//     postgres.lockPolicyForGrantTx).
//   - Entitlement source key: ('subscription', subscriptions.id) revision
//     1 — the same key the payment entitlement-sync worker converges on,
//     so a later real purchase revises this row instead of duplicating it.
//
// Idempotency / ledger discipline（幂等与账本纪律）:
//   - Re-run is a zero increment: inference_billing_accounts
//     UNIQUE(user_id) + inference_entitlements
//     UNIQUE(source_type, source_id, revision) absorb repeats
//     (ON CONFLICT DO NOTHING); a source that already has ANY entitlement
//     row (any status, any model set, including supersets) is skipped —
//     the tool is additive-only and NEVER updates/deletes existing rows.
//   - Per-user transaction: one failing row lands in errors[] and does
//     not abort the batch.
//
// Usage:
//
//	inference-entitlement-backfill -dry-run   # audit report, zero writes
//	inference-entitlement-backfill            # real run
//	inference-entitlement-backfill            # re-run → zero increment
//
// Env: DATABASE_URL (or -dsn). Migrations must already be applied.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	inferencepostgres "github.com/yunhou/users/internal/inference/postgres"
)

// defaultAppID is the app kaya desktop signs in with (migration 019: kaya
// 复用 app_id=yunhou-website 登录).
const defaultAppID = "yunhou-website"

func main() {
	log.SetFlags(0)
	log.SetPrefix("inference-entitlement-backfill: ")

	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "postgres DSN (default: DATABASE_URL)")
	dryRun := flag.Bool("dry-run", false, "compute the report only; writes nothing")
	sample := flag.Int("sample", 10, "number of per-user sample rows in the report")
	appID := flag.String("app", defaultAppID, "app_id the plan must cover")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall run timeout")
	flag.Parse()

	rep, err := run(*dsn, *dryRun, *sample, *appID, *timeout)
	if err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		log.Fatal(err)
	}
	// 行级失败不置退出码：批次已收敛，errors[] 是审计面（操作员据此修数后
	// 重跑）；只有工具自身无法运行（无 DSN / 无已发布目录 / 查询失败）才
	// log.Fatal 非零退出。
	if len(rep.Errors) > 0 {
		log.Printf("completed with %d row error(s) — see errors[] in the report", len(rep.Errors))
	}
}

// rowError is one per-user failure entry in the report.
type rowError struct {
	UserID string `json:"user_id"`
	Reason string `json:"reason"`
}

// sampleEntry is one audited row of the report's sample window.
type sampleEntry struct {
	UserID             string   `json:"user_id"`
	SubscriptionID     string   `json:"subscription_id"`
	PlanID             string   `json:"plan_id"`
	ModelIDs           []string `json:"model_ids"`
	AccountCreated     bool     `json:"account_created"`
	EntitlementCreated bool     `json:"entitlement_created"`
	Note               string   `json:"note,omitempty"`
}

// report is the audit document printed to stdout as JSON. In dry-run mode
// the *_created counters are what a real run WOULD create.
type report struct {
	DryRun               bool          `json:"dry_run"`
	AppID                string        `json:"app_id"`
	CatalogRevision      int           `json:"catalog_revision"`
	CatalogModels        []string      `json:"catalog_models"`
	Processed            int           `json:"processed"`
	AccountsCreated      int           `json:"accounts_created"`
	EntitlementsCreated  int           `json:"entitlements_created"`
	Skipped              int           `json:"skipped"`
	Errors               []rowError    `json:"errors"`
	Samples              []sampleEntry `json:"samples"`
}

// candidate is one eligible subscription row joined with its plan.
type candidate struct {
	SubID      string         `db:"id"`
	UserID     string         `db:"user_id"`
	PlanID     string         `db:"plan_id"`
	ExpiresAt  *time.Time     `db:"expires_at"`
	ChatModels pq.StringArray `db:"chat_models"` // NULL → nil
}

// run executes the backfill and returns the audit report. It is the
// testable seam behind main.
func run(dsn string, dryRun bool, sampleN int, appID string, timeout time.Duration) (*report, error) {
	if dsn == "" {
		return nil, errors.New("DATABASE_URL or -dsn is required")
	}
	if sampleN < 0 {
		return nil, errors.New("-sample must be >= 0")
	}
	if appID == "" {
		return nil, errors.New("-app must not be empty")
	}

	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 已发布目录快照是 "/chat 目录模型全集" 的唯一权威来源 —— 目录未发布
	// 时全集口径无从谈起，快速失败而不是按空集合回填。
	store := inferencepostgres.NewStore(db)
	rev, err := store.ActiveRevision(ctx, domain.ScopeCatalog)
	if err != nil {
		return nil, fmt.Errorf("load active catalog revision (publish the catalog first): %w", err)
	}
	snap, err := catalog.ParseSnapshot(rev)
	if err != nil {
		return nil, fmt.Errorf("parse catalog revision %d: %w", rev.Revision, err)
	}
	fullSet := activeModelIDs(snap)

	rep := &report{
		DryRun: dryRun, AppID: appID,
		CatalogRevision: rev.Revision, CatalogModels: fullSet,
		Errors: []rowError{}, Samples: []sampleEntry{},
	}

	cands, err := listCandidates(ctx, db, appID)
	if err != nil {
		return nil, err
	}
	for _, c := range cands {
		rep.Processed++
		out, rerr := processCandidate(ctx, db, c, fullSet, dryRun)
		entry := sampleEntry{
			UserID: c.UserID, SubscriptionID: c.SubID, PlanID: c.PlanID,
			ModelIDs:           out.modelIDs,
			AccountCreated:     out.accountCreated,
			EntitlementCreated: out.entitlementCreated,
		}
		switch {
		case rerr != nil:
			rep.Errors = append(rep.Errors, rowError{UserID: c.UserID, Reason: rerr.Error()})
			entry.Note = "error: " + rerr.Error()
		case out.skipped:
			rep.Skipped++
			entry.Note = "skipped: subscription source already has an entitlement (additive-only, left untouched)"
		default:
			if out.accountCreated {
				rep.AccountsCreated++
			}
			if out.entitlementCreated {
				rep.EntitlementsCreated++
			}
			entry.Note = "granted"
			if dryRun {
				entry.Note = "would grant"
			}
		}
		if len(rep.Samples) < sampleN {
			rep.Samples = append(rep.Samples, entry)
		}
	}
	return rep, nil
}

// listCandidates selects active, non-expired subscriptions whose plan is
// active and covers the app（不区分 product_code：同一来源幂等键让
// coding-plan 订阅撞上既有购买发放时自然跳过）.
func listCandidates(ctx context.Context, db *sqlx.DB, appID string) ([]candidate, error) {
	var rows []candidate
	err := db.SelectContext(ctx, &rows,
		`SELECT s.id, s.user_id, s.plan_id, s.expires_at, p.chat_models
		   FROM subscriptions s
		   JOIN plans p ON p.id = s.plan_id
		  WHERE s.status = 'active'
		    AND (s.expires_at IS NULL OR s.expires_at > now())
		    AND p.is_active
		    AND p.apps @> ARRAY[$1]::text[]
		  ORDER BY s.id`, appID)
	if err != nil {
		return nil, fmt.Errorf("list candidate subscriptions: %w", err)
	}
	return rows, nil
}

// outcome is one candidate's result (in dry-run: the would-be result).
type outcome struct {
	modelIDs           []string
	accountCreated     bool
	entitlementCreated bool
	skipped            bool
}

// processCandidate resolves the grant target and applies it (or simulates
// the application in dry-run). Row-level failures are returned as errors
// for the report; they never abort the batch.
func processCandidate(ctx context.Context, db *sqlx.DB, c candidate, fullSet []string, dryRun bool) (outcome, error) {
	models := targetModels(c.ChatModels, fullSet)
	if len(models) == 0 {
		return outcome{}, fmt.Errorf("plan %s resolves to an empty model set (published catalog has no active models)", c.PlanID)
	}
	if dryRun {
		return dryRunOutcome(ctx, db, c, models)
	}
	return applyOutcome(ctx, db, c, models)
}

// getter is the read surface shared by *sqlx.DB (dry-run probes) and
// *sqlx.Tx (real-run per-user transaction).
type getter interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

// resolvePolicy pins the plan's quota policy version from
// plan_benefit_configs — the same source the purchase path snapshots onto
// orders (migration 029). Retired versions reject new grants, mirroring
// postgres.lockPolicyForGrantTx (superseded/draft pass, retired 拒绝).
func resolvePolicy(ctx context.Context, g getter, planID string) (string, error) {
	var policyID string
	err := g.GetContext(ctx, &policyID,
		`SELECT policy_version_id FROM plan_benefit_configs WHERE plan_id = $1`, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("plan %s has no plan_benefit_configs row (no policy version to pin — publish the plan's benefit mapping first)", planID)
	}
	if err != nil {
		return "", fmt.Errorf("read plan_benefit_configs for plan %s: %w", planID, err)
	}
	var status string
	if err := g.GetContext(ctx, &status,
		`SELECT status FROM inference_policy_versions WHERE id = $1`, policyID); err != nil {
		return "", fmt.Errorf("read policy version %s: %w", policyID, err)
	}
	if status == "retired" {
		return "", fmt.Errorf("plan %s pins retired policy version %s (不再允许新发放引用)", planID, policyID)
	}
	return policyID, nil
}

// dryRunOutcome computes the would-be result with reads only — zero writes.
func dryRunOutcome(ctx context.Context, db *sqlx.DB, c candidate, models []string) (outcome, error) {
	if _, err := resolvePolicy(ctx, db, c.PlanID); err != nil {
		return outcome{modelIDs: models}, err
	}
	var entExists, acctExists bool
	if err := db.GetContext(ctx, &entExists,
		`SELECT EXISTS(SELECT 1 FROM inference_entitlements
		 WHERE source_type = 'subscription' AND source_id = $1)`, c.SubID); err != nil {
		return outcome{modelIDs: models}, fmt.Errorf("probe existing entitlement: %w", err)
	}
	if err := db.GetContext(ctx, &acctExists,
		`SELECT EXISTS(SELECT 1 FROM inference_billing_accounts WHERE user_id = $1)`, c.UserID); err != nil {
		return outcome{modelIDs: models}, fmt.Errorf("probe billing account: %w", err)
	}
	return outcome{
		modelIDs:           models,
		accountCreated:     !acctExists,
		entitlementCreated: !entExists,
		skipped:            entExists,
	}, nil
}

// applyOutcome runs one candidate's ensure-account + insert-entitlement in
// a single per-user transaction.
func applyOutcome(ctx context.Context, db *sqlx.DB, c candidate, models []string) (outcome, error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return outcome{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	out := outcome{modelIDs: models}
	policyID, err := resolvePolicy(ctx, tx, c.PlanID)
	if err != nil {
		return out, err
	}

	// 账户幂等建立：与 postgres.EnsureBillingAccount 同形
	// （UNIQUE(user_id) + ON CONFLICT 读回），只是跑在逐用户事务内。
	var acctID string
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_billing_accounts (user_id) VALUES ($1)
		 ON CONFLICT (user_id) DO NOTHING RETURNING id`, c.UserID).Scan(&acctID)
	switch {
	case err == nil:
		out.accountCreated = true
	case errors.Is(err, sql.ErrNoRows):
		if err := tx.GetContext(ctx, &acctID,
			`SELECT id FROM inference_billing_accounts WHERE user_id = $1`, c.UserID); err != nil {
			return out, fmt.Errorf("read back billing account: %w", err)
		}
	default:
		return out, fmt.Errorf("ensure billing account: %w", err)
	}

	// 只增不改（账本纪律）：同一来源已有任何状态/任何模型集合的权益行即
	// 跳过 —— 绝不 UPDATE/DELETE 既有行；后续规格变化由支付链路的
	// entitlement-sync worker 以修订语义处理。
	var exists bool
	if err := tx.GetContext(ctx, &exists,
		`SELECT EXISTS(SELECT 1 FROM inference_entitlements
		 WHERE source_type = 'subscription' AND source_id = $1)`, c.SubID); err != nil {
		return out, fmt.Errorf("probe existing entitlement: %w", err)
	}
	if exists {
		out.skipped = true
		// 账户 ensure 仍然提交（"ensure exists" 语义与权益跳过相互独立）。
		if err := tx.Commit(); err != nil {
			return out, fmt.Errorf("commit: %w", err)
		}
		return out, nil
	}

	now := time.Now().UTC()
	var entID string
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_entitlements
		 (billing_account_id, source_type, source_id, model_ids, policy_version_id,
		  anchor_at, effective_from, effective_to, revision, stackable, status)
		 VALUES ($1, 'subscription', $2, $3, $4, $5, $5, $6, 1, false, 'active')
		 ON CONFLICT (source_type, source_id, revision) DO NOTHING
		 RETURNING id`,
		acctID, c.SubID, pq.Array(models), policyID, now, c.ExpiresAt).Scan(&entID)
	switch {
	case err == nil:
		out.entitlementCreated = true
	case errors.Is(err, sql.ErrNoRows):
		// 并发发放者先到（购买链路 entitlement-sync 与批量回填竞争）——
		// 无失败语句、事务干净，按跳过计。
		out.skipped = true
	default:
		return out, fmt.Errorf("insert entitlement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// targetModels maps the legacy plan allowlist to the explicit entitlement
// model set: NULL/empty chat_models = legacy /chat "unrestricted"
// (service/chat.go only filters when the list is non-empty) → the full
// published-catalog active set; an explicit array is the granted subset,
// deduped, order preserved.
func targetModels(chatModels pq.StringArray, fullSet []string) []string {
	if len(chatModels) == 0 {
		return append([]string{}, fullSet...)
	}
	seen := make(map[string]struct{}, len(chatModels))
	out := make([]string, 0, len(chatModels))
	for _, id := range chatModels {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// activeModelIDs is the "/chat 目录全集" 口径：已发布目录快照中
// lifecycle='active' 的模型（与 catalog 可售判定同口径，revision.go 的
// RoutableModel 要求 active），排序保证报告与落库确定性。
func activeModelIDs(snap *catalog.Snapshot) []string {
	ids := make([]string, 0, len(snap.Models))
	for id, m := range snap.Models {
		if m.Lifecycle == domain.LifecycleActive {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
