package service

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// TestBackfillAppSecrets_NoUnhashedApps is the idempotent path — every row
// already has a hash, the loop is a no-op, and we return 0.
func TestBackfillAppSecrets_NoUnhashedApps(t *testing.T) {
	t.Parallel()
	mr := newMockAppRepo()
	mr.seedActive("yundian", "Yundian")
	mr.apps["yundian"].SecretHash = "H_PRESEEDED"

	n, err := BackfillAppSecrets(context.Background(), mr)
	if err != nil {
		t.Fatalf("BackfillAppSecrets: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 apps backfilled, got %d", n)
	}
	if mr.apps["yundian"].SecretHash != "H_PRESEEDED" {
		t.Errorf("pre-seeded hash should not be overwritten, got %q", mr.apps["yundian"].SecretHash)
	}
}

// TestBackfillAppSecrets_OneUnhashedApp covers the happy path — one row
// without a hash gets a freshly-generated one. Returns 1 and the row is
// now populated.
func TestBackfillAppSecrets_OneUnhashedApp(t *testing.T) {
	t.Parallel()
	mr := newMockAppRepo()
	mr.seedActive("yundian", "Yundian")
	// SecretHash is empty (zero value) — ListUnhashed will return it.

	n, err := BackfillAppSecrets(context.Background(), mr)
	if err != nil {
		t.Fatalf("BackfillAppSecrets: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 app backfilled, got %d", n)
	}
	if mr.apps["yundian"].SecretHash == "" {
		t.Error("expected SecretHash to be populated after backfill")
	}
	if len(mr.apps["yundian"].SecretHash) < 50 {
		t.Errorf("expected bcrypt-shaped hash (>=50 chars), got %d chars", len(mr.apps["yundian"].SecretHash))
	}
}

// TestBackfillAppSecrets_MultipleUnhashed verifies the loop counts every
// newly-hashed row (not just the last one).
func TestBackfillAppSecrets_MultipleUnhashed(t *testing.T) {
	t.Parallel()
	mr := newMockAppRepo()
	mr.seedActive("a", "App A")
	mr.seedActive("b", "App B")
	mr.seedActive("c", "App C")
	// all have empty SecretHash

	n, err := BackfillAppSecrets(context.Background(), mr)
	if err != nil {
		t.Fatalf("BackfillAppSecrets: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 apps backfilled, got %d", n)
	}
	for _, id := range []string{"a", "b", "c"} {
		if mr.apps[id].SecretHash == "" {
			t.Errorf("app %q should have a hash after backfill", id)
		}
	}
}

// TestBackfillAppSecrets_ListUnhashedError propagates errors from the
// underlying repo. The function returns 0 and the original error.
// Uses a thin stub around *mockAppRepo to inject the error.
func TestBackfillAppSecrets_ListUnhashedError(t *testing.T) {
	t.Parallel()
	mr := newMockAppRepo()
	mr.seedActive("yundian", "Yundian")
	ar := &errAppRepo{inner: mr, listUnhashedErr: errTestType{}}
	n, err := BackfillAppSecrets(context.Background(), ar)
	if err == nil {
		t.Fatal("expected error from ListUnhashed, got nil")
	}
	if n != 0 {
		t.Errorf("expected 0 on error, got %d", n)
	}
}

// errAppRepo wraps *mockAppRepo and lets the test inject errors on
// specific methods (the mock itself only exposes createErr / findErr
// etc., not listUnhashedErr).
type errAppRepo struct {
	inner          *mockAppRepo
	listUnhashedErr error
}

func (a *errAppRepo) Create(ctx context.Context, x *model.App) error {
	return a.inner.Create(ctx, x)
}
func (a *errAppRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	return a.inner.FindByID(ctx, id)
}
func (a *errAppRepo) Update(ctx context.Context, x *model.App) error {
	return a.inner.Update(ctx, x)
}
func (a *errAppRepo) List(ctx context.Context) ([]model.App, error) {
	return a.inner.List(ctx)
}
func (a *errAppRepo) ListUnhashed(ctx context.Context) ([]model.App, error) {
	return nil, a.listUnhashedErr
}
func (a *errAppRepo) RotateSecretHash(ctx context.Context, appID, newHash string) error {
	return a.inner.RotateSecretHash(ctx, appID, newHash)
}
func (a *errAppRepo) BackfillSecretHash(ctx context.Context, appID, newHash string) (bool, error) {
	return a.inner.BackfillSecretHash(ctx, appID, newHash)
}

// compile-time check
var _ repo.AppRepo = (*errAppRepo)(nil)
