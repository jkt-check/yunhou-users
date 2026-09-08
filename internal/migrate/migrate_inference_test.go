package migrate

import (
	"testing"
)

// Functional test for migrations/024-026 (inference module skeleton,
// Kaya Coding Plan Task 1): the real repo migrations must create the
// documented table groups with their key constraints, and the full chain
// must be re-runnable (repo idempotency convention).

// inferenceTables is the authoritative table list created by 024–026.
var inferenceTables = []string{
	// 024 catalog
	"inference_models", "inference_providers", "inference_deployments",
	"inference_model_routes", "inference_config_revisions",
	// 025 accounts
	"inference_credentials", "inference_upstream_accounts",
	"inference_billing_accounts", "inference_api_keys",
	// 026 accounting
	"inference_price_versions", "inference_policy_versions", "inference_entitlements",
	"inference_requests", "inference_attempts", "inference_usage_records",
	"inference_quota_windows", "inference_reservations", "inference_concurrency_leases",
	"inference_ledger_entries", "inference_adjustments",
	"inference_outbox", "inference_reconciliation_jobs",
}

func TestMigrations024to026_InferenceTables(t *testing.T) {
	db := freshTestDB(t)
	applyRealMigrations(t, db)

	for _, tbl := range inferenceTables {
		var exists bool
		if err := db.Get(&exists,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`, tbl); err != nil {
			t.Fatalf("check %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("table %s missing after migrations 024-026", tbl)
		}
	}

	// 账本边界：billing_accounts 对 users 不得级联删除。
	var deleteRule string
	if err := db.Get(&deleteRule,
		`SELECT rc.delete_rule FROM information_schema.referential_constraints rc
		 JOIN information_schema.key_column_usage kcu ON rc.constraint_name = kcu.constraint_name
		 JOIN information_schema.constraint_column_usage ccu ON rc.constraint_name = ccu.constraint_name
		 WHERE rc.constraint_name LIKE 'inference_billing_accounts%'
		   AND kcu.table_name = 'inference_billing_accounts' AND kcu.column_name = 'user_id'
		   AND ccu.table_name = 'users'`); err != nil {
		t.Fatalf("billing account FK delete rule: %v", err)
	}
	if deleteRule == "CASCADE" {
		t.Errorf("inference_billing_accounts.user_id delete rule = CASCADE, want NO ACTION/RESTRICT (账本不级联删除)")
	}

	// 结算唯一键：idx_inference_ledger_charge_once 存在且为部分唯一索引。
	var chargeIdx bool
	if err := db.Get(&chargeIdx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes
		                WHERE indexname = 'idx_inference_ledger_charge_once'
		                  AND indexdef LIKE '%WHERE%')`); err != nil {
		t.Fatalf("charge unique index: %v", err)
	}
	if !chargeIdx {
		t.Error("idx_inference_ledger_charge_once missing (结算唯一键)")
	}

	// 窗口不重叠排他约束存在。
	var excl bool
	if err := db.Get(&excl,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint
		                WHERE conname = 'inference_quota_windows_no_overlap' AND contype = 'x')`); err != nil {
		t.Fatalf("window exclude constraint: %v", err)
	}
	if !excl {
		t.Error("inference_quota_windows_no_overlap missing (窗口区间排他)")
	}
}
