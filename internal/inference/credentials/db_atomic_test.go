// db_atomic_test.go — DB-backed atomicity tests for the credential write
// path (Task 4 review fix #3): change + propagation + audit commit or roll
// back together inside ONE UnitOfWork. A missing DATABASE_URL skips — skip
// is NOT a pass; the acceptance run always sets it.

package credentials_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/migrate"
)

var atomicDSN string

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn != "" {
		db, err := sqlx.Connect("postgres", dsn)
		if err == nil {
			migs, err := migrate.LoadFiles("../../../migrations")
			if err != nil {
				fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
				os.Exit(1)
			}
			if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
				fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
				os.Exit(1)
			}
			atomicDSN = dsn
			db.Close()
		}
	}
	os.Exit(m.Run())
}

// failRecorder implements both management.AuditRecorder and the service's
// TxRecorder; every call fails — the fault-injection seam for rollback tests.
type failRecorder struct{ err error }

func (f *failRecorder) Record(_ context.Context, _ management.AuditEvent) error { return f.err }

func (f *failRecorder) RecordTx(_ context.Context, _ domain.UnitOfWork, _ management.AuditEvent) error {
	return f.err
}

func atomicSetup(t *testing.T) (*sqlx.DB, *postgres.Store, *credentials.Service, credentials.Operator, string) {
	t.Helper()
	if atomicDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", atomicDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM inference_session_bindings WHERE account_id IN (SELECT id FROM inference_upstream_accounts WHERE provider_id IN (SELECT id FROM inference_providers WHERE code = 'task4atomic'))`)
		db.Exec(`DELETE FROM inference_upstream_accounts WHERE provider_id IN (SELECT id FROM inference_providers WHERE code = 'task4atomic')`)
		db.Exec(`DELETE FROM inference_credentials WHERE provider_id IN (SELECT id FROM inference_providers WHERE code = 'task4atomic')`)
		db.Exec(`DELETE FROM inference_audit_log WHERE actor_app_id = 'task4-atomic-app'`)
		db.Exec(`DELETE FROM inference_providers WHERE code = 'task4atomic'`)
		db.Close()
	})
	if _, err := db.Exec(`INSERT INTO inference_providers (code, display_name, access_type)
		VALUES ('task4atomic','Atomic Provider','official_api')
		ON CONFLICT (code) DO NOTHING`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	var providerID string
	if err := db.Get(&providerID, `SELECT id FROM inference_providers WHERE code = 'task4atomic'`); err != nil {
		t.Fatalf("resolve provider: %v", err)
	}
	key := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: key}, 1)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewStore(db)
	svc := credentials.NewService(vault, store, store)
	op := credentials.Operator{UserID: "11111111-1111-4111-8111-111111111111", AppID: "task4-atomic-app", Roles: []string{"admin"}}
	return db, store, svc, op, providerID
}

func TestCreateAuditFailureRollsBack(t *testing.T) {
	db, store, _, op, providerID := atomicSetup(t)

	svc := credentials.NewService(mustVault(t), store, &failRecorder{err: fmt.Errorf("audit store down")})
	if _, err := svc.Create(context.Background(), op, providerID, "lbl", "api_key", "secret-material", "reason", nil); err == nil {
		t.Fatal("create with failing audit must error")
	}
	var n int
	if err := db.Get(&n, `SELECT count(*) FROM inference_credentials WHERE provider_id = $1`, providerID); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("credential committed despite audit failure: %d rows", n)
	}
	if err := db.Get(&n, `SELECT count(*) FROM inference_audit_log WHERE actor_app_id = 'task4-atomic-app'`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("partial audit state after rollback: %d rows", n)
	}
}

func TestSetStatusDisablePropagatesAtomically(t *testing.T) {
	db, store, svc, op, providerID := atomicSetup(t)
	ctx := context.Background()

	view, err := svc.Create(ctx, op, providerID, "lbl", "api_key", "secret-material", "reason", nil)
	if err != nil {
		t.Fatal(err)
	}
	account := &domain.UpstreamAccount{
		ProviderID: providerID, CredentialID: view.ID, ExternalAccountID: "ext-1",
		DisplayName: "acct", Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := store.InsertUpstreamAccount(ctx, account); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetStatus(ctx, op, view.ID, "revoked", "emergency"); err != nil {
		t.Fatal(err)
	}
	var credStatus, acctStatus string
	db.Get(&credStatus, `SELECT status FROM inference_credentials WHERE id = $1`, view.ID)
	db.Get(&acctStatus, `SELECT status FROM inference_upstream_accounts WHERE credential_id = $1`, view.ID)
	if credStatus != "revoked" || acctStatus != "disabled" {
		t.Fatalf("disable not atomic: credential=%s account=%s", credStatus, acctStatus)
	}
	var disabled int
	if err := db.Get(&disabled,
		`SELECT coalesce((detail->>'upstream_accounts_disabled')::int, -1)
		   FROM inference_audit_log WHERE action = 'credential.disable' AND object_id = $1`, view.ID); err != nil {
		t.Fatal(err)
	}
	if disabled != 1 {
		t.Fatalf("audit detail upstream_accounts_disabled = %d, want 1", disabled)
	}
}

func TestSetStatusAuditFailureRollsBack(t *testing.T) {
	db, store, svc, op, providerID := atomicSetup(t)
	ctx := context.Background()

	view, err := svc.Create(ctx, op, providerID, "lbl", "api_key", "secret-material", "reason", nil)
	if err != nil {
		t.Fatal(err)
	}
	account := &domain.UpstreamAccount{
		ProviderID: providerID, CredentialID: view.ID, ExternalAccountID: "ext-1",
		DisplayName: "acct", Status: domain.AccountActive, ConcurrencyLimit: 1,
	}
	if err := store.InsertUpstreamAccount(ctx, account); err != nil {
		t.Fatal(err)
	}

	broken := credentials.NewService(mustVault(t), store, &failRecorder{err: fmt.Errorf("audit store down")})
	if _, err := broken.SetStatus(ctx, op, view.ID, "revoked", "emergency"); err == nil {
		t.Fatal("set status with failing audit must error")
	}
	var credStatus, acctStatus string
	db.Get(&credStatus, `SELECT status FROM inference_credentials WHERE id = $1`, view.ID)
	db.Get(&acctStatus, `SELECT status FROM inference_upstream_accounts WHERE credential_id = $1`, view.ID)
	if credStatus != "active" || acctStatus != "active" {
		t.Fatalf("rollback violated: credential=%s account=%s, want both active", credStatus, acctStatus)
	}
	var auditRows int
	db.Get(&auditRows, `SELECT count(*) FROM inference_audit_log WHERE object_id = $1 AND action = 'credential.disable'`, view.ID)
	if auditRows != 0 {
		t.Fatalf("disable audit row committed despite failure: %d", auditRows)
	}
}

func mustVault(t *testing.T) *credentials.Vault {
	t.Helper()
	key := make([]byte, credentials.KeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := credentials.NewVault(map[int][]byte{1: key}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return vault
}
