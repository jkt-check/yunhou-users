// Package access resolves caller credentials into internal principals:
// customer API Keys for the standard /v1/* surface and Kaya JWTs via the
// facade (Task 5; 设计 §4.2、§9.2). Operator principals (Task 4) live in a
// separate chain and never mix with callers — X-App-Secret is not a
// customer credential.
package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// API key format: "yk-" + base64url(32 random bytes) — 256 bits of
// crypto/rand entropy, 46 chars total.
//
// Digest decision: SHA-256 over the full key, stored as "sha256:<hex>".
// The key itself is high-entropy (2^256 space), so brute force against a
// fast hash is infeasible; bcrypt's tunable cost exists to protect
// LOW-entropy human passwords and would add ~100ms to every /v1 request
// for zero security gain here. The plaintext is returned exactly once at
// creation; the DB holds only key_prefix (lookup/display) + key_hash.
const (
	KeyPrefix    = "yk-"
	keyBodyBytes = 32 // 256-bit entropy
	// lookupPrefixLen is the UNIQUE-indexed lookup prefix length stored in
	// key_prefix: "yk-" + 9 base64url chars ≈ 54 bits — enough to make
	// accidental collisions vanishingly rare; the UNIQUE constraint plus
	// create-time retry covers the residue.
	lookupPrefixLen = 12
)

// Generate mints a fresh API key, returning the plaintext (shown once),
// the lookup prefix and the verification digest to persist.
func Generate() (plaintext, lookupPrefix, digest string, err error) {
	var buf [keyBodyBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", "", err
	}
	plaintext = KeyPrefix + base64.RawURLEncoding.EncodeToString(buf[:])
	return plaintext, LookupPrefix(plaintext), Digest(plaintext), nil
}

// LookupPrefix extracts the indexed lookup prefix from a presented key.
// Malformed keys yield "" (the caller maps that to CodeInvalidKey).
func LookupPrefix(presented string) string {
	if len(presented) < lookupPrefixLen || !strings.HasPrefix(presented, KeyPrefix) {
		return ""
	}
	return presented[:lookupPrefixLen]
}

