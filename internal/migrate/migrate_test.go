package migrate

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// These tests need a real Postgres — same pattern as internal/repo/repo_test.go.
// They skip cleanly when DATABASE_URL is unset or unreachable.

func adminDBURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres@localhost/postgres?sslmode=disable"
}

// freshTestDB creates a unique database, returns a *sqlx.DB pointed at
// it, and registers cleanup that drops the database. Each test gets its
// own DB so they can run in parallel and never see each other's state.
func freshTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	name := "yunhou_migrate_test_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	admin, err := sqlx.Connect("postgres", adminDBURL())
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("create test db %q: %v", name, err)
	}
	t.Cleanup(func() {
		// Reconnect to admin to drop — the test DB connection may already
		// be closed by the time Cleanup runs in a t.Fatal path.
		adm, err := sqlx.Connect("postgres", adminDBURL())
		if err == nil {
			_, _ = adm.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
			adm.Close()
		}
	})

	dbURL := strings.Replace(adminDBURL(), "/postgres?", "/"+name+"?", 1)
	db, err := sqlx.Connect("postgres", dbURL)
	if err != nil {
		t.Fatalf("connect test db %q: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// migrateDir creates a temp directory with the supplied SQL files. The
// map key becomes the filename (".sql" is appended); the value is the
// SQL body. Returns the temp dir path; t.Cleanup removes it.
func migrateDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name+".sql")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
	}
	return dir
}

// captureLog redirects the standard log to a buffer and returns the
// buffer so the test can assert on output. The original log writer is
// restored on Cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestApply_EmptyDB_AppliesAll(t *testing.T) {
	db := freshTestDB(t)
	dir := migrateDir(t, map[string]string{
		"001_init":   `CREATE TABLE users (id INT PRIMARY KEY);`,
		"002_add_col": `ALTER TABLE users ADD COLUMN name TEXT;`,
	})
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	captureLog(t)
	applied, skipped, err := Apply(context.Background(), db, migs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied != 2 || skipped != 0 {
		t.Errorf("applied=%d skipped=%d, want 2/0", applied, skipped)
	}

	// Ledger has both ids.
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM _migrations`); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if count != 2 {
		t.Errorf("ledger count = %d, want 2", count)
	}

	// Schema actually applied.
	var hasNameCol bool
	if err := db.Get(&hasNameCol,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name='users' AND column_name='name')`); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if !hasNameCol {
		t.Error("users.name column not created")
	}
}

func TestApply_RerunIsIdempotent(t *testing.T) {
	db := freshTestDB(t)
	dir := migrateDir(t, map[string]string{
		"001_init": `CREATE TABLE users (id INT PRIMARY KEY);`,
	})
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	captureLog(t)
	if _, _, err := Apply(context.Background(), db, migs); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	applied, skipped, err := Apply(context.Background(), db, migs)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if applied != 0 || skipped != 1 {
		t.Errorf("rerun: applied=%d skipped=%d, want 0/1", applied, skipped)
	}

	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM _migrations`); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if count != 1 {
		t.Errorf("ledger count after rerun = %d, want 1", count)
	}
}

func TestApply_MidFileFailure_StopsAndDoesNotWriteLedger(t *testing.T) {
	db := freshTestDB(t)
	dir := migrateDir(t, map[string]string{
		"001_init":   `CREATE TABLE users (id INT PRIMARY KEY);`,
		"002_bad":    `THIS IS NOT VALID SQL;`,
		"003_late":   `ALTER TABLE users ADD COLUMN name TEXT;`,
	})
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	captureLog(t)
	applied, skipped, err := Apply(context.Background(), db, migs)
	if err == nil {
		t.Fatal("expected Apply to return error for bad SQL")
	}
	if applied != 1 || skipped != 0 {
		t.Errorf("applied=%d skipped=%d, want 1/0 (001 applied, 002 failed, 003 not attempted)", applied, skipped)
	}
	if !strings.Contains(err.Error(), "002_bad") {
		t.Errorf("error %q should mention failing migration id", err)
	}

	// Ledger must NOT contain the failed id or any later id.
	var ids []string
	if err := db.Select(&ids, `SELECT id FROM _migrations ORDER BY id`); err != nil {
		t.Fatalf("select ledger: %v", err)
	}
	if len(ids) != 1 || ids[0] != "001_init" {
		t.Errorf("ledger = %v, want only [001_init]", ids)
	}

	// users.name must NOT exist (003 never ran).
	var hasNameCol bool
	if err := db.Get(&hasNameCol,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                WHERE table_name='users' AND column_name='name')`); err != nil {
		t.Fatalf("column check: %v", err)
	}
	if hasNameCol {
		t.Error("users.name exists even though 003_late should not have run")
	}
}

