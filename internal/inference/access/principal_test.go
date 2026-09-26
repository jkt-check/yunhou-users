package access

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// mintKey creates an account + entitlement + key in the fake store and
// returns the plaintext (the service path under test never sees it again).
func mintKey(t *testing.T, fs *fakeStore, svc *KeyService, userID string) (plaintext string, keyID string, accountID string) {
	t.Helper()
	created, err := svc.CreateKey(context.Background(), userID, CreateParams{})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return created.Plaintext, created.Key.ID, created.Key.BillingAccountID
}

func TestAuthenticate_HappyPath(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	r := NewResolver(fs, nil)
	plaintext, keyID, accountID := mintKey(t, fs, svc, "user-a")

	res, err := r.Authenticate(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Principal.Kind != domain.PrincipalAPIKey {
		t.Errorf("kind = %q, want api_key", res.Principal.Kind)
	}
	if res.Principal.BillingAccountID != accountID || res.Principal.APIKeyID != keyID {
		t.Errorf("principal ids wrong: %+v", res.Principal)
	}
	if len(fs.touched) != 1 || fs.touched[0] != keyID {
		t.Errorf("last_used not touched: %v", fs.touched)
	}
}

func TestAuthenticate_Rejections(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	r := NewResolver(fs, nil)
	plaintext, _, _ := mintKey(t, fs, svc, "user-a")

	// wrongSecret 的末字符必须与真值不同:真值末字符恰好是 "A" 时
	// 直接拼接会得到正确的 key(每跑约 1.6% 的随机 flake,评审轮5)。
	wrongLast := "A"
	if plaintext[len(plaintext)-1] == 'A' {
		wrongLast = "B"
	}
	for name, presented := range map[string]string{
		"empty":         "",
		"malformed":     "sk-not-ours",
		"unknownPrefix": "yk-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",
		"wrongSecret":   plaintext[:len(plaintext)-1] + wrongLast,
	} {
		if _, err := r.Authenticate(context.Background(), presented); err == nil || domain.CodeOf(err) != domain.CodeInvalidKey {
			t.Errorf("%s: want invalid_key, got %v", name, err)
		}
	}
}

func TestAuthenticate_RevokedAndExpired(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()

	// Revoked.
	revoked, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r := NewResolver(fs, nil)
	if _, err := r.Authenticate(ctx, revoked.Plaintext); err != nil {
		t.Fatalf("pre-revoke auth: %v", err)
	}
	if _, err := svc.RevokeKey(ctx, "user-a", revoked.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := r.Authenticate(ctx, revoked.Plaintext); err == nil || domain.CodeOf(err) != domain.CodeInvalidKey {
		t.Fatalf("revoked key accepted: %v", err)
	}

	// Expired: fake clock beyond expires_at; the lazy transition flips
	// status to expired.
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	svc2 := NewKeyService(fs, clock)
	exp := now.Add(time.Hour)
	expKey, err := svc2.CreateKey(ctx, "user-b", CreateParams{ExpiresAt: &exp})
	if err != nil {
		t.Fatalf("create expiring: %v", err)
	}
	r2 := NewResolver(fs, clock)
	if _, err := r2.Authenticate(ctx, expKey.Plaintext); err != nil {
		t.Fatalf("pre-expiry auth: %v", err)
	}
	now = exp // at the boundary: [start, end) semantics → expired
	if _, err := r2.Authenticate(ctx, expKey.Plaintext); err == nil || domain.CodeOf(err) != domain.CodeInvalidKey {
		t.Fatalf("expired key accepted: %v", err)
	}
	if len(fs.markedExpired) != 1 || fs.markedExpired[0] != expKey.Key.ID {
		t.Errorf("lazy expiry transition missing: %v", fs.markedExpired)
	}
}

func TestAuthenticate_SuspendedAccount(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	r := NewResolver(fs, nil)
	plaintext, _, accountID := mintKey(t, fs, svc, "user-a")
	for _, a := range fs.accountsByUser {
		if a.ID == accountID {
			a.Status = "suspended"
		}
	}
	if _, err := r.Authenticate(context.Background(), plaintext); err == nil || domain.CodeOf(err) != domain.CodeInvalidKey {
		t.Fatalf("suspended account key accepted: %v", err)
	}
}

func TestResolveUserSession_Facade(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	r := NewResolver(fs, nil)
	ctx := context.Background()

	// No account yet → not_found (facade is read-only, never creates).
	if _, err := r.ResolveUserSession(ctx, "ghost"); err == nil || domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	acct, err := svc.EnsureAccount(ctx, "user-a")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	p, err := r.ResolveUserSession(ctx, "user-a")
	if err != nil {
		t.Fatalf("ResolveUserSession: %v", err)
	}
	if p.Kind != domain.PrincipalKayaJWT || p.BillingAccountID != acct.ID || p.UserID != "user-a" {
		t.Errorf("facade principal wrong: %+v", p)
	}
	// Suspended account fails closed.
	fs.accountsByUser["user-a"].Status = "closed"
	if _, err := r.ResolveUserSession(ctx, "user-a"); err == nil {
		t.Error("suspended account session accepted")
	}
}

func TestAuthorizedModelIDs_UnionAndNarrowing(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	r := NewResolver(fs, nil)
	ctx := context.Background()
	acct, _ := svc.EnsureAccount(ctx, "user-a")
	fs.ents = append(fs.ents,
		domain.Entitlement{ID: "e1", BillingAccountID: acct.ID, Status: domain.EntitlementActive, ModelIDs: []string{"a", "b"}},
		domain.Entitlement{ID: "e2", BillingAccountID: acct.ID, Status: domain.EntitlementActive, ModelIDs: []string{"b", "c"}},
	)
	p := &domain.Principal{Kind: domain.PrincipalKayaJWT, BillingAccountID: acct.ID}

	got, err := r.AuthorizedModelIDs(ctx, p, nil)
	if err != nil {
		t.Fatalf("AuthorizedModelIDs: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("union = %v, want 3 models", got)
	}
	// Key-level narrowing intersects, never widens.
	key := &domain.APIKey{ModelAllow: []string{"a", "zzz"}}
	got, err = r.AuthorizedModelIDs(ctx, p, key)
	if err != nil {
		t.Fatalf("narrowed: %v", err)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("narrowed = %v, want [a]", got)
	}
}
