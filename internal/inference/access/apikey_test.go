package access

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// fakeStore is an in-memory KeyStore for service/resolver logic tests.
// Postgres behavior (UNIQUE races, pagination) is covered by the real-DB
// tests in internal/inference/postgres.
type fakeStore struct {
	mu             sync.Mutex
	accountsByUser map[string]*domain.BillingAccount
	keys           map[string]*storedKey // by id
	byPrefix       map[string]string     // prefix → id
	ents           []domain.Entitlement
	markedExpired  []string
	touched        []string
}

type storedKey struct {
	key  domain.APIKey
	hash string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accountsByUser: make(map[string]*domain.BillingAccount),
		keys:           make(map[string]*storedKey),
		byPrefix:       make(map[string]string),
	}
}

func (f *fakeStore) EnsureBillingAccount(_ context.Context, userID string) (*domain.BillingAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.accountsByUser[userID]; ok {
		return a, nil
	}
	a := &domain.BillingAccount{ID: uuid.NewString(), UserID: userID, SubjectType: "user", Status: "active"}
	f.accountsByUser[userID] = a
	return a, nil
}

func (f *fakeStore) GetBillingAccountByUser(_ context.Context, userID string) (*domain.BillingAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.accountsByUser[userID]; ok {
		return a, nil
	}
	return nil, domain.NewError(domain.CodeNotFound, "billing account not found")
}

