// Package migrate is the migration-ledger binary used by cmd/migrate and
// by the test suite. The package is deliberately small: it owns the
// _migrations table, applies *.sql files in lexicographic filename order,
// and records each successfully applied file in the same transaction as
// the SQL it ran. If the SQL fails, the ledger row is rolled back too —
// there is never a half-applied state.
//
// Design contract (mirrors yunhou-deploy/PROGRESS.md §A1):
//   - Each file's filename (minus the .sql extension) is the migration
//     ID. Example: 001_init.sql → id "001_init".
//   - The ledger table is _migrations(id TEXT PRIMARY KEY, applied_at
//     TIMESTAMPTZ NOT NULL DEFAULT now()).
//   - Idempotent: re-running Apply on a clean DB is a no-op (skipped++).
//   - Files are loaded from disk at call time (no //go:embed) so the
//     cmd/migrate binary doesn't need a ../../migrations path that
//     embed rules forbid. Dockerfile COPY /migrations at runtime
//     instead.
package migrate

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ledgerDDL is the schema for the migration ledger. We use IF NOT EXISTS
// so Apply is safe to run on a fresh DB and on a DB where _migrations
// already exists from a prior run.
const ledgerDDL = `
CREATE TABLE IF NOT EXISTS _migrations (
    id          TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// Migration is one SQL file loaded from disk. ID is the filename minus
// .sql (e.g. "001_init"); SQL is the file's contents verbatim.
type Migration struct {
	ID  string
	SQL string
}

// LoadFiles scans dir for *.sql files, sorts them lexicographically
// (which matches the NNN_ prefix ordering), and reads each file into a
// Migration. Files that cannot be read return an error — silent skip
// would let a partial state ship.
func LoadFiles(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	out := make([]Migration, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("open %q: %w", p, err)
		}
		body, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", p, err)
		}
		id := strings.TrimSuffix(filepath.Base(p), ".sql")
		out = append(out, Migration{ID: id, SQL: string(body)})
	}
	return out, nil
}

// Apply runs every migration that has not yet been recorded in
// _migrations. Each migration runs in its own transaction alongside its
// ledger INSERT — a failure rolls back BOTH, so the ledger never
// contains an id whose SQL didn't complete.
//
// Returns (applied, skipped, err). On err the migration that failed is
// not recorded; subsequent migrations are not attempted.
func Apply(ctx context.Context, db *sqlx.DB, migrations []Migration) (applied, skipped int, err error) {
	if _, err := db.ExecContext(ctx, ledgerDDL); err != nil {
		return 0, 0, fmt.Errorf("create ledger: %w", err)
	}

	// pg_advisory_lock so two concurrent `cmd/migrate` invocations
	// (e.g. a rolling deploy where two replicas race to apply the same
	// pending migration) serialise on the lock instead of both reading
	// the empty ledger, both attempting the same INSERT INTO _migrations,
	// and one tripping the unique-key constraint aborting the entire
	// batch. The lock auto-releases on COMMIT/ROLLBACK of the wrapping
	// tx — no manual unlock needed.
	lockTx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin lock tx: %w", err)
	}
	if _, err := lockTx.ExecContext(ctx, `SELECT pg_advisory_lock(7243817)`); err != nil {
		_ = lockTx.Rollback()
		return 0, 0, fmt.Errorf("acquire migrate advisory lock: %w", err)
	}
	// We can't COMMIT the lock-tx because that releases the lock. Hold
	// it open for the duration of Apply; release on every return path
	// below via the defer-on-lockTx wrapper. Errors abort with ROLLBACK,
	// success path commits AFTER the last applyOne.
	committed := false
	defer func() {
		if !committed {
			_ = lockTx.Rollback()
		}
	}()

	rows, err := lockTx.QueryContext(ctx, `SELECT id FROM _migrations`)
	if err != nil {
		return 0, 0, fmt.Errorf("select applied: %w", err)
	}
	appliedSet := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan applied id: %w", err)
		}
		appliedSet[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("iterate applied ids: %w", err)
	}
	rows.Close()

	for _, m := range migrations {
		if _, ok := appliedSet[m.ID]; ok {
			skipped++
			log.Printf("[migrate] skip %s (already applied)", m.ID)
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return applied, skipped, fmt.Errorf("apply %s: %w", m.ID, err)
		}
		applied++
		log.Printf("[migrate] applied %s", m.ID)
	}
	if err := lockTx.Commit(); err != nil {
		return applied, skipped, fmt.Errorf("release migrate advisory lock: %w", err)
	}
	committed = true
	return applied, skipped, nil
}

// applyOne runs m.SQL and the ledger INSERT in a single transaction.
// On any error the transaction is rolled back so the ledger never
// contains a half-applied id.
func applyOne(ctx context.Context, db *sqlx.DB, m Migration) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Rollback is a no-op if Commit succeeded; safe to call defer'd.
	defer func() {
		_ = tx.Rollback()
	}()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _migrations (id) VALUES ($1)`, m.ID); err != nil {
		return fmt.Errorf("insert ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Status writes a human-readable table to stdout: one row per migration,
// prefixed with ✅ for already-applied and ⏳ for pending. The format
// is intentionally plain-text so CI logs and shell pipelines can grep
// it directly.
func Status(ctx context.Context, db *sqlx.DB, migrations []Migration) error {
	if _, err := db.ExecContext(ctx, ledgerDDL); err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM _migrations`)
	if err != nil {
		return fmt.Errorf("select applied: %w", err)
	}
	appliedSet := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied id: %w", err)
		}
		appliedSet[id] = struct{}{}
	}
	rows.Close()

	if len(migrations) == 0 {
		log.Print("[migrate] no migrations found")
		return nil
	}
	for _, m := range migrations {
		if _, ok := appliedSet[m.ID]; ok {
			log.Printf("✅ %s", m.ID)
		} else {
			log.Printf("⏳ %s", m.ID)
		}
	}
	return nil
}