// Digest returns the stored verification form of a key ("sha256:<hex>").
func Digest(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// VerifyDigest compares a presented key against a stored digest in
// constant time. Unknown digest schemes never match.
func VerifyDigest(presented, storedDigest string) bool {
	scheme, hexDigest, ok := strings.Cut(storedDigest, ":")
	if !ok || scheme != "sha256" {
		return false
	}
	want, err := hex.DecodeString(hexDigest)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// KeyStore is the persistence surface the access layer needs. Satisfied
// by inference/postgres.Store.
type KeyStore interface {
	EnsureBillingAccount(ctx context.Context, userID string) (*domain.BillingAccount, error)
	GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error)
	GetBillingAccountByID(ctx context.Context, id string) (*domain.BillingAccount, error)
	ListActiveEntitlements(ctx context.Context, billingAccountID string, at time.Time) ([]domain.Entitlement, error)

	InsertAPIKey(ctx context.Context, k *domain.APIKey, keyHash string) error
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (*domain.APIKey, string, error)
	GetAPIKeyByID(ctx context.Context, id string) (*domain.APIKey, error)
	ListAPIKeysByAccount(ctx context.Context, accountID string, limit, offset int) ([]domain.APIKey, int64, error)
	UpdateAPIKey(ctx context.Context, k *domain.APIKey) error
	RevokeAPIKey(ctx context.Context, id string, at time.Time) (bool, error)
	MarkAPIKeyExpired(ctx context.Context, id string) error
	TouchAPIKeyLastUsed(ctx context.Context, id string, at time.Time) error
}

// CreateParams carries the customer-chosen attributes of a new key.
type CreateParams struct {
	Name       string
	ModelAllow []string
	// BudgetMicros is the optional per-Key sub-budget. It only caps how
	// much THIS key may draw from the shared account entitlement pool —
	// it never adds to the account's total (设计 §4.2, Task 5 控制者决定).
	BudgetMicros *int64
	RPMLimit     *int
	ExpiresAt    *time.Time
}

// CreatedKey is the one-time creation result: Plaintext leaves the server
// in this response only and is never recoverable afterwards.
type CreatedKey struct {
	Key       *domain.APIKey
	Plaintext string
}

// KeyPage is one page of the account's keys plus the total count.
type KeyPage struct {
	Items []domain.APIKey
	Total int64
}

const maxKeyNameLen = 128

// KeyService implements the /user/api-keys management use cases. All
// ownership derives from the server-verified user identity — never from
// request-body fields.
type KeyService struct {
	store KeyStore
	now   func() time.Time
}

// NewKeyService builds the management service; a nil clock uses the wall
// clock (tests inject a fake one for expiry paths).
func NewKeyService(store KeyStore, now func() time.Time) *KeyService {
	if now == nil {
		now = time.Now
	}
	return &KeyService{store: store, now: now}
}

// EnsureAccount idempotently establishes the user's personal billing
// account (UNIQUE(user_id), ON CONFLICT DO NOTHING in the store). The
// owner is always the authenticated user.
func (s *KeyService) EnsureAccount(ctx context.Context, userID string) (*domain.BillingAccount, error) {
	return s.store.EnsureBillingAccount(ctx, userID)
}

// CreateKey mints a key for the user's own account. A non-empty
// ModelAllow must stay within the union of the account's ACTIVE
// entitlement model sets — key-level permissions narrow, never widen
// (设计 §4.2).
func (s *KeyService) CreateKey(ctx context.Context, userID string, p CreateParams) (*CreatedKey, error) {
	if len(p.Name) > maxKeyNameLen {
		return nil, domain.NewError(domain.CodeInvalidInput, "name too long (max 128 chars)")
	}
	if p.BudgetMicros != nil && *p.BudgetMicros < 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "budget_micros must be >= 0")
	}
	if p.RPMLimit != nil && *p.RPMLimit <= 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "rpm_limit must be > 0")
	}
	if p.ExpiresAt != nil && !p.ExpiresAt.After(s.now()) {
		return nil, domain.NewError(domain.CodeInvalidInput, "expires_at must be in the future")
	}
	allow, err := normalizeModelAllow(p.ModelAllow)
	if err != nil {
		return nil, err
	}
	account, err := s.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(allow) > 0 {
		if err := s.checkModelScope(ctx, account.ID, allow); err != nil {
			return nil, err
		}
	}
	// Retry on the vanishingly rare prefix/digest collision (UNIQUE).
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		plaintext, prefix, digest, err := Generate()
		if err != nil {
			return nil, domain.WrapError(domain.CodeInternal, "key generation failed", err)
		}
		key := &domain.APIKey{
			BillingAccountID: account.ID,
			Name:             p.Name,
			Prefix:           prefix,
			ModelAllow:       allow,
			RPMLimit:         p.RPMLimit,
			Status:           domain.APIKeyActive,
			ExpiresAt:        p.ExpiresAt,
		}
		if p.BudgetMicros != nil {
			b := domain.Microcredit(*p.BudgetMicros)
			key.BudgetLimit = &b
		}
		if err := s.store.InsertAPIKey(ctx, key, digest); err != nil {
			if domain.CodeOf(err) == domain.CodeConflict {
				lastErr = err
				continue
			}
			return nil, err
		}
		return &CreatedKey{Key: key, Plaintext: plaintext}, nil
	}
	return nil, lastErr
}

// ListKeys returns one page of the caller's keys. A user without a
// billing account yet gets an empty page — listing must not create one.
func (s *KeyService) ListKeys(ctx context.Context, userID string, limit, offset int) (*KeyPage, error) {
	account, err := s.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return &KeyPage{Items: []domain.APIKey{}, Total: 0}, nil
		}
		return nil, err
	}
	items, total, err := s.store.ListAPIKeysByAccount(ctx, account.ID, limit, offset)
	if err != nil {
		return nil, err
	}
	return &KeyPage{Items: items, Total: total}, nil
}

// GetKey loads one of the caller's keys. A key owned by another account
// is reported as not_found — existence never leaks across customers.
func (s *KeyService) GetKey(ctx context.Context, userID, keyID string) (*domain.APIKey, error) {
	return s.ownedKey(ctx, userID, keyID)
}

// KeyPatch describes a permission/budget update. A nil pointer keeps the
// current value; the Clear* flags remove an optional constraint
// (budget / rpm / expiry). ModelAllow uses a pointer-to-slice: nil keeps,
// &[]string{} clears the narrowing, &[]string{...} replaces.
type KeyPatch struct {
	Name         *string
	ModelAllow   *[]string
	BudgetMicros *int64
	ClearBudget  bool
	RPMLimit     *int
	ClearRPM     bool
	ExpiresAt    *time.Time
	ClearExpires bool
}

