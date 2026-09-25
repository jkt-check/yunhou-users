package repo

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
