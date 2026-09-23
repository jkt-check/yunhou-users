package credentials

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// service_edges_test.go — Task 16 覆盖率补强：Service 的 List/Get/校验与
// 代次钉住分支（memStore 基建已在 service_test.go）。

func TestServiceListAndGet(t *testing.T) {
	v, err := NewVault(testKeys(t, 1), 1)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(v, newMemStore(), &memRecorder{})
	ctx := context.Background()

	view1, err := svc.Create(ctx, testOp(), "prov-a", "k1", "api_key", "sk-1", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, testOp(), "prov-a", "k2", "api_key", "sk-2", "r2", nil); err != nil {
		t.Fatal(err)
	}
	views, err := svc.List(ctx, "prov-a", 10)
	if err != nil || len(views) != 2 {
		t.Fatalf("list = %d/%v", len(views), err)
	}
	for _, vw := range views {
		if vw.Status == "" || vw.ID == "" {
			t.Errorf("view shape = %+v", vw)
		}
	}
	got, err := svc.Get(ctx, view1.ID)
	if err != nil || got.ID != view1.ID {
		t.Fatalf("get = %+v/%v", got, err)
	}
	if _, err := svc.Get(ctx, "no-such-id"); err == nil {
		t.Fatal("get unknown must fail")
	}
}

func TestServiceCreateValidationEdges(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	ctx := context.Background()

	for name, args := range map[string][6]string{
		"empty provider": {"", "label", "api_key", "sk", "reason", ""},
		"bad auth type":  {"prov", "label", "magic", "sk", "reason", ""},
		"empty secret":   {"prov", "label", "api_key", "", "reason", ""},
	} {
		if _, err := svc.Create(ctx, testOp(), args[0], args[1], args[2], args[3], args[4], nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestServiceResolvePinnedGenerationBranches(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	ctx := context.Background()

	view, err := svc.Create(ctx, testOp(), "prov", "k", "api_key", "sk-top", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 钉住当前代次 → 放行。
	pin := view.Generation
	plain, cred, err := svc.ResolveSecret(ctx, view.ID, &pin)
	if err != nil || string(plain) != "sk-top" || cred.ID != view.ID {
		t.Fatalf("pinned resolve = %v/%+v", err, cred)
	}
	// 钉住旧代次 → 冲突（在途请求不得静默用新秘密）。
	stale := view.Generation + 99
	if _, _, err := svc.ResolveSecret(ctx, view.ID, &stale); err == nil ||
		domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale pin = %v, want conflict", err)
	}
	// 不钉 → 取当前。
	if _, _, err := svc.ResolveSecret(ctx, view.ID, nil); err != nil {
		t.Fatalf("unpinned resolve = %v", err)
	}
}

func TestServiceSetStatusBranches(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	svc := NewService(v, newMemStore(), &memRecorder{})
	ctx := context.Background()

	view, err := svc.Create(ctx, testOp(), "prov", "k", "api_key", "sk", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 合法状态集为 active/rotating/revoked：limbo 与 disabled 都拒绝。
	for _, bad := range []string{"limbo", "disabled"} {
		if _, err := svc.SetStatus(ctx, testOp(), view.ID, bad, "x"); err == nil {
			t.Errorf("status %q accepted", bad)
		}
	}
	// revoked → active 恢复（审计各一条）。
	if _, err := svc.SetStatus(ctx, testOp(), view.ID, "revoked", "compromised"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSecret(ctx, view.ID, nil); err == nil {
		t.Error("revoked credential must not resolve")
	}
	if _, err := svc.SetStatus(ctx, testOp(), view.ID, "active", "restore"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSecret(ctx, view.ID, nil); err != nil {
		t.Error("restored credential must resolve")
	}
}