func (f *fakeStore) GetBillingAccountByID(_ context.Context, id string) (*domain.BillingAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accountsByUser {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, domain.NewError(domain.CodeNotFound, "billing account not found")
}

func (f *fakeStore) ListActiveEntitlements(_ context.Context, accountID string, _ time.Time) ([]domain.Entitlement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Entitlement
	for _, e := range f.ents {
		if e.BillingAccountID == accountID && e.Status == domain.EntitlementActive {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) InsertAPIKey(_ context.Context, k *domain.APIKey, keyHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.byPrefix[k.Prefix]; dup {
		return domain.NewError(domain.CodeConflict, "duplicate prefix")
	}
	k.ID = uuid.NewString()
	sk := &storedKey{key: *k, hash: keyHash}
	f.keys[k.ID] = sk
	f.byPrefix[k.Prefix] = k.ID
	return nil
}

func (f *fakeStore) GetAPIKeyByPrefix(_ context.Context, prefix string) (*domain.APIKey, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byPrefix[prefix]
	if !ok {
		return nil, "", domain.NewError(domain.CodeNotFound, "key not found")
	}
	sk := f.keys[id]
	k := sk.key
	return &k, sk.hash, nil
}

func (f *fakeStore) GetAPIKeyByID(_ context.Context, id string) (*domain.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sk, ok := f.keys[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "key not found")
	}
	k := sk.key
	return &k, nil
}

func (f *fakeStore) ListAPIKeysByAccount(_ context.Context, accountID string, limit, offset int) ([]domain.APIKey, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []domain.APIKey
	for _, sk := range f.keys {
		if sk.key.BillingAccountID == accountID {
			all = append(all, sk.key)
		}
	}
	total := int64(len(all))
	if offset >= len(all) {
		return []domain.APIKey{}, total, nil
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total, nil
}

func (f *fakeStore) UpdateAPIKey(_ context.Context, k *domain.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sk, ok := f.keys[k.ID]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "key not found")
	}
	sk.key = *k
	return nil
}

func (f *fakeStore) RevokeAPIKey(_ context.Context, id string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sk, ok := f.keys[id]
	if !ok {
		return false, domain.NewError(domain.CodeNotFound, "key not found")
	}
	if sk.key.Status != domain.APIKeyActive {
		return false, nil
	}
	sk.key.Status = domain.APIKeyRevoked
	sk.key.RevokedAt = &at
	return true, nil
}

func (f *fakeStore) MarkAPIKeyExpired(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sk, ok := f.keys[id]; ok && sk.key.Status == domain.APIKeyActive {
		sk.key.Status = domain.APIKeyExpired
		f.markedExpired = append(f.markedExpired, id)
	}
	return nil
}

func (f *fakeStore) TouchAPIKeyLastUsed(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sk, ok := f.keys[id]; ok {
		sk.key.LastUsedAt = &at
		f.touched = append(f.touched, id)
	}
	return nil
}

// --- generation / digest ---

func TestGenerate_FormatAndEntropy(t *testing.T) {
	plaintext, prefix, digest, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(plaintext, KeyPrefix) {
		t.Errorf("plaintext missing %q prefix: %q", KeyPrefix, plaintext)
	}
	// "yk-" + 43 base64url chars = 46 total, 256-bit body.
	if len(plaintext) != 46 {
		t.Errorf("unexpected key length %d", len(plaintext))
	}
	if prefix != plaintext[:12] || len(prefix) != 12 {
		t.Errorf("lookup prefix mismatch: %q", prefix)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("digest scheme: %q", digest)
	}
	if !VerifyDigest(plaintext, digest) {
		t.Error("VerifyDigest rejected a fresh key")
	}
	if VerifyDigest(plaintext+"x", digest) {
		t.Error("VerifyDigest accepted a tampered key")
	}
	if VerifyDigest(plaintext, "bcrypt:"+digest[7:]) {
		t.Error("VerifyDigest accepted an unknown scheme")
	}
}

func TestGenerate_UniquePrefixes(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 2000; i++ {
		p, prefix, _, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, dup := seen[prefix]; dup {
			t.Fatalf("prefix collision at %d: %q", i, prefix)
		}
		seen[prefix] = struct{}{}
		_ = p
	}
}

func TestLookupPrefix_Malformed(t *testing.T) {
	for _, bad := range []string{"", "yk-", "sk-abc1234567890", "yk-short"} {
		if LookupPrefix(bad) != "" {
			t.Errorf("LookupPrefix(%q) should be empty", bad)
		}
	}
}

// --- KeyService ---

func TestCreateKey_EnsuresAccountAndReturnsPlaintextOnce(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()

	c1, err := svc.CreateKey(ctx, "user-a", CreateParams{Name: "cli"})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !strings.HasPrefix(c1.Plaintext, KeyPrefix) {
		t.Errorf("plaintext missing prefix")
	}
	// The store must hold prefix + digest, never the plaintext.
	sk := fs.keys[c1.Key.ID]
	if sk.hash == c1.Plaintext || !strings.HasPrefix(sk.hash, "sha256:") {
		t.Errorf("store kept plaintext or wrong digest: %q", sk.hash)
	}
	// Second create reuses the SAME account (idempotent establishment).
	c2, err := svc.CreateKey(ctx, "user-a", CreateParams{Name: "ci"})
	if err != nil {
		t.Fatalf("CreateKey #2: %v", err)
	}
	if c1.Key.BillingAccountID != c2.Key.BillingAccountID {
		t.Error("two keys landed on different accounts for one user")
	}
}

func TestCreateKey_ModelScopeEnforced(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()

	// No entitlement at all → any explicit model list is rejected.
	if _, err := svc.CreateKey(ctx, "user-a", CreateParams{ModelAllow: []string{"glm-4.6"}}); err == nil || domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Fatalf("want model_not_allowed without entitlement, got %v", err)
	}

	acct, _ := svc.EnsureAccount(ctx, "user-a")
	fs.ents = append(fs.ents, domain.Entitlement{
		ID: uuid.NewString(), BillingAccountID: acct.ID, Status: domain.EntitlementActive,
		ModelIDs: []string{"glm-4.6", "deepseek-v4"},
	})
	if _, err := svc.CreateKey(ctx, "user-a", CreateParams{ModelAllow: []string{"glm-4.6"}}); err != nil {
		t.Fatalf("narrowing within entitlement rejected: %v", err)
	}
	if _, err := svc.CreateKey(ctx, "user-a", CreateParams{ModelAllow: []string{"glm-4.6", "gpt-x"}}); err == nil || domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Fatalf("widening beyond entitlement accepted: %v", err)
	}
	// nil/empty allow = no key-level narrowing (entitlement still governs).
	if _, err := svc.CreateKey(ctx, "user-a", CreateParams{}); err != nil {
		t.Fatalf("nil allow rejected: %v", err)
	}
	if _, err := svc.CreateKey(ctx, "user-a", CreateParams{ModelAllow: []string{}}); err != nil {
		t.Fatalf("empty allow rejected: %v", err)
	}
}