// UpdateKey applies a patch after the same ownership and scope checks as
// CreateKey. Permission changes can never exceed the account entitlement
// (设计 §4.2: 权限变更不能超出所属账户权益).
func (s *KeyService) UpdateKey(ctx context.Context, userID, keyID string, patch KeyPatch) (*domain.APIKey, error) {
	key, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return nil, err
	}
	if patch.Name != nil {
		if len(*patch.Name) > maxKeyNameLen {
			return nil, domain.NewError(domain.CodeInvalidInput, "name too long (max 128 chars)")
		}
		key.Name = *patch.Name
	}
	if patch.ModelAllow != nil {
		allow, err := normalizeModelAllow(*patch.ModelAllow)
		if err != nil {
			return nil, err
		}
		if len(allow) > 0 {
			if err := s.checkModelScope(ctx, key.BillingAccountID, allow); err != nil {
				return nil, err
			}
		}
		key.ModelAllow = allow
	}
	if patch.ClearBudget {
		key.BudgetLimit = nil
	} else if patch.BudgetMicros != nil {
		if *patch.BudgetMicros < 0 {
			return nil, domain.NewError(domain.CodeInvalidInput, "budget_micros must be >= 0")
		}
		b := domain.Microcredit(*patch.BudgetMicros)
		key.BudgetLimit = &b
	}
	if patch.ClearRPM {
		key.RPMLimit = nil
	} else if patch.RPMLimit != nil {
		if *patch.RPMLimit <= 0 {
			return nil, domain.NewError(domain.CodeInvalidInput, "rpm_limit must be > 0")
		}
		key.RPMLimit = patch.RPMLimit
	}
	if patch.ClearExpires {
		key.ExpiresAt = nil
	} else if patch.ExpiresAt != nil {
		if !patch.ExpiresAt.After(s.now()) {
			return nil, domain.NewError(domain.CodeInvalidInput, "expires_at must be in the future")
		}
		key.ExpiresAt = patch.ExpiresAt
	}
	if err := s.store.UpdateAPIKey(ctx, key); err != nil {
		return nil, err
	}
	return key, nil
}

// RevokeKey revokes one of the caller's keys. Idempotent: revoking an
// already-revoked key reports changed=false. Resolution reads status
// straight from the store on every call, so revocation takes effect on
// the very next /v1 request (设计 §5: 禁用客户 Key 需服务端及时检查).
func (s *KeyService) RevokeKey(ctx context.Context, userID, keyID string) (changed bool, err error) {
	key, err := s.ownedKey(ctx, userID, keyID)
	if err != nil {
		return false, err
	}
	return s.store.RevokeAPIKey(ctx, key.ID, s.now())
}

// ownedKey loads keyID and proves it belongs to the caller's account.
// Cross-customer access is indistinguishable from a missing key (404).
func (s *KeyService) ownedKey(ctx context.Context, userID, keyID string) (*domain.APIKey, error) {
	key, err := s.store.GetAPIKeyByID(ctx, keyID)
	if err != nil {
		return nil, err
	}
	account, err := s.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		// No account → cannot own any key.
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, domain.NewError(domain.CodeNotFound, "api key not found")
		}
		return nil, err
	}
	if key.BillingAccountID != account.ID {
		return nil, domain.NewError(domain.CodeNotFound, "api key not found")
	}
	return key, nil
}

// checkModelScope verifies allow ⊆ union of the account's active
// entitlement model sets at the current time.
func (s *KeyService) checkModelScope(ctx context.Context, accountID string, allow []string) error {
	ents, err := s.store.ListActiveEntitlements(ctx, accountID, s.now())
	if err != nil {
		return err
	}
	if err := ValidateModelScope(allow, ents); err != nil {
		return err
	}
	return nil
}

// ValidateModelScope enforces key-level narrowing: every allowed model
// must appear in the union of the given entitlements' explicit model
// sets. An account with no active entitlement authorizes nothing.
func ValidateModelScope(allow []string, ents []domain.Entitlement) error {
	entitled := make(map[string]struct{})
	for _, e := range ents {
		for _, id := range e.ModelIDs {
			entitled[id] = struct{}{}
		}
	}
	for _, id := range allow {
		if _, ok := entitled[id]; !ok {
			return domain.NewError(domain.CodeModelNotAllowed,
				"model "+id+" is outside the account's entitlement")
		}
	}
	return nil
}

func normalizeModelAllow(in []string) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, domain.NewError(domain.CodeInvalidInput, "model_ids entries must be non-empty")
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
