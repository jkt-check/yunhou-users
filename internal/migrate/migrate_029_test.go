package migrate

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrate029_OrderBenefitSnapshot verifies the Task 10 migration:
//   - pre-029 orders are backfilled with the product/interval snapshot from
//     their plan rows (等价于旧路径当时会读到的值，行为不变);
//   - the snapshot CHECK constraints reject malformed rows (grant mode
//     without policy version, unknown kind);
//   - plan_benefit_configs / plan_upgrade_rules exist with their FKs;
//   - re-application is a ledger-level no-op (idempotent).
func TestMigrate029_OrderBenefitSnapshot(t *testing.T) {
	db := freshTestDB(t)
	ctx := context.Background()

	migs, err := LoadFiles(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Skipf("no migrations/ dir (%v)", err)
	}
	var pre, all []Migration
	var mig029 Migration
	for _, m := range migs {
		if m.ID == "029_order_benefit_snapshot" {
			mig029 = m
			continue
		}
		pre = append(pre, m)
	}
	if mig029.ID == "" {
		t.Skip("029_order_benefit_snapshot.sql not present")
	}
	all = append(append([]Migration{}, pre...), mig029)

	// Stage a pre-029 order (no snapshot columns) and a plan to backfill from.
	if _, _, err := Apply(ctx, db, pre); err != nil {
		t.Fatalf("apply pre-029: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, status) VALUES ('11111111-1111-1111-1111-111111111111', 'active');
		INSERT INTO plans (id, name, price, interval_days, product_code)
		VALUES ('cp_pre', 'CP Pre', 29.9, 30, 'coding-plan');
		INSERT INTO orders (user_id, plan_id, amount, currency, status)
		VALUES ('11111111-1111-1111-1111-111111111111', 'cp_pre', 29.9, 'CNY', 'pending');
	`); err != nil {
		t.Fatalf("stage pre-029 rows: %v", err)
	}

	if _, _, err := Apply(ctx, db, []Migration{mig029}); err != nil {
		t.Fatalf("apply 029: %v", err)
	}

	// Backfill: the staged order carries the plan's product + interval.
	var product string
	var interval int
	if err := db.QueryRowxContext(ctx,
		`SELECT product_code, plan_interval_days FROM orders WHERE plan_id = 'cp_pre'`).
		Scan(&product, &interval); err != nil {
		t.Fatalf("read backfilled order: %v", err)
	}
	if product != "coding-plan" || interval != 30 {
		t.Fatalf("backfill = %s/%d, want coding-plan/30", product, interval)
	}

	// CHECK: benefit_grant_mode without policy version is rejected.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders (user_id, plan_id, amount, currency, status, benefit_grant_mode)
		VALUES ('11111111-1111-1111-1111-111111111111', 'cp_pre', 1, 'CNY', 'pending', 'subscription')
	`); err == nil {
		t.Fatal("snapshot consistency CHECK must reject grant mode without policy version")
	}
	// CHECK: unknown order kind is rejected.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO orders (user_id, plan_id, amount, currency, status, order_kind)
		VALUES ('11111111-1111-1111-1111-111111111111', 'cp_pre', 1, 'CNY', 'pending', 'bogus')
	`); err == nil {
		t.Fatal("order_kind CHECK must reject unknown kinds")
	}

	// plan_benefit_configs: FK to plans and inference_policy_versions.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan_benefit_configs (plan_id, policy_version_id, model_ids)
		VALUES ('cp_pre', '00000000-0000-0000-0000-000000000000', '{}')
	`); err == nil {
		t.Fatal("benefit config must reject a dangling policy version reference")
	}
	// plan_upgrade_rules: same-plan rule rejected by CHECK.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plan_upgrade_rules (from_plan_id, to_plan_id) VALUES ('cp_pre', 'cp_pre')
	`); err == nil {
		t.Fatal("self-upgrade rule must be rejected")
	}

	// Idempotent re-apply: the ledger records it once; the second run skips.
	if _, _, err := Apply(ctx, db, all); err != nil {
		t.Fatalf("re-apply all: %v", err)
	}
	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM _migrations WHERE id = '029_order_benefit_snapshot'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ledger rows for 029 = %d, want 1", n)
	}
}
