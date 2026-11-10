package access

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// principal_authenticate_edges_test.go — Resolver.Authenticate 的分支覆盖
// （fake store）。
//
// Task 4：原来的进程内 RPM 计数器（access.RPMCounter）已删除 —— 业务速率
// 限制的唯一权威是业务准入里的持久速率预占（PostgreSQL 共享）。认证层
// 只做认证。

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
