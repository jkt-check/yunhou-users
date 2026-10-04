package access

import (
	"context"
	"testing"
	"time"
)

// principal_touch_test.go — last_used 遥测写入节流（内存 fake store + 手动
// 时钟；同一 key 窗口内最多落库一次，消失 key 的节流条目被概率性清扫）。

func TestAuthenticate_TouchLastUsedThrottled(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	r := NewResolver(fs, clock)
	ctx := context.Background()
	plaintext, keyID, _ := mintKey(t, fs, svc, "user-a")

	if _, err := r.Authenticate(ctx, plaintext); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(fs.touched) != 1 || fs.touched[0] != keyID {
		t.Fatalf("first touch = %v, want [%s]", fs.touched, keyID)
	}
	// 窗口内重复请求直接跳过落库。
	for i := 0; i < 5; i++ {
		if _, err := r.Authenticate(ctx, plaintext); err != nil {
			t.Fatalf("Authenticate #%d: %v", i, err)
		}
	}
	if len(fs.touched) != 1 {
		t.Fatalf("in-window touches = %v, want still 1 write", fs.touched)
	}

	// 节流按 keyID 隔离：另一把 key 的首次请求照常落库。
	plaintext2, keyID2, _ := mintKey(t, fs, svc, "user-b")
	if _, err := r.Authenticate(ctx, plaintext2); err != nil {
		t.Fatalf("Authenticate key2: %v", err)
	}
	if len(fs.touched) != 2 || fs.touched[1] != keyID2 {
		t.Fatalf("other key must not share the window: %v", fs.touched)
	}

	// 窗口过后重新落库。
	now = now.Add(lastUsedTouchWindow + time.Second)
	if _, err := r.Authenticate(ctx, plaintext); err != nil {
		t.Fatalf("post-window Authenticate: %v", err)
	}
	if len(fs.touched) != 3 || fs.touched[2] != keyID {
		t.Fatalf("post-window touch = %v, want key1 written again", fs.touched)
	}
}

func TestTouchLastUsed_SweepsStaleEntries(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	r := NewResolver(fs, clock)
	ctx := context.Background()
	plaintext, keyID, _ := mintKey(t, fs, svc, "user-a")

	// 预置一个消失 key 的陈旧条目，并把写入计数推到清扫阈值前——下一次
	// 真实写入触发顺带清扫。
	r.lastTouched["ghost-key"] = now.Add(-3 * lastUsedTouchWindow)
	r.touchWrites = lastUsedSweepEvery - 1

	if _, err := r.Authenticate(ctx, plaintext); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	r.touchMu.Lock()
	_, ghostKept := r.lastTouched["ghost-key"]
	_, curKept := r.lastTouched[keyID]
	r.touchMu.Unlock()
	if ghostKept {
		t.Error("stale ghost-key entry must be swept")
	}
	if !curKept {
		t.Error("fresh entry must survive the sweep")
	}
}
