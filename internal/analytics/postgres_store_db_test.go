package analytics

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/yunhou/users/internal/migrate"
)

// outboxTestDB connects to the shared test Postgres (same DATABASE_URL
// convention as internal/service DB tests) and makes sure the real
// migrations — including 046_analytics_outbox — are applied.
func outboxTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost/yunhou_users?sslmode=disable"
	}
	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })

	migs, err := migrate.LoadFiles("../../migrations")
	if err != nil {
		t.Skipf("no migrations dir (%v)", err)
	}
	if _, _, err := migrate.Apply(context.Background(), db, migs); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func TestPostgresStore_SaveFailed_RejectsEmptyUUID(t *testing.T) {
	// The guard fires before the DB is touched, so a nil pool is fine — no
	// postgres needed for this one.
	store := NewPostgresStore(nil)
	err := store.SaveFailed(context.Background(), Event{Name: "purchase_completed", DistinctID: "u-1"})
	if err == nil {
		t.Fatal("SaveFailed with empty UUID: want error, got nil")
	}
}

func TestPostgresStore_SaveFailed_IsIdempotentOnUUID(t *testing.T) {
	db := outboxTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM analytics_outbox`); err != nil {
		t.Fatalf("clean outbox: %v", err)
	}
	store := NewPostgresStore(db)

	evt := Event{
		Name:       "purchase_completed",
		DistinctID: "user-outbox-1",
		UUID:       EventUUID("purchase:outbox-txn-1"),
		Timestamp:  time.Now().UTC().Truncate(time.Second),
		Properties: map[string]any{"amount": 19.9, "attribution": map[string]any{"first_touch": map[string]any{"utm_source": "google"}}},
	}
	// The same event failing twice must produce ONE row (uuid = idempotency key).
	if err := store.SaveFailed(ctx, evt); err != nil {
		t.Fatalf("SaveFailed: %v", err)
	}
	if err := store.SaveFailed(ctx, evt); err != nil {
		t.Fatalf("SaveFailed again: %v", err)
	}

	var n int
	if err := db.GetContext(ctx, &n, `SELECT COUNT(*) FROM analytics_outbox WHERE uuid = $1`, evt.UUID); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows for uuid = %d, want 1 (INSERT ON CONFLICT DO NOTHING)", n)
	}
}

func TestPostgresStore_RoundTrip_DueMarkSentReschedule(t *testing.T) {
	db := outboxTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM analytics_outbox`); err != nil {
		t.Fatalf("clean outbox: %v", err)
	}
	store := NewPostgresStore(db)

	now := time.Now().UTC().Truncate(time.Second)
	evt := Event{
		Name:       "purchase_completed",
		DistinctID: "user-outbox-2",
		UUID:       EventUUID("purchase:outbox-txn-2"),
		Timestamp:  now,
		Properties: map[string]any{"amount": 199.9, "purchase_type": "renewal"},
	}
	if err := store.SaveFailed(ctx, evt); err != nil {
		t.Fatalf("SaveFailed: %v", err)
	}

	// Due immediately: round-trips with the payload semantically equal
	// (JSONB normalizes formatting; UUID and properties must survive).
	due, err := store.ListDue(ctx, 50)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	var got *OutboxEvent
	for i := range due {
		if due[i].UUID == evt.UUID {
			got = &due[i]
		}
	}
	if got == nil {
		t.Fatalf("saved event not due; due batch = %+v", due)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", got.Attempts)
	}
	if !got.Timestamp.Equal(now) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, now)
	}
	props, err := json.Marshal(got.Properties)
	if err != nil {
		t.Fatalf("marshal round-tripped props: %v", err)
	}
	wantProps, _ := json.Marshal(evt.Properties)
	if string(props) != string(wantProps) {
		t.Errorf("properties = %s, want %s", props, wantProps)
	}

	// Reschedule: leaves the due set until the backoff elapses.
	next := time.Now().Add(time.Hour).UTC()
	if err := store.Reschedule(ctx, evt.UUID, 1, next); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	due, err = store.ListDue(ctx, 50)
	if err != nil {
		t.Fatalf("ListDue after reschedule: %v", err)
	}
	for _, e := range due {
		if e.UUID == evt.UUID {
			t.Fatalf("rescheduled event still due (next=%v)", next)
		}
	}
	var attempts int
	if err := db.GetContext(ctx, &attempts,
		`SELECT attempts FROM analytics_outbox WHERE uuid = $1`, evt.UUID); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}

	// MarkSent: excluded from due scans afterwards.
	if err := store.MarkSent(ctx, evt.UUID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	var sentAt *time.Time
	if err := db.GetContext(ctx, &sentAt,
		`SELECT sent_at FROM analytics_outbox WHERE uuid = $1`, evt.UUID); err != nil {
		t.Fatalf("read sent_at: %v", err)
	}
	if sentAt == nil {
		t.Error("sent_at is NULL after MarkSent")
	}
}

func TestPostgresStore_ListDue_DeadLettersUndecodablePayload(t *testing.T) {
	db := outboxTestDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM analytics_outbox`); err != nil {
		t.Fatalf("clean outbox: %v", err)
	}
	store := NewPostgresStore(db)

	// A healthy row and a tampered one (type mismatch: Name is a string in
	// Event but a number here). ListDue must deliver the healthy row and
	// park the bad one past every real timestamp, not abort the batch.
	good := Event{
		Name:       "purchase_completed",
		DistinctID: "user-outbox-good",
		UUID:       EventUUID("purchase:outbox-good"),
		Timestamp:  time.Now().UTC().Truncate(time.Second),
	}
	if err := store.SaveFailed(ctx, good); err != nil {
		t.Fatalf("SaveFailed good: %v", err)
	}
	badUUID := EventUUID("purchase:outbox-bad")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO analytics_outbox (uuid, event_name, distinct_id, payload)
		VALUES ($1, 'purchase_completed', 'user-outbox-bad', '{"Name": 123}')
	`, badUUID); err != nil {
		t.Fatalf("insert tampered row: %v", err)
	}

	due, err := store.ListDue(ctx, 50)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(due) != 1 || due[0].UUID != good.UUID {
		t.Fatalf("due = %+v, want only the healthy row", due)
	}
	var parked bool
	if err := db.GetContext(ctx, &parked, `
		SELECT next_attempt_at > now() + interval '100 years'
		FROM analytics_outbox WHERE uuid = $1
	`, badUUID); err != nil {
		t.Fatalf("read parked state: %v", err)
	}
	if !parked {
		t.Error("undecodable row not dead-lettered")
	}

	// A later scan no longer surfaces the bad row at all.
	due, err = store.ListDue(ctx, 50)
	if err != nil {
		t.Fatalf("second ListDue: %v", err)
	}
	for _, e := range due {
		if e.UUID == badUUID {
			t.Fatal("dead-lettered row surfaced again")
		}
	}
}
