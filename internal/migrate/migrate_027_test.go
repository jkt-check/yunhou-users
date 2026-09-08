package migrate

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// loadRealMigrations loads the repo's migrations/ directory, splitting the
// result into "everything before 027" and the 027 file itself so the test
// can stage pre-027 anomalous data before applying it.
func loadRealMigrations(t *testing.T) (pre []Migration, mig027 Migration) {
	t.Helper()
	migs, err := LoadFiles(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Skipf("no migrations/ dir (%v)", err)
	}
	found := false
	for _, m := range migs {
		if m.ID == "027_subscription_product_scope" {
			mig027 = m
			found = true
			continue
		}
		pre = append(pre, m)
	}
	if !found {
		t.Skip("027_subscription_product_scope.sql not present")
	}
	return pre, mig027
}

// TestMigrate027_DuplicateActiveDiagnostic stages the historical anomaly the
// 027 DO block guards against (two active subscriptions for one
// user+product) and asserts the migration refuses with a readable diagnostic
// naming the offending rows — instead of a bare CREATE UNIQUE INDEX
// unique_violation or, worse, silent success. After the operator resolves
// the anomaly the same migration must apply cleanly, and re-application is a
// ledger-level no-op.
func TestMigrate027_DuplicateActiveDiagnostic(t *testing.T) {
	db := freshTestDB(t)
	ctx := context.Background()
	pre, mig027 := loadRealMigrations(t)

	if _, _, err := Apply(ctx, db, pre); err != nil {
		t.Fatalf("apply pre-027 migrations: %v", err)
	}

	// Stage the anomaly. The legacy global index idx_subscriptions_user_active
	// makes the state unreachable through normal writes, so the test drops it
	// first — the same way manual production surgery could have produced it.
	if _, err := db.ExecContext(ctx, `DROP INDEX idx_subscriptions_user_active`); err != nil {
		t.Fatalf("drop legacy index: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, status) VALUES ('11111111-1111-1111-1111-111111111111', 'active')
	`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	for _, planID := range []string{"monthly", "yearly"} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO subscriptions (user_id, plan_id, status, started_at)
			VALUES ('11111111-1111-1111-1111-111111111111', $1, 'active', now())
		`, planID); err != nil {
			t.Fatalf("insert anomalous sub %s: %v", planID, err)
		}
	}

	// 027 must refuse with the readable diagnostic (user id + count + sub ids).
	_, _, err := Apply(ctx, db, []Migration{mig027})
	if err == nil {
		t.Fatal("027 applied despite duplicate active subscriptions")
	}
	if !strings.Contains(err.Error(), "duplicate active subscriptions per (user_id, product_code)") {
		t.Fatalf("error lacks diagnostic header: %v", err)
	}
	if !strings.Contains(err.Error(), "user_id=11111111-1111-1111-1111-111111111111 product_code=kaya-membership active_count=2") {
		t.Fatalf("error lacks offending row detail: %v", err)
	}

	// The failure must not leave a half-applied state: neither the new index
	// nor the product_code column may exist yet.
	var colExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'subscriptions' AND column_name = 'product_code'
		)
	`).Scan(&colExists); err != nil {
		t.Fatalf("check column: %v", err)
	}
	if colExists {
		t.Fatal("product_code column present after failed migration — partial apply leaked")
	}

	// Operator resolution: cancel one of the two active rows.
	if _, err := db.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'cancelled'
		WHERE id = (
			SELECT id FROM subscriptions
			WHERE user_id = '11111111-1111-1111-1111-111111111111' AND status = 'active'
			ORDER BY created_at LIMIT 1
		)
	`); err != nil {
		t.Fatalf("resolve anomaly: %v", err)
	}
	if _, _, err := Apply(ctx, db, []Migration{mig027}); err != nil {
		t.Fatalf("027 after resolving anomaly: %v", err)
	}

	// Post-conditions: new per-product index exists, legacy index is gone,
	// existing rows were backfilled to kaya-membership.
	var idxExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_subscriptions_user_product_active')
	`).Scan(&idxExists); err != nil {
		t.Fatalf("check new index: %v", err)
	}
	if !idxExists {
		t.Fatal("idx_subscriptions_user_product_active missing after 027")
	}
	var legacyExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_subscriptions_user_active')
	`).Scan(&legacyExists); err != nil {
		t.Fatalf("check legacy index: %v", err)
	}
	if legacyExists {
		t.Fatal("legacy idx_subscriptions_user_active still present after 027")
	}
	var pc string
	if err := db.QueryRowContext(ctx, `
		SELECT product_code FROM subscriptions
		WHERE user_id = '11111111-1111-1111-1111-111111111111' AND status = 'active'
	`).Scan(&pc); err != nil {
		t.Fatalf("read backfilled product_code: %v", err)
	}
	if pc != "kaya-membership" {
		t.Errorf("backfilled product_code = %q, want kaya-membership", pc)
	}

	// Ledger no-op: re-applying 027 is skipped, not re-run.
	applied, skipped, err := Apply(ctx, db, []Migration{mig027})
	if err != nil {
		t.Fatalf("027 rerun: %v", err)
	}
	if applied != 0 || skipped != 1 {
		t.Errorf("027 rerun: applied=%d skipped=%d, want 0/1", applied, skipped)
	}
}
