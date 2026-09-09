package httpapi_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/migrate"
)

// DB-backed handler tests against the disposable instance (skip is NOT a
// pass). Migrations applied once in TestMain; -p 1 keeps the shared wipe
// race-free.

var testDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
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

// newTestServer builds a gin engine with BOTH the read-only and the write
// routes mounted. Production router.Setup mounts only RegisterReadOnly in
// Task 3 (see router_test.go); the write surface here stands in for the
// Task 4 authorized mount so the handlers are exercised end to end.
func newTestServer(t *testing.T) *gin.Engine {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`TRUNCATE
		inference_session_bindings, inference_oauth_grants,
		inference_audit_log,
		operator_roles,
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

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := httpapi.NewAdminModelsHandler(management.NewCatalogManager(catalog.NewService(postgres.NewStore(db)), nil,
		func(context.Context, string) error { return nil })) // permissive egress stub
	group := engine.Group("/admin")
	h.RegisterReadOnly(group)
	h.RegisterWrite(group)
	return engine
}