func TestCreateKey_InputValidation(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()
	neg := int64(-1)
	zero := 0
	past := time.Now().Add(-time.Hour)
	cases := []CreateParams{
		{Name: strings.Repeat("x", 129)},
		{BudgetMicros: &neg},
		{RPMLimit: &zero},
		{ExpiresAt: &past},
		{ModelAllow: []string{"  "}},
	}
	for i, p := range cases {
		if _, err := svc.CreateKey(ctx, "user-a", p); err == nil || domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("case %d: want invalid_input, got %v", i, err)
		}
	}
}

func TestListKeys_NoAccountIsEmptyPage(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	page, err := svc.ListKeys(context.Background(), "ghost", 50, 0)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Errorf("want empty page, got %+v", page)
	}
	if len(fs.accountsByUser) != 0 {
		t.Error("listing must not create a billing account")
	}
}

func TestGetKey_CrossCustomerIs404(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// Customer B cannot read A's key; the error is indistinguishable from
	// a missing key.
	if _, err := svc.GetKey(ctx, "user-b", created.Key.ID); err == nil || domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-customer read: want not_found, got %v", err)
	}
	if _, err := svc.GetKey(ctx, "user-a", created.Key.ID); err != nil {
		t.Fatalf("owner read: %v", err)
	}
}

func TestUpdateKey_ScopeAndClears(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()
	acct, _ := svc.EnsureAccount(ctx, "user-a")
	fs.ents = append(fs.ents, domain.Entitlement{
		ID: uuid.NewString(), BillingAccountID: acct.ID, Status: domain.EntitlementActive,
		ModelIDs: []string{"glm-4.6"},
	})
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	id := created.Key.ID

	// Widening beyond entitlement rejected.
	if _, err := svc.UpdateKey(ctx, "user-a", id, KeyPatch{ModelAllow: &[]string{"gpt-x"}}); err == nil || domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Fatalf("widening accepted: %v", err)
	}
	// Narrowing within entitlement + budget set.
	budget := int64(5_000_000)
	updated, err := svc.UpdateKey(ctx, "user-a", id, KeyPatch{
		ModelAllow: &[]string{"glm-4.6"}, BudgetMicros: &budget,
	})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if len(updated.ModelAllow) != 1 || updated.BudgetLimit == nil || *updated.BudgetLimit != 5_000_000 {
		t.Errorf("patch not applied: %+v", updated)
	}
	// Clear budget; clear narrowing.
	updated, err = svc.UpdateKey(ctx, "user-a", id, KeyPatch{ClearBudget: true, ModelAllow: &[]string{}})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if updated.BudgetLimit != nil || len(updated.ModelAllow) != 0 {
		t.Errorf("clears not applied: %+v", updated)
	}
	// Cross-customer update is 404.
	if _, err := svc.UpdateKey(ctx, "user-b", id, KeyPatch{}); err == nil || domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-customer update: want not_found, got %v", err)
	}
}

func TestRevokeKey_IdempotentAndOwned(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	id := created.Key.ID
	if _, err := svc.RevokeKey(ctx, "user-b", id); err == nil || domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-customer revoke: want not_found, got %v", err)
	}
	changed, err := svc.RevokeKey(ctx, "user-a", id)
	if err != nil || !changed {
		t.Fatalf("first revoke: changed=%v err=%v", changed, err)
	}
	changed, err = svc.RevokeKey(ctx, "user-a", id)
	if err != nil || changed {
		t.Fatalf("second revoke must be idempotent: changed=%v err=%v", changed, err)
	}
}

func TestValidateModelScope(t *testing.T) {
	ents := []domain.Entitlement{{ModelIDs: []string{"a", "b"}}, {ModelIDs: []string{"c"}}}
	if err := ValidateModelScope([]string{"a", "c"}, ents); err != nil {
		t.Errorf("subset rejected: %v", err)
	}
	if err := ValidateModelScope([]string{"a", "x"}, ents); err == nil {
		t.Error("superset accepted")
	}
	if err := ValidateModelScope([]string{"a"}, nil); err == nil {
		t.Error("no-entitlement account authorized a model")
	}
}
