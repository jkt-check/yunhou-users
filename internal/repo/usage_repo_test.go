package repo

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// These tests run against a real Postgres (setupDB skips when unavailable).
// The usage_events table comes from migration 021; setupDB's
// TRUNCATE ... users CASCADE wipes it between tests.

// seedUsageUser inserts a minimal users row (usage_events.user_id FK) and
// returns its ID. The 'yundian' app row comes from setupDB.
func seedUsageUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	u := &model.User{ID: newUUID(), Status: "active"}
	if err := NewUserRepo(db).Create(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

func usageBeat(localDate string, activeSeconds int, platform, version string) model.UsageBeat {
	return model.UsageBeat{
		ClientEventID: newUUID(),
		OccurredAt:    time.Now(),
		LocalDate:     localDate,
		ActiveSeconds: activeSeconds,
		Platform:      platform,
		AppVersion:    version,
	}
}

const usageTestApp = "yundian" // seeded by setupDB

func TestUsageRepo_InsertBeatsIdempotent(t *testing.T) {
	db := setupDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	uid := seedUsageUser(t, db)

	beats := []model.UsageBeat{
		usageBeat("2026-09-04", 300, "macos", "2.1.14"),
		usageBeat("2026-09-04", 60, "macos", "2.1.14"),
	}
	n, err := r.InsertBeats(ctx, uid, usageTestApp, beats)
	if err != nil {
		t.Fatalf("InsertBeats: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2", n)
	}

	// Replay the identical batch (offline buffer flush / retry): the
	// (user_id, client_event_id) unique constraint must swallow both rows.
	n, err = r.InsertBeats(ctx, uid, usageTestApp, beats)
	if err != nil {
		t.Fatalf("replay InsertBeats: %v", err)
	}
	if n != 0 {
		t.Errorf("replay inserted = %d, want 0", n)
	}

	// The same client_event_id under a DIFFERENT user is a distinct key.
	uid2 := seedUsageUser(t, db)
	n, err = r.InsertBeats(ctx, uid2, usageTestApp, beats)
	if err != nil {
		t.Fatalf("other-user InsertBeats: %v", err)
	}
	if n != 2 {
		t.Errorf("other-user inserted = %d, want 2", n)
	}
}

func TestUsageRepo_CountActiveUsers(t *testing.T) {
	db := setupDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	u1, u2 := seedUsageUser(t, db), seedUsageUser(t, db)

	// u1: two beats on 09-04 + one on 09-03. u2: one beat on 09-04.
	if _, err := r.InsertBeats(ctx, u1, usageTestApp, []model.UsageBeat{
		usageBeat("2026-09-04", 300, "macos", "2.1.14"),
		usageBeat("2026-09-04", 60, "macos", "2.1.14"),
		usageBeat("2026-09-03", 300, "macos", "2.1.14"),
	}); err != nil {
		t.Fatalf("seed u1 beats: %v", err)
	}
	if _, err := r.InsertBeats(ctx, u2, usageTestApp, []model.UsageBeat{
		usageBeat("2026-09-04", 300, "windows", "2.1.13"),
	}); err != nil {
		t.Fatalf("seed u2 beats: %v", err)
	}

	// DAU for 09-04: both users (u1's two beats count once — DISTINCT).
	if n, err := r.CountActiveUsers(ctx, "2026-09-04", "2026-09-04"); err != nil || n != 2 {
		t.Errorf("DAU = %d, err = %v; want 2", n, err)
	}
	// Rolling window covering both days: still 2 distinct users.
	if n, err := r.CountActiveUsers(ctx, "2026-08-29", "2026-09-04"); err != nil || n != 2 {
		t.Errorf("window count = %d, err = %v; want 2", n, err)
	}
	// Window before any beat: 0.
	if n, err := r.CountActiveUsers(ctx, "2026-09-01", "2026-09-02"); err != nil || n != 0 {
		t.Errorf("empty window = %d, err = %v; want 0", n, err)
	}
}

func TestUsageRepo_CountActiveUsersGrouped(t *testing.T) {
	db := setupDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	u1, u2 := seedUsageUser(t, db), seedUsageUser(t, db)

	if _, err := r.InsertBeats(ctx, u1, usageTestApp, []model.UsageBeat{
		usageBeat("2026-09-04", 300, "macos", "2.1.14"),
	}); err != nil {
		t.Fatalf("seed u1: %v", err)
	}
	if _, err := r.InsertBeats(ctx, u2, usageTestApp, []model.UsageBeat{
		usageBeat("2026-09-04", 300, "windows", "2.1.13"),
	}); err != nil {
		t.Fatalf("seed u2: %v", err)
	}

	rows, err := r.CountActiveUsersGrouped(ctx, "2026-09-04", "2026-09-04", "platform")
	if err != nil {
		t.Fatalf("grouped: %v", err)
	}
	got := map[string]int{}
	for _, row := range rows {
		got[row.Group] = row.Users
	}
	if got["macos"] != 1 || got["windows"] != 1 || len(got) != 2 {
		t.Errorf("by platform = %v, want macos:1 windows:1", got)
	}

	rows, err = r.CountActiveUsersGrouped(ctx, "2026-09-04", "2026-09-04", "app_version")
	if err != nil {
		t.Fatalf("grouped by version: %v", err)
	}
	got = map[string]int{}
	for _, row := range rows {
		got[row.Group] = row.Users
	}
	if got["2.1.14"] != 1 || got["2.1.13"] != 1 {
		t.Errorf("by version = %v", got)
	}

	// The column name reaches SQL text — a non-whitelisted value must be
	// rejected before it can become injection.
	if _, err := r.CountActiveUsersGrouped(ctx, "2026-09-04", "2026-09-04", "user_id; DROP TABLE users--"); err == nil {
		t.Error("non-whitelisted group_by must return an error")
	}
}

func TestUsageRepo_UsageDurationStats(t *testing.T) {
	db := setupDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()
	u1, u2 := seedUsageUser(t, db), seedUsageUser(t, db)

	if _, err := r.InsertBeats(ctx, u1, usageTestApp, []model.UsageBeat{
		usageBeat("2026-08-31", 300, "macos", "2.1.14"), // previous month
		usageBeat("2026-09-04", 300, "macos", "2.1.14"),
	}); err != nil {
		t.Fatalf("seed u1: %v", err)
	}
	if _, err := r.InsertBeats(ctx, u2, usageTestApp, []model.UsageBeat{
		usageBeat("2026-09-04", 600, "windows", "2.1.13"),
	}); err != nil {
		t.Fatalf("seed u2: %v", err)
	}

	// Day granularity, cross-month range: beats stay in their own
	// local_date buckets (no cross-midnight split — client-side semantics).
	rows, err := r.UsageDurationStats(ctx, "2026-08-31", "2026-09-04", "day", "")
	if err != nil {
		t.Fatalf("day stats: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("day rows = %v", rows)
	}
	if rows[0].Period != "2026-08-31" || rows[0].TotalSeconds != 300 || rows[0].ActiveUsers != 1 {
		t.Errorf("row[0] = %+v", rows[0])
	}
	if rows[1].Period != "2026-09-04" || rows[1].TotalSeconds != 900 || rows[1].ActiveUsers != 2 {
		t.Errorf("row[1] = %+v", rows[1])
	}

	// Month granularity: the 08-31 beat lands in 2026-08, the rest in 2026-09.
	rows, err = r.UsageDurationStats(ctx, "2026-08-01", "2026-09-30", "month", "")
	if err != nil {
		t.Fatalf("month stats: %v", err)
	}
	if len(rows) != 2 || rows[0].Period != "2026-08" || rows[1].Period != "2026-09" {
		t.Fatalf("month rows = %v", rows)
	}
	if rows[0].TotalSeconds != 300 || rows[1].TotalSeconds != 900 {
		t.Errorf("month totals = %d/%d", rows[0].TotalSeconds, rows[1].TotalSeconds)
	}

	// Grouped by platform within one day.
	rows, err = r.UsageDurationStats(ctx, "2026-09-04", "2026-09-04", "day", "platform")
	if err != nil {
		t.Fatalf("grouped stats: %v", err)
	}
	got := map[string]int64{}
	for _, row := range rows {
		got[row.Group] = row.TotalSeconds
	}
	if got["macos"] != 300 || got["windows"] != 600 {
		t.Errorf("grouped totals = %v", got)
	}
}

func TestUsageRepo_CountNewUsers(t *testing.T) {
	db := setupDB(t)
	r := NewUsageRepo(db)
	ctx := context.Background()

	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	// Empty range → no rows (not an error).
	rows, err := r.CountNewUsers(ctx, yesterday, yesterday)
	if err != nil {
		t.Fatalf("empty range: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("yesterday rows = %v, want none", rows)
	}

	seedUsageUser(t, db)
	seedUsageUser(t, db)

	rows, err = r.CountNewUsers(ctx, today, today)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if len(rows) != 1 || rows[0].Date != today || rows[0].Users != 2 {
		t.Errorf("today rows = %v, want one row with 2 users", rows)
	}
}
