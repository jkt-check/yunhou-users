package migrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
)

// Functional tests for migrations/019_kaya_bridge.sql.
//
// TestApply_RealMigrationsFromRepo applies the real files to a fresh DB,
// but 019's second statement (the jsonb callback_urls append) can never
// match there — fresh DBs have no apps row for yunhou-website, so the
// jsonb logic is never evaluated. These tests seed an apps fixture and
// then execute 019's SQL read straight from the repo file, so the test
// can never drift from the shipped migration.

const migration019Path = "../../migrations/019_kaya_bridge.sql"

const (
	kayaBridgeStagingURL = "https://staging.yunhouai.com/auth/kaya-bridge"
	kayaBridgeProdURL    = "https://yunhouai.com/auth/kaya-bridge"
	preexistingCallback  = "https://yunhouai.com/auth/callback"
)

// applyRealMigrations loads and applies every file under migrations/ so
// the fixture gets the real plans/apps schema and seeded plan rows
// (002: free/monthly/quarterly/yearly; 018: trial).
func applyRealMigrations(t *testing.T, db *sqlx.DB) {
	t.Helper()
	migs, err := LoadFiles(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Skipf("no migrations/ dir (%v)", err)
	}
	if len(migs) == 0 {
		t.Skip("no migrations found — run from repo root")
	}
	captureLog(t)
	if _, _, err := Apply(context.Background(), db, migs); err != nil {
		t.Fatalf("Apply real migrations: %v", err)
	}
}

// execMigration019 reads the shipped 019 file and executes it verbatim.
func execMigration019(t *testing.T, db *sqlx.DB) {
	t.Helper()
	sqlBytes, err := os.ReadFile(migration019Path)
	if err != nil {
		t.Fatalf("read %s: %v", migration019Path, err)
	}
	if _, err := db.Exec(string(sqlBytes)); err != nil {
		t.Fatalf("exec 019: %v", err)
	}
}

// planHasApp reports whether plan `id` has `app` in its apps array.
func planHasApp(t *testing.T, db *sqlx.DB, id, app string) bool {
	t.Helper()
	var has bool
	if err := db.Get(&has,
		`SELECT $2 = ANY(apps) FROM plans WHERE id = $1`, id, app); err != nil {
		t.Fatalf("plan %q apps check: %v", id, err)
	}
	return has
}

// wechatCallbackURLs returns the wechat callback_urls array for
// yunhou-website, or nil when the path is absent.
func wechatCallbackURLs(t *testing.T, db *sqlx.DB) []string {
	t.Helper()
	var raw []byte
	if err := db.Get(&raw,
		`SELECT config #> '{oauth_providers,wechat,callback_urls}'
		   FROM apps WHERE app_id = 'yunhou-website'`); err != nil {
		t.Fatalf("read callback_urls: %v", err)
	}
	if raw == nil {
		return nil
	}
	var urls []string
	if err := json.Unmarshal(raw, &urls); err != nil {
		t.Fatalf("unmarshal callback_urls %q: %v", raw, err)
	}
	return urls
}

func TestMigration019_KayaBridge(t *testing.T) {
	t.Run("adds bridge URLs and grants yunhou-website to full plans", func(t *testing.T) {
		db := freshTestDB(t)
		applyRealMigrations(t, db)

		// Fixture: the yunhou-website app row with one pre-existing
		// wechat callback URL (the website's own callback).
		if _, err := db.Exec(
			`INSERT INTO apps (app_id, name, is_active, config)
			 VALUES ('yunhou-website', 'Yunhou Website', true,
			         '{"oauth_providers":{"wechat":{"callback_urls":["` + preexistingCallback + `"]}}}')`); err != nil {
			t.Fatalf("insert apps fixture: %v", err)
		}

		// Apply() already ran 019 (as a no-op for apps); re-execute it
		// now that the fixture exists so the jsonb path is exercised.
		execMigration019(t, db)

		// Plans: full-function set gains yunhou-website, free does not.
		// quarterly 也在集合内(016 已 is_active=false,不实际授权;
		// 有意包含,复活即为全功能 —— review minor #1 锁定)
		for _, id := range []string{"monthly", "yearly", "trial", "quarterly"} {
			if !planHasApp(t, db, id, "yunhou-website") {
				t.Errorf("plan %q missing yunhou-website in apps", id)
			}
		}
		if planHasApp(t, db, "free", "yunhou-website") {
			t.Error("plan free must NOT gain yunhou-website")
		}

		// callback_urls: original entry first, then staging + prod bridge.
		got := wechatCallbackURLs(t, db)
		want := []string{preexistingCallback, kayaBridgeStagingURL, kayaBridgeProdURL}
		if len(got) != len(want) {
			t.Fatalf("callback_urls = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("callback_urls[%d] = %q, want %q", i, got[i], want[i])
			}
		}

		// Idempotency: re-run the file — URLs must not duplicate and
		// plan rows must not change.
		execMigration019(t, db)
		got = wechatCallbackURLs(t, db)
		if len(got) != len(want) {
			t.Errorf("after rerun callback_urls = %v, want unchanged %v", got, want)
		}
		for _, id := range []string{"monthly", "yearly", "trial"} {
			if !planHasApp(t, db, id, "yunhou-website") {
				t.Errorf("after rerun plan %q lost yunhou-website", id)
			}
		}
		if planHasApp(t, db, "free", "yunhou-website") {
			t.Error("after rerun plan free must still NOT have yunhou-website")
		}
	})

	t.Run("silently skips when wechat block not configured", func(t *testing.T) {
		db := freshTestDB(t)
		applyRealMigrations(t, db)

		// App row exists but config has no wechat block.
		if _, err := db.Exec(
			`INSERT INTO apps (app_id, name, is_active, config)
			 VALUES ('yunhou-website', 'Yunhou Website', true, '{}')`); err != nil {
			t.Fatalf("insert apps fixture: %v", err)
		}

		// The UPDATE must be a no-op and the DO block must only RAISE
		// WARNING (lib/pq surfaces warnings as notices, not errors).
		execMigration019(t, db)

		if got := wechatCallbackURLs(t, db); got != nil {
			t.Errorf("callback_urls = %v, want absent (silent skip)", got)
		}
	})
}
