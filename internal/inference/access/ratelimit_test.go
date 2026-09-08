package access

import (
	"context"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic window tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func TestRPMCounter_BasicAllowAndReject(t *testing.T) {
	fc := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	c := NewRPMCounter(fc.now)

	for i := 0; i < 3; i++ {
		if !c.Allow("key:1", 3) {
			t.Fatalf("request %d rejected under limit", i+1)
		}
	}
	if c.Allow("key:1", 3) {
		t.Error("4th request within the window accepted")
	}
	// A different key has its own bucket.
	if !c.Allow("key:2", 3) {
		t.Error("independent key rejected")
	}
	// limit <= 0 disables limiting.
	if !c.Allow("key:1", 0) {
		t.Error("limit=0 should be unlimited")
	}
}

func TestRPMCounter_SlidingWindow(t *testing.T) {
	fc := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 50, 0, time.UTC)}
	c := NewRPMCounter(fc.now)

	// Fill the limit in the 12:00 bucket.
	for i := 0; i < 2; i++ {
		if !c.Allow("k", 2) {
			t.Fatalf("fill %d rejected", i)
		}
	}
	if c.Allow("k", 2) {
		t.Error("window should be saturated")
	}
	// 30s into the next bucket the previous one still contributes ~2/3:
	// estimate = 0 + 2*(1-20/60) ≈ 1.33 < 2 → exactly one more fits.
	fc.advance(30 * time.Second)
	if !c.Allow("k", 2) {
		t.Error("sliding estimate should admit one more after partial rollover")
	}
	if c.Allow("k", 2) {
		t.Error("sliding estimate should still reject once saturated")
	}
	// More than two spans later the buckets reset entirely.
	fc.advance(3 * time.Minute)
	if !c.Allow("k", 2) || !c.Allow("k", 2) {
		t.Error("post-reset requests rejected")
	}
}

func TestRPMCounter_LongIdleResets(t *testing.T) {
	fc := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	c := NewRPMCounter(fc.now)
	c.Allow("k", 1)
	fc.advance(10 * time.Minute)
	if !c.Allow("k", 1) {
		t.Error("long-idle key should start fresh")
	}
}

func TestRPMCounter_RetryAfter(t *testing.T) {
	fc := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 15, 0, time.UTC)}
	c := NewRPMCounter(fc.now)
	if got := c.RetryAfter("missing"); got != 0 {
		t.Errorf("unknown key RetryAfter = %v, want 0", got)
	}
	c.Allow("k", 1) // saturates the bucket
	if got := c.RetryAfter("k"); got <= 0 || got > time.Minute {
		t.Errorf("RetryAfter = %v, want (0, 60s]", got)
	}
	fc.advance(30 * time.Second)
	if got := c.RetryAfter("k"); got != 15*time.Second {
		t.Errorf("RetryAfter after 30s = %v, want 15s", got)
	}
}

func TestRPMCounter_Sweep(t *testing.T) {
	fc := &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
	c := NewRPMCounter(fc.now)
	c.Allow("old", 10)
	fc.advance(3 * time.Minute)
	c.Allow("new", 10)
	c.Sweep(2 * time.Minute)
	if _, ok := c.buckets["old"]; ok {
		t.Error("idle bucket not swept")
	}
	if _, ok := c.buckets["new"]; !ok {
		t.Error("live bucket swept")
	}
}

func TestRPMCounter_JanitorStops(t *testing.T) {
	c := NewRPMCounter(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.RunJanitor(ctx, time.Hour, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("janitor did not stop on ctx cancel")
	}
}
