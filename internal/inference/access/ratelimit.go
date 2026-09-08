package access

import (
	"context"
	"sync"
	"time"
)

// ratelimit.go — 按账户/Key 的 RPM 限速原语（Task 5）。
//
// Sliding-window RPM counter over two span-aligned buckets: the estimate
// is curr + prev*(1 - elapsed/span), the standard sliding-window
// approximation. The clock is injectable so tests drive window rollover
// deterministically. Process-local by design: cross-instance coordination
// and concurrency leases are Task 7's database leases; the per-IP bucket
// (middleware.RateLimit) stays as the outer perimeter guard only.

type window struct {
	// start is the span-aligned start of the CURRENT bucket.
	start time.Time
	prev  int64
	curr  int64
	seen  time.Time
}

// RPMCounter is a goroutine-safe sliding-window counter keyed by an
// arbitrary scope string ("key:<id>", "acct:<id>").
type RPMCounter struct {
	mu      sync.Mutex
	now     func() time.Time
	span    time.Duration
	buckets map[string]*window
}

// NewRPMCounter builds a counter with a one-minute span; a nil clock uses
// the wall clock.
func NewRPMCounter(now func() time.Time) *RPMCounter {
	if now == nil {
		now = time.Now
	}
	return &RPMCounter{now: now, span: time.Minute, buckets: make(map[string]*window)}
}

// Allow records one request for key and reports whether it fits under
// limit requests per span. limit <= 0 disables limiting.
func (c *RPMCounter) Allow(key string, limit int) bool {
	if limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.bucketLocked(key)
	if w.estimate(c.now(), c.span) >= float64(limit) {
		return false
	}
	w.curr++
	return true
}

// RetryAfter estimates how long until the window rolls enough to admit
// another request for key — the computable recovery time the API layer
// may expose as Retry-After (设计 §9.1: 只有可计算恢复时间时设置).
// Returns 0 when the key has no live bucket.
func (c *RPMCounter) RetryAfter(key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.buckets[key]
	if !ok {
		return 0
	}
	now := c.now()
	if w.estimate(now, c.span) <= 0 {
		return 0
	}
	elapsed := now.Sub(w.start)
	if elapsed < 0 {
		elapsed = 0
	}
	if remaining := c.span - elapsed; remaining > 0 {
		return remaining
	}
	return 0
}

// Sweep evicts buckets idle for longer than idle, bounding table growth.
func (c *RPMCounter) Sweep(idle time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := c.now().Add(-idle)
	for k, w := range c.buckets {
		if w.seen.Before(cutoff) {
			delete(c.buckets, k)
		}
	}
}

// RunJanitor sweeps every interval until ctx is done. The janitor cadence
// uses the real clock; Allow/Sweep semantics stay driven by the injected
// clock.
func (c *RPMCounter) RunJanitor(ctx context.Context, interval, idle time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			c.Sweep(idle)
		case <-ctx.Done():
			return
		}
	}
}

// bucketLocked returns the key's window, rolling buckets when the span
// boundary has passed. Rolls by exactly one span when the gap is one;
// beyond that both buckets reset (long-idle keys start fresh).
func (c *RPMCounter) bucketLocked(key string) *window {
	now := c.now()
	start := now.Truncate(c.span)
	w, ok := c.buckets[key]
	if !ok {
		w = &window{start: start}
		c.buckets[key] = w
	} else if !start.Equal(w.start) {
		if start.Sub(w.start) == c.span {
			w.prev = w.curr
		} else {
			w.prev = 0
		}
		w.curr = 0
		w.start = start
	}
	w.seen = now
	return w
}

// estimate is the sliding-window count: current bucket plus the
// fractional remainder of the previous one.
func (w *window) estimate(now time.Time, span time.Duration) float64 {
	elapsed := now.Sub(w.start)
	frac := 1 - float64(elapsed)/float64(span)
	if frac < 0 {
		frac = 0
	}
	return float64(w.curr) + float64(w.prev)*frac
}
