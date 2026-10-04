package access

import (
	"context"
	"sync"
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
// revocation and expiry take effect on the very next call (设计 §5). Only
// the last_used 遥测写入按 key 节流（见 touchLastUsed）：它纯属使用信号，
// 鉴权决策从不依赖它。
type Resolver struct {
	store KeyStore
	now   func() time.Time

	// last_used 写入节流：同一 key 在窗口内最多真正落库一次，避免热点 key
	// 并发时在该行上串行化。
	touchMu     sync.Mutex
	lastTouched map[string]time.Time // keyID → 上次真正落库时间
	touchWrites int                  // 真实写入计数（驱动确定性清扫）
}

// lastUsedTouchWindow 是 last_used 遥测写入的节流窗口：同一 key 在窗口内
// 的重复请求直接跳过落库。
const lastUsedTouchWindow = time.Minute

// lastUsedSweepEvery 次真实写入后顺带清扫一次陈旧条目，防止已删除/轮换
// key 的节流条目让 map 缓慢增长。
const lastUsedSweepEvery = 64

// NewResolver builds the resolver; a nil clock uses the wall clock.
func NewResolver(store KeyStore, now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{store: store, now: now, lastTouched: map[string]time.Time{}}
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
	r.touchLastUsed(ctx, key.ID)
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

// touchLastUsed 按 keyID 节流 best-effort 的 last_used 遥测写入：同一
// key 在窗口内最多真正落库一次，窗口内重复请求直接跳过。写入错误一律
// 忽略——鉴权决策不得依赖该写入。
func (r *Resolver) touchLastUsed(ctx context.Context, keyID string) {
	now := r.now()
	r.touchMu.Lock()
	if last, ok := r.lastTouched[keyID]; ok && now.Sub(last) < lastUsedTouchWindow {
		r.touchMu.Unlock()
		return
	}
	r.lastTouched[keyID] = now
	r.touchWrites++
	if r.touchWrites%lastUsedSweepEvery == 0 {
		// 确定性清扫：删掉超过 2 个窗口未再命中的条目（已删除/轮换的 key
		// 不再命中，其条目随写入推进被惰性回收）。
		cutoff := now.Add(-2 * lastUsedTouchWindow)
		for id, at := range r.lastTouched {
			if at.Before(cutoff) {
				delete(r.lastTouched, id)
			}
		}
	}
	r.touchMu.Unlock()
	// best-effort：DB 写失败也照样标记已触（上面 lastTouched 已写入），
	// 瞬时故障最多让 last_used_at 滞后一个窗口——遥测信号，不值得为此
	// 重试或阻塞鉴权路径。
	_ = r.store.TouchAPIKeyLastUsed(ctx, keyID, now)
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
