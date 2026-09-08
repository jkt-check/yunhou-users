package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/migrate"
)

// bootstrapTestDSN points at the disposable instance (DATABASE_URL env,
// same convention as the other DB-backed suites).
func bootstrapTestDSN() string {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

// setupBootstrapDB applies all migrations (including 028) and returns a db
// handle plus the seeded user id.
func setupBootstrapDB(t *testing.T) (*sqlx.DB, string) {
	t.Helper()
	dsn := bootstrapTestDSN()
	db, err := sqlx.Connect("postgres", dsn)
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
	var userID string
	err = db.Get(&userID,
		`INSERT INTO users (nickname, status) VALUES ('bootstrap-target','active') RETURNING id`)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM operator_roles WHERE user_id = $1`, userID)
		db.Exec(`DELETE FROM inference_audit_log`)
		db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	})
	return db, userID
}

func TestBootstrapIdempotent(t *testing.T) {
	db, userID := setupBootstrapDB(t)
	dsn := bootstrapTestDSN()

	// First run creates the grant; second and third runs are no-ops that
	// still succeed (重复运行不产生重复管理员).
	for i := 0; i < 3; i++ {
		roles, target, err := run(dsn, userID, "", "admin", "founding operator", 15*time.Second)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if target != userID || len(roles) != 1 || roles[0] != "admin" {
			t.Fatalf("run %d: roles=%v target=%s", i, roles, target)
		}
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM operator_roles WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("operator_roles rows = %d, want exactly 1", n)
	}
}

func TestBootstrapMultipleRoles(t *testing.T) {
	_, userID := setupBootstrapDB(t)
	dsn := bootstrapTestDSN()
	roles, _, err := run(dsn, userID, "", "admin,operator,auditor", "full grant", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 3 {
		t.Fatalf("roles = %v", roles)
	}
}

func TestBootstrapValidation(t *testing.T) {
	dsn := bootstrapTestDSN()
	for _, tc := range []struct {
		name, dsn, userID, email, roles, reason string
	}{
		{"no dsn", "", "u", "", "admin", "r"},
		{"both identities", dsn, "u", "e@x.com", "admin", "r"},
		{"neither identity", dsn, "", "", "admin", "r"},
		{"empty reason", dsn, "u", "", "admin", ""},
		{"unknown role", dsn, "u", "", "superuser", "r"},
	} {
		if _, _, err := run(tc.dsn, tc.userID, tc.email, tc.roles, tc.reason, 5*time.Second); err == nil {
			t.Errorf("%s: must fail", tc.name)
		}
	}
}

func TestBootstrapRejectsMissingUser(t *testing.T) {
	setupBootstrapDB(t)
	dsn := bootstrapTestDSN()
	missing := "99999999-9999-4999-8999-999999999999"
	if _, _, err := run(dsn, missing, "", "admin", "r", 10*time.Second); err == nil {
		t.Fatal("granting to a nonexistent user must fail")
	}
}

func TestBootstrapResolvesByEmail(t *testing.T) {
	db, userID := setupBootstrapDB(t)
	dsn := bootstrapTestDSN()
	email := fmt.Sprintf("bootstrap-%s@example.com", userID[:8])
	if _, err := db.Exec(
		`INSERT INTO social_identities (user_id, provider, provider_uid, email)
		 VALUES ($1, 'github', $2, $3)`, userID, "gh-"+userID, email); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	_, target, err := run(dsn, "", email, "admin", "email bootstrap", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if target != userID {
		t.Fatalf("resolved %s, want %s", target, userID)
	}
}

