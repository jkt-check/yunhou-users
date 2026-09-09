package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/migrate"
)

// These tests need a real PostgreSQL (专用可丢弃实例; 禁止对业务库运行).
// They skip only when no database is reachable — the task acceptance
// requires running them against the disposable instance, a skip is NOT a
// pass.
//
// Migrations are applied ONCE in TestMain: migrate.Apply takes the
// session-scoped pg_advisory_lock (not released at COMMIT), so calling it
// per-test on a shared database would deadlock the connection pool
// against itself. Per-test isolation comes from wiping the tables.

var testDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		// No database: tests will skip individually via testDB.
		os.Exit(m.Run())
	}
	migs, err := migrate.LoadFiles("../../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		os.Exit(1)
	}
	if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		os.Exit(1)
	}
	testDSN = dsn
	db.Close()
	os.Exit(m.Run())
}

// testDB connects and wipes all inference (+ users) rows. Tests run
// under `-p 1` (plan constraint: 数据库测试包串行), so a shared wipe is
// race-free.
func testDB(t *testing.T) (*sqlx.DB, *Store) {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	wipeInference(t, db)
	return db, NewStore(db)
}

// wipeInference resets all inference tables (plus users for account
// fixtures). Tests in this package run under `-p 1` (plan constraint:
// 数据库测试包串行), so a shared wipe is race-free.
func wipeInference(t *testing.T, db *sqlx.DB) {
	t.Helper()
	_, err := db.Exec(`TRUNCATE
		inference_session_bindings, inference_oauth_grants,
		inference_reconciliation_jobs, inference_outbox,
		inference_ledger_entries, inference_adjustments,
		inference_concurrency_leases, inference_reservations,
		inference_quota_windows, inference_usage_records,
		inference_attempts, inference_requests,
		inference_entitlements, inference_policy_versions,
		inference_price_versions,
		inference_api_keys, inference_billing_accounts,
		inference_upstream_accounts, inference_credentials,
		inference_config_revisions, inference_model_routes,
		inference_deployments, inference_providers, inference_models,
		users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
}

// fixture builds the minimal reference chain for accounting tests:
// user → billing account → policy → entitlement (+ optional key).
type fixture struct {
	userID    string
	accountID string
	policyID  string
	entID     string
	keyID     string
	modelID   string
}

func seedFixture(t *testing.T, s *Store, withKey bool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{modelID: "glm-4.6"}

	userID := uuid.NewString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	f.userID = userID

	acct := &domain.BillingAccount{UserID: userID}
	if err := s.InsertBillingAccount(ctx, acct); err != nil {
		t.Fatalf("insert billing account: %v", err)
	}
	f.accountID = acct.ID

	if err := s.InsertModel(ctx, &domain.Model{
		ID: f.modelID, DisplayName: "GLM 4.6",
		ContextTokens: 200000, MaxOutputTokens: 8192,
	}); err != nil {
		t.Fatalf("insert model: %v", err)
	}

	pol := &PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{f.modelID},
		FiveHourLimit: micro(1_000_000), WeeklyLimit: micro(10_000_000),
		MonthlyLimit: micro(100_000_000),
	}
	if err := s.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	f.policyID = pol.ID

	now := time.Date(2026, 9, 8, 4, 0, 0, 0, time.UTC)
	ent := &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}
	if err := s.InsertEntitlement(ctx, ent); err != nil {
		t.Fatalf("insert entitlement: %v", err)
	}
	f.entID = ent.ID

	if withKey {
		key := &domain.APIKey{
			BillingAccountID: acct.ID, Name: "cli", Prefix: "yk-test-" + uuid.NewString()[:8],
			BudgetLimit: micro(5_000_000),
		}
		if err := s.InsertAPIKey(ctx, key, "sha256:"+uuid.NewString()); err != nil {
			t.Fatalf("insert api key: %v", err)
		}
		f.keyID = key.ID
	}
	return f
}

func micro(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

// makeWindows creates the three active windows for an entitlement.
func makeWindows(t *testing.T, s *Store, entID string) (w5, ww, wm string) {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	specs := []struct {
		kind  domain.WindowKind
		end   time.Time
		limit domain.Microcredit
	}{
		{domain.WindowFiveHour, start.Add(5 * time.Hour), 1_000_000},
		{domain.WindowWeekly, start.Add(7 * 24 * time.Hour), 10_000_000},
		{domain.WindowMonthly, start.AddDate(0, 1, 0), 100_000_000},
	}
	ids := make([]string, 0, 3)
	for _, sp := range specs {
		w := &domain.QuotaWindow{
			EntitlementID: entID, Kind: sp.kind, Start: start, End: sp.end, Limit: sp.limit,
		}
		if err := s.InsertQuotaWindow(ctx, w); err != nil {
			t.Fatalf("insert %s window: %v", sp.kind, err)
		}
		ids = append(ids, w.ID)
	}
	return ids[0], ids[1], ids[2]
}
