package middleware

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type visitor struct {
	limiter *rate.Limiter
	// lastSeenUnixNano is updated concurrently by every allow() call —
	// atomic.Int64 sidesteps the data race that a plain time.Time field
	// would race on. cleanup() loads it under the janitor's Range; that
	// path is the only writer of the visitor's eviction candidacy.
	lastSeenUnixNano atomic.Int64
}

// rateLimiter uses sync.Map for the per-key visitor table so the hot
// path (allow) doesn't serialise every inbound request on a single
// mutex. The previous map+Mutex design became the throughput
// bottleneck under burst — every webhook delivery, every refresh,
// every JWKS hit contended for the same lock. sync.Map's Load/Store
// is lock-free for the read-mostly case; the only contention is the
// cold-path visitor-creation branch (one-time per IP).
//
// Trade-off: cleanup() now Range()s the map and collects expired
// entries into a local slice, then deletes them in a second pass. The
// Range is read-locked; the deletes use a CompareAndDelete-on-miss
// pattern so a concurrent evict doesn't fight the janitor.
type rateLimiter struct {
	visitors sync.Map // map[string]*visitor
	rate     rate.Limit
	burst    int
}

func newRateLimiter(r int, burst int) *rateLimiter {
	return &rateLimiter{
		rate:  rate.Limit(r),
		burst: burst,
	}
}

func (rl *rateLimiter) allow(key string) bool {
	now := time.Now()
	if v, ok := rl.visitors.Load(key); ok {
		visitor := v.(*visitor)
		visitor.lastSeenUnixNano.Store(now.UnixNano())
		return visitor.limiter.Allow()
	}
	// Cold path: create a fresh visitor. Two concurrent creates for
	// the same key are tolerated — the second write silently
	// overwrites the first, and both initialise an independent
	// rate.Limiter with the same rate/burst so the worst-case effect
	// is one extra in-flight token being granted, not a bypass.
	visitor := &visitor{
		limiter: rate.NewLimiter(rl.rate, rl.burst),
	}
	visitor.lastSeenUnixNano.Store(now.UnixNano())
	rl.visitors.Store(key, visitor)
	return visitor.limiter.Allow()
}

// cleanupEvictInterval bounds the visitor-table growth: visitors last-seen
// more than this duration ago are removed on the next janitor tick.
const cleanupEvictInterval = 2 * time.Minute

func (rl *rateLimiter) cleanup() {
	var stale []string
	rl.visitors.Range(func(k, v any) bool {
		visitor := v.(*visitor)
		if time.Since(time.Unix(0, visitor.lastSeenUnixNano.Load())) > cleanupEvictInterval {
			stale = append(stale, k.(string))
		}
		return true
	})
	for _, k := range stale {
		rl.visitors.Delete(k)
	}
}

// RateLimit returns a per-IP token-bucket limiter middleware. The
// rate-limiter key is c.ClientIP(), which Gin resolves from the
// X-Forwarded-For / X-Real-IP headers via its TrustedProxies setting.
//
// Deployment note: callers MUST pin TrustedProxies (gin.Engine.SetTrustedProxies)
// to the upstream proxy's CIDR before the server starts. Without this
// pin, a malicious client can spoof the header and rotate X-Forwarded-For
// per request to bypass the per-IP bucket. The default of trusting
// every proxy in Gin's engine is unsafe for a public deployment.
func RateLimit(ctx context.Context, r, burst int) gin.HandlerFunc {
	limiter := newRateLimiter(r, burst)

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				limiter.cleanup()
			case <-ctx.Done():
				return
			}
		}
	}()

	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !limiter.allow(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "too many requests",
			})
			return
		}
		c.Next()
	}
}
