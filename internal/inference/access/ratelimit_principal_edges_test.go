package access

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// ratelimit_principal_edges_test.go — Task 16 覆盖率补强：RPM 计数器的
// RetryAfter/janitor 与 Resolver.Authenticate 的分支（fake store）。

func TestRPMCounter_RetryAfterAndJanitor(t *testing.T) {
	now := time.Now()
	cur := now
	c := NewRPMCounter(func() time.Time { return cur })

	// 未知桶 → 0。
	if got := c.RetryAfter("ghost"); got != 0 {
		t.Errorf("unknown bucket retry-after = %v", got)
	}
	// 打满窗口（3/min）→ RetryAfter > 0；窗口推进后恢复。
	for i := 0; i < 3; i++ {
		if !c.Allow("k", 3) {
			t.Fatalf("call %d must be allowed", i)
		}
	}
	if c.Allow("k", 3) {
		t.Fatal("4th call in window must be limited")
	}
	if got := c.RetryAfter("k"); got <= 0 {
		t.Errorf("saturated bucket retry-after = %v, want > 0", got)
	}
	// 无限额（0）→ 不限制。
	if !c.Allow("k", 0) {
		t.Error("unlimited key must always pass")
	}
	// 窗口滑动：60s 后旧事件退出 → 再次放行。
	cur = cur.Add(61 * time.Second)
	if !c.Allow("k", 3) {
		t.Error("post-window call must pass")
	}
	// janitor：空闲超阈值后桶被清扫（RetryAfter 归零）。
	cur = cur.Add(31 * time.Second)
	c.Sweep(30 * time.Second)
	if got := c.RetryAfter("k"); got != 0 {
		t.Errorf("swept bucket retry-after = %v", got)
	}
	// RunJanitor 随 ctx 取消退出（不泄漏 goroutine 循环）。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.RunJanitor(ctx, 5*time.Millisecond, time.Millisecond); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not stop on cancel")
	}
}

func TestResolver_AuthenticateBranches(t *testing.T) {
	fs := newFakeStore()
	r := NewResolver(fs, nil)
	ctx := context.Background()

	// 形状非法（无 yk- 前缀）→ invalid。
	if _, err := r.Authenticate(ctx, "not-a-key"); err == nil {
		t.Error("malformed key accepted")
	}
	// 前缀不存在 → invalid。
	if _, err := r.Authenticate(ctx, "yk-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); err == nil {
		t.Error("unknown prefix accepted")
	}
	// 建一个真 key：伪造/吊销/过期的断言。
	acct, _ := fs.EnsureBillingAccount(ctx, "user-a")
	fs.ents = append(fs.ents, domain.Entitlement{
		ID: uuid.NewString(), BillingAccountID: acct.ID, Status: domain.EntitlementActive,
		ModelIDs: []string{"glm-4.6"},
	})
	svc := NewKeyService(fs, nil)
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatal(err)
	}
	// 真 key 放行。
	res, err := r.Authenticate(ctx, created.Plaintext)
	if err != nil || res.Principal.BillingAccountID != acct.ID {
		t.Fatalf("authenticate = %+v/%v", res, err)
	}
	// 伪造（改一个字符）→ invalid。
	forged := created.Plaintext[:len(created.Plaintext)-1] + "X"
	if _, err := r.Authenticate(ctx, forged); err == nil {
		t.Error("forged key accepted")
	}
	// 吊销 → invalid（下一次调用即生效）。
	if _, err := svc.RevokeKey(ctx, "user-a", created.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(ctx, created.Plaintext); err == nil {
		t.Error("revoked key accepted")
	}
}
