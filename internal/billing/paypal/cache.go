package paypal

import (
	"context"
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
// rate limits during deploys / TTL expiry. This is the standard
// singleflight pattern inlined to avoid the golang.org/x/sync dependency.
type TokenCache struct {
	// safetyMargin is subtracted from expires_in so we don't hand out a
	// token that's about to expire.
	safetyMargin time.Duration
	mu           sync.Mutex
	entries      map[string]cachedToken
	inflight     map[string]chan struct{} // keys with a fetch in flight
}

type cachedToken struct {
	token     *Token
	expiresAt time.Time
}

func NewTokenCache(safetyMargin time.Duration) *TokenCache {
	return &TokenCache{
		safetyMargin: safetyMargin,
		entries:      map[string]cachedToken{},
		inflight:     map[string]chan struct{}{},
	}
}

// GetOrFetch returns a token, calling fetch on miss or after expiry. cacheKey
// is typically the client_id. Concurrent misses for the same cacheKey share
// one fetch; later callers wait for the in-flight result.
func (c *TokenCache) GetOrFetch(cacheKey string, fetch func() (*Token, error)) (*Token, error) {
	c.mu.Lock()
	if e, ok := c.entries[cacheKey]; ok && time.Now().Before(e.expiresAt) {
		c.mu.Unlock()
		return e.token, nil
	}
	// Already being fetched by another goroutine — wait for it.
	if ch, ok := c.inflight[cacheKey]; ok {
		c.mu.Unlock()
		<-ch
		// Re-check cache: the leader just stored its result.
		c.mu.Lock()
		if e, ok := c.entries[cacheKey]; ok {
			c.mu.Unlock()
			return e.token, nil
		}
		c.mu.Unlock()
		// Leader's fetch failed (no entry written) — fall through and try
		// ourselves to avoid being permanently stuck on a failure.
	}

	// We're the leader for this key.
	ch := make(chan struct{})
	c.inflight[cacheKey] = ch
	c.mu.Unlock()

	tok, err := fetch()

	c.mu.Lock()
	if err == nil && tok != nil {
		ttl := time.Duration(tok.ExpiresIn)*time.Second - c.safetyMargin
		if ttl <= 0 {
			ttl = 30 * time.Second
		}
		c.entries[cacheKey] = cachedToken{token: tok, expiresAt: time.Now().Add(ttl)}
	}
	delete(c.inflight, cacheKey)
	c.mu.Unlock()
	close(ch)

	if err != nil {
		return nil, err
	}
	return tok, nil
}

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
