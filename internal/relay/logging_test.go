package relay

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// captureWarnLog redirects the standard logger into a buffer so throttle
// behavior can be asserted on emitted lines.
func captureWarnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestWarnThrottled_ThrottlesSameKey(t *testing.T) {
	buf := captureWarnLog(t)
	w := newWarnThrottled()
	w.Logf("k1", "hello fail %s", "a")
	w.Logf("k1", "hello fail %s", "b") // throttled: same key within 1/min
	w.Logf("k2", "hello fail %s", "c") // distinct key: logged
	if got := strings.Count(buf.String(), "hello fail"); got != 2 {
		t.Errorf("logged lines = %d, want 2 (k1 throttled, k2 logged): %q", got, buf.String())
	}
}

// TestWarnThrottled_EvictsStaleKeys pins the eviction sweep: keys untouched
// for over 2× the throttle window are dropped so an attacker rotating
// IP-hash / client_id keys cannot grow the map without bound.
func TestWarnThrottled_EvictsStaleKeys(t *testing.T) {
	now := time.Now()
	w := newWarnThrottled()
	w.last["stale"] = now.Add(-3 * time.Minute)
	w.last["fresh"] = now.Add(-30 * time.Second)
	w.evictStaleLocked(now)
	if _, ok := w.last["stale"]; ok {
		t.Error("stale key (3m idle) should have been evicted")
	}
	if _, ok := w.last["fresh"]; !ok {
		t.Error("fresh key (30s idle) must survive the sweep")
	}
}

// TestWarnThrottled_SweepRunsOnLogf: the sweep rides the write path — once
// the sweep interval has elapsed, the next Logf evicts stale entries.
func TestWarnThrottled_SweepRunsOnLogf(t *testing.T) {
	captureWarnLog(t)
	w := newWarnThrottled()
	// Pretend the last sweep ran long ago and a stale key accumulated.
	w.lastSweep = time.Now().Add(-2 * time.Minute)
	w.last["stale"] = time.Now().Add(-3 * time.Minute)
	w.Logf("trigger", "boom")
	w.mu.Lock()
	_, staleKept := w.last["stale"]
	w.mu.Unlock()
	if staleKept {
		t.Error("Logf past the sweep interval should have evicted the stale key")
	}
}
