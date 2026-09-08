package access

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// principal.go — 调用方 principal 解析（Task 5）。
//
// Two caller paths converge on ONE internal principal shape
// (domain.Principal, 设计 §3):
//
//   - /v1/* standard protocol: customer API Key → Authenticate →
//     Principal{Kind: PrincipalAPIKey, BillingAccountID, APIKeyID}.
//   - Kaya JWT (facade, consumed by the Task 8 gateway): userID →
//     ResolveUserSession → Principal{Kind: PrincipalKayaJWT, ...} — the
//     SAME shape, so the gateway never branches on the outer credential.
//
// Operator principals (Task 4, X-App-Secret + JWT dual identity) are a
// separate chain; customers never touch it.

// AuthResult is the authenticated caller plus the key/account context the
// rate limiter and quota gate need. Secret material is never carried:
// only prefix/digest live at rest, and neither appears here.
type AuthResult struct {
	Principal *domain.Principal
	Key       *domain.APIKey
	Account   *domain.BillingAccount
}

// Resolver authenticates presented credentials into principals. Every
// resolution reads current state from the store — there is no cache, so
// revocation and expiry take effect on the very next call (设计 §5).
type Resolver struct {
	store KeyStore
	now   func() time.Time
}

// NewResolver builds the resolver; a nil clock uses the wall clock.
func NewResolver(store KeyStore, now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{store: store, now: now}
}

var errInvalidKey = domain.NewError(domain.CodeInvalidKey, "invalid, revoked or expired API key")

// Authenticate verifies a presented customer key:
//  1. extract the indexed lookup prefix (malformed → invalid_key);
//  2. fetch the row by prefix and constant-time compare the SHA-256
//     digest of the FULL presented key (wrong secret → invalid_key);
//  3. enforce status — revoked keys fail immediately;
//  4. enforce expiry — a key past expires_at is marked expired
//     (best-effort) and rejected;
//  5. enforce the owning billing account being active.
func (r *Resolver) Authenticate(ctx context.Context, presented string) (*AuthResult, error) {
	prefix := LookupPrefix(presented)
	if prefix == "" {
		return nil, errInvalidKey
	}
	key, digest, err := r.store.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, errInvalidKey
		}
		return nil, err
	}
	if !VerifyDigest(presented, digest) {
		return nil, errInvalidKey
	}
	switch key.Status {
	case domain.APIKeyActive:
	case domain.APIKeyRevoked, domain.APIKeyExpired:
		return nil, errInvalidKey
	default:
		return nil, errInvalidKey
	}
	if key.ExpiresAt != nil && !r.now().Before(*key.ExpiresAt) {
		// Lazy status transition so later reads show the truth.
		_ = r.store.MarkAPIKeyExpired(ctx, key.ID) // best-effort
		return nil, errInvalidKey
	}
	account, err := r.store.GetBillingAccountByID(ctx, key.BillingAccountID)
	if err != nil {
		return nil, err
	}
	if account.Status != "active" {
		return nil, domain.NewError(domain.CodeInvalidKey, "billing account is not active")
	}
	// Best-effort usage signal; an auth decision must not depend on it.
	_ = r.store.TouchAPIKeyLastUsed(ctx, key.ID, r.now())
	return &AuthResult{
		Principal: &domain.Principal{
			Kind:             domain.PrincipalAPIKey,
			BillingAccountID: account.ID,
			APIKeyID:         key.ID,
		},
		Key:     key,
		Account: account,
	}, nil
}

// ResolveUserSession is the facade adapter: a server-verified Kaya user
// identity maps to the SAME internal principal shape with kind
// PrincipalKayaJWT and the user's own billing account. It is read-only —
// listing/resolving never creates an account; creation happens on the
// first management write (POST /user/api-keys) via EnsureBillingAccount.
func (r *Resolver) ResolveUserSession(ctx context.Context, userID string) (*domain.Principal, error) {
	if userID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "user identity required")
	}
	account, err := r.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if account.Status != "active" {
		return nil, domain.NewError(domain.CodeInvalidKey, "billing account is not active")
	}
	return &domain.Principal{
		Kind:             domain.PrincipalKayaJWT,
		BillingAccountID: account.ID,
		UserID:           userID,
	}, nil
}

// AuthorizedModelIDs resolves the caller's effective model set: the union
// of the account's active entitlement model sets, narrowed by the key's
// model_allow when present (key-level narrowing can never widen, 设计
// §4.2). Session principals have no key-level narrowing.
func (r *Resolver) AuthorizedModelIDs(ctx context.Context, p *domain.Principal, key *domain.APIKey) ([]string, error) {
	ents, err := r.store.ListActiveEntitlements(ctx, p.BillingAccountID, r.now())
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	union := make([]string, 0)
	for _, e := range ents {
		for _, id := range e.ModelIDs {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				union = append(union, id)
			}
		}
	}
	if key == nil || len(key.ModelAllow) == 0 {
		return union, nil
	}
	out := make([]string, 0, len(key.ModelAllow))
	for _, id := range key.ModelAllow {
		if _, ok := seen[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}