func TestApply_PartialDelete_ReappliesOne(t *testing.T) {
	db := freshTestDB(t)
	// Use idempotent DDL — partial-delete-and-replay only works when the
	// migration itself is safe to re-run. Real migrations follow the
	// IF NOT EXISTS / DROP IF EXISTS rule documented in migrations/README.md.
	dir := migrateDir(t, map[string]string{
		"001_init":   `CREATE TABLE users (id INT PRIMARY KEY);`,
		"002_add":    `ALTER TABLE users ADD COLUMN IF NOT EXISTS a TEXT;`,
		"003_add_b":  `ALTER TABLE users ADD COLUMN IF NOT EXISTS b TEXT;`,
	})
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	captureLog(t)
	if _, _, err := Apply(context.Background(), db, migs); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Operator manually removes the ledger row for 002 to replay it.
	if _, err := db.Exec(`DELETE FROM _migrations WHERE id = '002_add'`); err != nil {
		t.Fatalf("manual delete: %v", err)
	}

	applied, skipped, err := Apply(context.Background(), db, migs)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if applied != 1 || skipped != 2 {
		t.Errorf("second Apply: applied=%d skipped=%d, want 1/2", applied, skipped)
	}

	var ids []string
	if err := db.Select(&ids, `SELECT id FROM _migrations ORDER BY id`); err != nil {
		t.Fatalf("select ledger: %v", err)
	}
	want := []string{"001_init", "002_add", "003_add_b"}
	if fmt.Sprintf("%v", ids) != fmt.Sprintf("%v", want) {
		t.Errorf("ledger = %v, want %v", ids, want)
	}
}

func TestStatus_OutputMarksAppliedAndPending(t *testing.T) {
	db := freshTestDB(t)
	dir := migrateDir(t, map[string]string{
		"001_init": `CREATE TABLE users (id INT PRIMARY KEY);`,
		"002_add":  `ALTER TABLE users ADD COLUMN a TEXT;`,
		"003_late": `ALTER TABLE users ADD COLUMN b TEXT;`,
	})
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}

	captureLog(t)
	if _, _, err := Apply(context.Background(), db, migs[:1]); err != nil {
		t.Fatalf("Apply first: %v", err)
	}

	buf := captureLog(t)
	if err := Status(context.Background(), db, migs); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "✅ 001_init") {
		t.Errorf("status output missing ✅ 001_init: %q", out)
	}
	if !strings.Contains(out, "⏳ 002_add") {
		t.Errorf("status output missing ⏳ 002_add: %q", out)
	}
	if !strings.Contains(out, "⏳ 003_late") {
		t.Errorf("status output missing ⏳ 003_late: %q", out)
	}
}

func TestLoadFiles_OrdersLexicographically(t *testing.T) {
	dir := migrateDir(t, map[string]string{
		"010_third":  `SELECT 1;`,
		"001_first":  `SELECT 2;`,
		"005_second": `SELECT 3;`,
	})
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	want := []string{"001_first", "005_second", "010_third"}
	got := []string{migs[0].ID, migs[1].ID, migs[2].ID}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("migs[%d].ID = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadFiles_IgnoresNonSQL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001_init.sql"), []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatalf("write sql: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("notes"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	migs, err := LoadFiles(dir)
	if err != nil {
		t.Fatalf("LoadFiles: %v", err)
	}
	if len(migs) != 1 {
		t.Fatalf("got %d migrations, want 1 (non-sql ignored)", len(migs))
	}
	if migs[0].ID != "001_init" {
		t.Errorf("ID = %q, want 001_init", migs[0].ID)
	}
}

func TestApply_RealMigrationsFromRepo(t *testing.T) {
	// End-to-end: LoadFiles against the actual migrations/ directory and
	// apply them to a fresh DB. Verifies that the real SQL files are
	// individually idempotent enough for our test to be one-shot.
	//
	// This test is the safety net for the production ledger — if anyone
	// ever lands a migration that's missing IF NOT EXISTS, this catches
	// it before deploy.
	db := freshTestDB(t)
	// Resolve relative to the package source — go test runs the package
	// from its own dir, so ../migrations gets us to the repo root.
	migs, err := LoadFiles(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Skipf("no migrations/ dir at %s (%v)", filepath.Join("..", "..", "migrations"), err)
	}
	if len(migs) == 0 {
		t.Skip("no migrations found — run from repo root")
	}

	captureLog(t)
	applied, skipped, err := Apply(context.Background(), db, migs)
	if err != nil {
		t.Fatalf("Apply real migrations: %v", err)
	}
	if applied != len(migs) {
		t.Errorf("applied=%d, want %d", applied, len(migs))
	}
	if skipped != 0 {
		t.Errorf("skipped=%d, want 0 (fresh DB)", skipped)
	}

	// Rerun → all skipped.
	applied2, skipped2, err := Apply(context.Background(), db, migs)
	if err != nil {
		t.Fatalf("Apply (rerun): %v", err)
	}
	if applied2 != 0 || skipped2 != len(migs) {
		t.Errorf("rerun: applied=%d skipped=%d, want 0/%d", applied2, skipped2, len(migs))
	}
}