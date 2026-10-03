package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestInsertMembershipSubConflictKeepsTxUsable: a 23505 on the partial
// unique index (concurrent grant won idx_subscriptions_user_product_active)
// must not abort the Postgres transaction. InsertMembershipSub rolls back
// to its savepoint, so the vip.reject audit row the service writes next
// commits in the same tx (I-4: 409 + audit instead of a 500 with nothing
// recorded). Without the savepoint the InsertAudit below would fail with
// "current transaction is aborted".
func TestInsertMembershipSubConflictKeepsTxUsable(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	uid := uuid.NewString()

	if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, expires_at, product_code)
		VALUES ($1, 'monthly', 'active', now() + interval '30 days', 'kaya-membership')
	`, uid); err != nil {
		t.Fatalf("seed active membership: %v", err)
	}

	r := NewAdminUsersRepo(db)
	target := "user:" + uid
	err := r.WithTx(ctx, func(tx AdminUsersTx) error {
		if _, _, err := tx.InsertMembershipSub(ctx, uid, 30); !errors.Is(err, ErrAdminSubscriptionConflict) {
			return fmt.Errorf("insert conflict: got %v, want ErrAdminSubscriptionConflict", err)
		}
		return tx.InsertAudit(ctx, "admin:yundash", "vip.reject", target,
			map[string]any{"days": 30, "reject_reason": "concurrent grant conflict"})
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	// The conflicting INSERT rolled back to its savepoint: still exactly
	// one active membership row, owned by the original seed.
	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT count(*) FROM subscriptions WHERE user_id = $1`, uid); err != nil || n != 1 {
		t.Fatalf("subscriptions count = %d (err %v), want 1", n, err)
	}
	var auditN int
	if err := db.GetContext(ctx, &auditN,
		`SELECT count(*) FROM audit_log WHERE action = 'vip.reject' AND target = $1`, target); err != nil || auditN != 1 {
		t.Fatalf("reject audit count = %d (err %v), want 1", auditN, err)
	}
}

// TestIdempotencyRecordRoundTrip: the payload digest written with the key
// (migration 038) comes back on the replay read; rows predating 038 have
// NULL request_hash and must read back as nil so the service skips the
// payload check for them.
func TestIdempotencyRecordRoundTrip(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	r := NewAdminUsersRepo(db)
	// Fixed keys below: clear prior runs (setupDB does not truncate this
	// 037 table).
	if _, err := db.ExecContext(ctx, `DELETE FROM admin_idempotency_keys WHERE app_id = 'yundash'`); err != nil {
		t.Fatalf("clear idempotency keys: %v", err)
	}

	const hash = "deadbeef"
	err := r.WithTx(ctx, func(tx AdminUsersTx) error {
		inserted, err := tx.InsertIdempotencyKey(ctx, "yundash", "k-hash", "vip.grant", "user:"+uuid.NewString(), hash,
			json.RawMessage(`{"action":"granted"}`))
		if err != nil || !inserted {
			t.Fatalf("insert: inserted=%v err=%v", inserted, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}

	rec, err := r.GetIdempotencyRecord(ctx, "yundash", "k-hash")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// jsonb normalizes the payload (whitespace), so compare decoded.
	var decoded struct {
		Action string `json:"action"`
	}
	if rec == nil || json.Unmarshal(rec.Response, &decoded) != nil || decoded.Action != "granted" {
		t.Fatalf("record: %+v", rec)
	}
	if rec.RequestHash == nil || *rec.RequestHash != hash {
		t.Fatalf("request hash: %+v, want %q", rec.RequestHash, hash)
	}

	// Legacy 037 row (no digest): NULL comes back as nil.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO admin_idempotency_keys (app_id, key, action, target, response)
		VALUES ('yundash', 'k-legacy', 'vip.grant', 'user:x', '{"action":"granted"}'::jsonb)
	`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	legacy, err := r.GetIdempotencyRecord(ctx, "yundash", "k-legacy")
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if legacy == nil || legacy.RequestHash != nil {
		t.Fatalf("legacy record: %+v, want nil hash", legacy)
	}
}

// TestExtendMembershipSub pins the extend semantics in both directions:
// finite rows extend from max(expires_at, now()); lifetime rows
// (expires_at NULL) must stay NULL — GREATEST alone would treat NULL as
// -inf and silently rewrite a lifetime membership into a finite one.
func TestExtendMembershipSub(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	r := NewAdminUsersRepo(db)

	seed := func(t *testing.T, expiresAt interface{}) string {
		t.Helper()
		uid := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, uid); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO subscriptions (user_id, plan_id, status, expires_at, product_code)
			VALUES ($1, 'monthly', 'active', $2, 'kaya-membership')
		`, uid, expiresAt); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
		return uid
	}

	t.Run("lifetime row stays NULL", func(t *testing.T) {
		uid := seed(t, nil)
		err := r.WithTx(ctx, func(tx AdminUsersTx) error {
			updated, planID, expiresAt, err := tx.ExtendMembershipSub(ctx, uid, 30)
			if err != nil {
				return err
			}
			if !updated || planID != "monthly" {
				t.Errorf("updated=%v planID=%q, want true/monthly", updated, planID)
			}
			if !expiresAt.IsZero() {
				t.Errorf("expiresAt = %v, want zero (lifetime preserved)", expiresAt)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		var isNull bool
		if err := db.GetContext(ctx, &isNull,
			`SELECT expires_at IS NULL FROM subscriptions WHERE user_id = $1`, uid); err != nil || !isNull {
			t.Fatalf("expires_at IS NULL = %v (err %v), want true", isNull, err)
		}
	})

	t.Run("finite row extends from expiry", func(t *testing.T) {
		base := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Microsecond)
		uid := seed(t, base)
		var got time.Time
		err := r.WithTx(ctx, func(tx AdminUsersTx) error {
			updated, _, expiresAt, err := tx.ExtendMembershipSub(ctx, uid, 30)
			if err != nil {
				return err
			}
			if !updated {
				t.Error("updated = false, want true")
			}
			got = expiresAt
			return nil
		})
		if err != nil {
			t.Fatalf("tx: %v", err)
		}
		want := base.Add(30 * 24 * time.Hour)
		if !got.Equal(want) {
			t.Errorf("expiresAt = %v, want %v (base + 30d)", got, want)
		}
	})
}
