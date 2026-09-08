package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// Real-DB tests for billing_account_repo.go / apikey_repo.go (Task 5).
// Run against the disposable instance; skip is NOT a pass.

func TestEnsureBillingAccount_Idempotent(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	userID := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	a1, err := s.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("ensure #1: %v", err)
	}
	a2, err := s.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("ensure #2: %v", err)
	}
	if a1.ID != a2.ID {
		t.Fatalf("two accounts for one user: %s vs %s", a1.ID, a2.ID)
	}

	// Concurrent establishment loses no row (ON CONFLICT DO NOTHING).
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := s.EnsureBillingAccount(ctx, userID)
			if err != nil {
				t.Errorf("concurrent ensure: %v", err)
				return
			}
			ids[i] = a.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != a1.ID {
			t.Errorf("concurrent ensure produced a second account: %s", id)
		}
	}
	var count int
	if err := s.db.GetContext(ctx, &count,
		`SELECT count(*) FROM inference_billing_accounts WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("account rows = %d, want 1", count)
	}
}

func TestGetBillingAccountByID(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	userID := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	a, err := s.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got, err := s.GetBillingAccountByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != userID || got.Status != "active" {
		t.Errorf("wrong account: %+v", got)
	}
	if _, err := s.GetBillingAccountByID(ctx, uuid.NewString()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("missing id: want not_found, got %v", err)
	}
}

func TestAPIKeyRepo_ListGetUpdateRevoke(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	// Three keys on the one account.
	for _, name := range []string{"k1", "k2", "k3"} {
		k := &domain.APIKey{BillingAccountID: f.accountID, Name: name, Prefix: "yk-test-" + uuid.NewString()[:9]}
		if err := s.InsertAPIKey(ctx, k, "sha256:"+uuid.NewString()); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	// Newest first; page through with limit 2.
	page1, total, err := s.ListAPIKeysByAccount(ctx, f.accountID, 2, 0)
	if err != nil {
		t.Fatalf("list p1: %v", err)
	}
	if total != 3 || len(page1) != 2 {
		t.Fatalf("page1: total=%d len=%d", total, len(page1))
	}
	page2, _, err := s.ListAPIKeysByAccount(ctx, f.accountID, 2, 2)
	if err != nil {
		t.Fatalf("list p2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len=%d", len(page2))
	}
	if page1[0].Name != "k3" || page2[0].Name != "k1" {
		t.Errorf("ordering wrong: p1[0]=%s p2[0]=%s", page1[0].Name, page2[0].Name)
	}

	// GetAPIKeyByID returns the management view.
	got, err := s.GetAPIKeyByID(ctx, page1[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "k3" || got.Status != domain.APIKeyActive {
		t.Errorf("wrong key: %+v", got)
	}
	if _, err := s.GetAPIKeyByID(ctx, uuid.NewString()); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("missing: want not_found, got %v", err)
	}

	// Update management fields; status is NOT writable here.
	rpm := 60
	budget := domain.Microcredit(7_500_000)
	got.Name = "k3-renamed"
	got.ModelAllow = []string{f.modelID}
	got.BudgetLimit = &budget
	got.RPMLimit = &rpm
	if err := s.UpdateAPIKey(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	back, err := s.GetAPIKeyByID(ctx, got.ID)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if back.Name != "k3-renamed" || len(back.ModelAllow) != 1 ||
		back.BudgetLimit == nil || *back.BudgetLimit != budget ||
		back.RPMLimit == nil || *back.RPMLimit != 60 {
		t.Errorf("update not persisted: %+v", back)
	}
	// budget_used stays under quota-path control only.
	if back.BudgetUsed != 0 {
		t.Errorf("budget_used changed by management update: %d", back.BudgetUsed)
	}
	got.ID = uuid.NewString()
	if err := s.UpdateAPIKey(ctx, got); domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("update missing: want not_found, got %v", err)
	}

	// Revoke: first flip reports changed=true, second is a no-op.
	changed, err := s.RevokeAPIKey(ctx, back.ID, nowUTC())
	if err != nil || !changed {
		t.Fatalf("revoke #1: changed=%v err=%v", changed, err)
	}
	changed, err = s.RevokeAPIKey(ctx, back.ID, nowUTC())
	if err != nil || changed {
		t.Fatalf("revoke #2 must be idempotent: changed=%v err=%v", changed, err)
	}
	back, _ = s.GetAPIKeyByID(ctx, back.ID)
	if back.Status != domain.APIKeyRevoked || back.RevokedAt == nil {
		t.Errorf("revoke not persisted: %+v", back)
	}
}

func TestAPIKeyRepo_ExpiryAndLastUsed(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	past := nowUTC().Add(-time.Hour)
	future := nowUTC().Add(time.Hour)
	expired := &domain.APIKey{BillingAccountID: f.accountID, Prefix: "yk-test-" + uuid.NewString()[:9], ExpiresAt: &past}
	live := &domain.APIKey{BillingAccountID: f.accountID, Prefix: "yk-test-" + uuid.NewString()[:9], ExpiresAt: &future}
	for _, k := range []*domain.APIKey{expired, live} {
		if err := s.InsertAPIKey(ctx, k, "sha256:"+uuid.NewString()); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := s.MarkAPIKeyExpired(ctx, expired.ID); err != nil {
		t.Fatalf("mark expired: %v", err)
	}
	if err := s.MarkAPIKeyExpired(ctx, live.ID); err != nil {
		t.Fatalf("mark live: %v", err)
	}
	got, _ := s.GetAPIKeyByID(ctx, expired.ID)
	if got.Status != domain.APIKeyExpired {
		t.Errorf("expired key status = %s", got.Status)
	}
	got, _ = s.GetAPIKeyByID(ctx, live.ID)
	if got.Status != domain.APIKeyActive {
		t.Errorf("live key flipped: %s", got.Status)
	}

	at := nowUTC()
	if err := s.TouchAPIKeyLastUsed(ctx, live.ID, at); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ = s.GetAPIKeyByID(ctx, live.ID)
	if got.LastUsedAt == nil {
		t.Error("last_used_at not set")
	}
}

// TestAPIKeys_ShareAccountPool pins the acceptance rule: creating
// multiple keys never adds to the account's entitlement/quota pool — the
// account row, its entitlements and any quota windows are untouched, and
// each key's budget is an independent sub-budget starting at zero.
func TestAPIKeys_ShareAccountPool(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, false)

	count := func(q string, args ...interface{}) int {
		var n int
		if err := db.GetContext(ctx, &n, q, args...); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	entsBefore := count(`SELECT count(*) FROM inference_entitlements WHERE billing_account_id = $1`, f.accountID)
	windowsBefore := count(`SELECT count(*) FROM inference_quota_windows`)

	for i := 0; i < 3; i++ {
		b := domain.Microcredit(int64(1_000_000 * (i + 1)))
		k := &domain.APIKey{
			BillingAccountID: f.accountID, Prefix: "yk-test-" + uuid.NewString()[:9],
			BudgetLimit: &b,
		}
		if err := s.InsertAPIKey(ctx, k, "sha256:"+uuid.NewString()); err != nil {
			t.Fatalf("insert key %d: %v", i, err)
		}
	}

	if got := count(`SELECT count(*) FROM inference_entitlements WHERE billing_account_id = $1`, f.accountID); got != entsBefore {
		t.Errorf("entitlement pool grew: %d → %d", entsBefore, got)
	}
	if got := count(`SELECT count(*) FROM inference_quota_windows`); got != windowsBefore {
		t.Errorf("quota windows changed: %d → %d", windowsBefore, got)
	}
	keys, total, err := s.ListAPIKeysByAccount(ctx, f.accountID, 50, 0)
	if err != nil || total != 3 {
		t.Fatalf("list: total=%d err=%v", total, err)
	}
	for _, k := range keys {
		if k.BudgetUsed != 0 {
			t.Errorf("key %s budget_used = %d, want 0", k.Name, k.BudgetUsed)
		}
	}
	// The account itself is unchanged beyond updated_at not even that:
	// still exactly one active row.
	if got := count(`SELECT count(*) FROM inference_billing_accounts WHERE id = $1 AND status = 'active'`, f.accountID); got != 1 {
		t.Errorf("account row count = %d", got)
	}
}

func nowUTC() time.Time { return time.Now().UTC() }
