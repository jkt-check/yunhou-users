package llm

import (
	"sync"
	"time"
)

// KeyCooldown is how long a key is skipped after the upstream rejects it
// with 429/5xx. One minute matches typical per-minute rate windows.
const KeyCooldown = 60 * time.Second

// KeyPool is a round-robin pool of interchangeable API keys for one
// provider, with per-key cooldown. It deliberately does NOT retry or sleep:
// the caller (ChatService) decides whether to try another key.
type KeyPool struct {
	mu          sync.Mutex
	keys        []string
	next        int
	cooledUntil map[int]time.Time
}

func NewKeyPool(keys []string) *KeyPool {
	return &KeyPool{keys: keys, cooledUntil: map[int]time.Time{}}
}

// Len reports the pool size (ChatService uses it to decide the retry budget).
func (p *KeyPool) Len() int { return len(p.keys) }

// Acquire returns the next non-cooled key in round-robin order. When every
// key is cooled down it returns the key whose cooldown expires soonest —
// an extra few seconds of 429 risk beats hard-failing the request.
func (p *KeyPool) Acquire(now time.Time) (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, bestUntil := -1, time.Time{}
	for i := 0; i < len(p.keys); i++ {
		idx := (p.next + i) % len(p.keys)
		until, cooled := p.cooledUntil[idx]
		if !cooled || !until.After(now) {
			p.next = idx + 1
			return idx, p.keys[idx]
		}
		if best == -1 || until.Before(bestUntil) {
			best, bestUntil = idx, until
		}
	}
	p.next = best + 1
	return best, p.keys[best]
}

// Cool marks key idx as unavailable until the given time. Out-of-range
// indexes are ignored (defensive; callers pass Acquire's return value).
func (p *KeyPool) Cool(idx int, until time.Time) {
	if idx < 0 || idx >= len(p.keys) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooledUntil[idx] = until
}
