package llm

import (
	"testing"
	"time"
)

func TestKeyPool_RoundRobin(t *testing.T) {
	p := NewKeyPool([]string{"a", "b", "c"})
	now := time.Now()
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		_, k := p.Acquire(now)
		seen[k]++
	}
	for _, k := range []string{"a", "b", "c"} {
		if seen[k] != 1 {
			t.Errorf("key %q acquired %d times, want 1", k, seen[k])
		}
	}
}

func TestKeyPool_SkipsCooledKeys(t *testing.T) {
	p := NewKeyPool([]string{"a", "b"})
	now := time.Now()
	_, first := p.Acquire(now) // "a"
	p.Cool(0, now.Add(time.Minute))
	_, second := p.Acquire(now)
	if second != "b" || first != "a" {
		t.Errorf("acquire = %q then %q, want a then b", first, second)
	}
	// Cooldown expiry makes the key available again.
	_, third := p.Acquire(now.Add(2 * time.Minute))
	if third != "a" && third != "b" {
		t.Errorf("impossible key %q", third)
	}
}

func TestKeyPool_AllCooledFallsBackToSoonestExpiring(t *testing.T) {
	p := NewKeyPool([]string{"a", "b"})
	now := time.Now()
	p.Cool(0, now.Add(10*time.Second))
	p.Cool(1, now.Add(5*time.Second))
	idx, key := p.Acquire(now)
	if key != "b" || idx != 1 {
		t.Errorf("Acquire = (%d, %q), want (1, b) (soonest expiring cooldown)", idx, key)
	}
}

func TestKeyPool_SingleKey(t *testing.T) {
	p := NewKeyPool([]string{"only"})
	if p.Len() != 1 {
		t.Fatalf("Len = %d", p.Len())
	}
	p.Cool(0, time.Now().Add(time.Hour))
	if _, k := p.Acquire(time.Now()); k != "only" {
		t.Errorf("single key pool must always return its key, got %q", k)
	}
}
