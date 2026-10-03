package paypal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yunhou/users/internal/model"
)

// TokenCache memoizes PayPal OAuth tokens in-process. One cache per Yunhou
// instance is fine — horizontal-scaling scenarios accept that each instance
// refreshes independently because PayPal OAuth is idempotent for the same
// (client_id, client_secret).
//
// Concurrent fetches for the same cacheKey are collapsed into a single
// upstream call (per-key inflight dedup) — without this, a cold cache
// under burst load would issue N parallel POSTs to PayPal and risk hitting
// rate limits during deploys / TTL expiry.
//
// This is the standard singleflight pattern inlined to avoid the
// golang.org/x/sync dependency (which would otherwise be a transitive
// dep). The inlined version preserves the two contract guarantees the
// upstream singleflight.Group provides that matter here:
//
//  1. Leader-error propagation: a single fetch failure surfaces to every
//     follower as ErrUpstreamFailed. Followers do NOT each retry —
//     retrying would re-create the thundering-herd the dedup prevents
//     (see TestTokenCache_GetOrFetch_ConcurrentMissesDeduped).
//  2. No-leader-window: the inflight entry is deleted AND the done
//     channel is closed while holding c.mu, so a caller that missed the
//     inflight slot never becomes a duplicate leader mid-flight.
//
// Neither guarantee relies on x/sync internals; both are enforced in the
// loop below. Switching to singleflight.Group would not change behaviour.
type TokenCache struct {
	// safetyMargin is subtracted from expires_in so we don't hand out a
	// token that's about to expire.
	safetyMargin time.Duration
	mu           sync.Mutex
	entries      map[string]cachedToken
	inflight     map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{} // closed when leader's fetch returns
	tok  *Token
	err  error
}

type cachedToken struct {
	token     *Token
	expiresAt time.Time
}

func NewTokenCache(safetyMargin time.Duration) *TokenCache {
	return &TokenCache{
		safetyMargin: safetyMargin,
		entries:      map[string]cachedToken{},
		inflight:     map[string]*inflightCall{},
	}
}

// GetOrFetch returns a token, calling fetch on miss or after expiry. cacheKey
// is typically the client_id. Concurrent misses for the same cacheKey share
// one fetch; later callers wait for the in-flight result.
//
// On leader failure the error propagates to all waiters — they do NOT each
// fan out and try again, because that would re-create the thundering-herd
// behaviour the singleflight is designed to prevent (PayPal incidents that
// return 503 would otherwise see N parallel retries from every Yunhou
// instance).
//
// fetch takes no context: cancellation is the callback's own concern —
// CachedClient.FetchToken binds the request ctx into its closure, so the
// leader's upstream call dies with that request. A waiter that abandons the
// call does not cancel the shared fetch; the result still lands in the
// cache for the next caller.
func (c *TokenCache) GetOrFetch(cacheKey string, fetch func() (*Token, error)) (*Token, error) {
	c.mu.Lock()
	if e, ok := c.entries[cacheKey]; ok && time.Now().Before(e.expiresAt) {
		// 命中时把 ExpiresIn 折算成剩余秒数 —— 签发时的原值会让调用方在
		// 缓存窗口尾部高估 token 余命。expiresAt 已扣过 safetyMargin,
		// 折算值偏保守,方向正确。返回副本,不回写共享的缓存条目。
		tok := *e.token
		tok.ExpiresIn = int(time.Until(e.expiresAt).Seconds())
		c.mu.Unlock()
		return &tok, nil
	}
	if call, ok := c.inflight[cacheKey]; ok {
		c.mu.Unlock()
		<-call.done
		// Either the leader stored an entry (call.tok non-nil) or it
		// errored (call.err). Return whichever it set; do not retry. A
		// nil token alongside a nil error means the fetch callback
		// returned (nil, nil) — treat as upstream failure rather than
		// dereferencing nil and panicking in the caller's AccessToken
		// read.
		if call.err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUpstreamFailed, call.err)
		}
		if call.tok == nil {
			return nil, fmt.Errorf("%w: leader returned nil token", ErrUpstreamFailed)
		}
		return call.tok, nil
	}
	// We're the leader for this key.
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[cacheKey] = call
	c.mu.Unlock()

	call.tok, call.err = fetch()

	if call.err == nil && call.tok != nil {
		ttl := time.Duration(call.tok.ExpiresIn)*time.Second - c.safetyMargin
		if ttl <= 0 {
			ttl = 30 * time.Second
		}
		// PayPal's documented expires_in for client_credentials OAuth is
		// up to ~3h (recent docs allow up to ~12h for partner tokens).
		// The previous 1h cap forced a 3x increase in upstream POSTs vs.
		// what the protocol allows, which compounds during PayPal
		// incidents. Cap at 3h — covers the documented value with a
		// comfortable margin but still bounds a leaked cache key's
		// blast radius against a misconfigured upstream that returns
		// expires_in = 86400 or similar.
		const maxTTL = 3 * time.Hour
		if ttl > maxTTL {
			ttl = maxTTL
		}
		c.mu.Lock()
		c.entries[cacheKey] = cachedToken{token: call.tok, expiresAt: time.Now().Add(ttl)}
		c.mu.Unlock()
	}

	// Hold the lock across inflight-delete + done-close. The race we are
	// closing: between unlock (below) and close, a concurrent caller could
	// see no cache entry (leader failed → no entry written) and no inflight
	// (just deleted), and become a NEW leader — re-fanning out exactly the
	// upstream burst the singleflight is meant to prevent. Closing inside
	// the lock guarantees every follower has been notified before any
	// caller can re-enter as a new leader.
	c.mu.Lock()
	delete(c.inflight, cacheKey)
	close(call.done)
	c.mu.Unlock()

	if call.err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUpstreamFailed, call.err)
	}
	if call.tok == nil {
		return nil, fmt.Errorf("%w: leader returned nil token", ErrUpstreamFailed)
	}
	return call.tok, nil
}

// ErrUpstreamFailed is returned (wrapped) when the upstream PayPal OAuth
// call failed. Callers can map this to a 502 Bad Gateway at the handler
// layer. Both the leader and any concurrent followers receive this error;
// using errors.Is lets callers tell a singleflight-cache failure from any
// other failure mode the fetch callback might surface.
var ErrUpstreamFailed = errors.New("paypal token fetch failed")

// CachedClient composes OAuthClient and TokenCache, returning the response
// shape (model.ProviderToken) that the API layer hands back to callers.
type CachedClient struct {
	oauth *OAuthClient
	cache *TokenCache
}

func NewCachedClient(oauth *OAuthClient, cache *TokenCache) *CachedClient {
	return &CachedClient{oauth: oauth, cache: cache}
}

// FetchToken returns a model.ProviderToken. Caches per client_id; safe for
// concurrent use.
func (c *CachedClient) FetchToken(ctx context.Context, clientID, clientSecret string) (*model.ProviderToken, error) {
	tok, err := c.cache.GetOrFetch(clientID, func() (*Token, error) {
		return c.oauth.FetchToken(ctx, clientID, clientSecret)
	})
	if err != nil {
		return nil, err
	}
	return &model.ProviderToken{
		Channel:     "paypal",
		AccessToken: tok.AccessToken,
		ExpiresIn:   tok.ExpiresIn,
	}, nil
}
