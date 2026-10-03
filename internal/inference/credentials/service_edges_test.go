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

// 评审修复（组6-B）：rotate 只替换秘密材料，不做生命周期状态迁移——
// rotating 状态的凭据 rotate 后，响应 View 与库存行都必须仍是 rotating
// （rotateCredentialSecret SQL 不碰 status；状态迁移归 SetStatus 所有）。
// 旧实现把 View 谎报成 active，与库存真实状态不一致。
func TestServiceRotateKeepsLifecycleStatus(t *testing.T) {
	v, _ := NewVault(testKeys(t, 1), 1)
	store := newMemStore()
	svc := NewService(v, store, &memRecorder{})
	ctx := context.Background()

	view, err := svc.Create(ctx, testOp(), "prov", "k", "api_key", "sk-1", "r", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStatus(ctx, testOp(), view.ID, "rotating", "key swap underway"); err != nil {
		t.Fatal(err)
	}
	rotated, err := svc.Rotate(ctx, testOp(), view.ID, "sk-2", "swap secret")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Status != "rotating" {
		t.Errorf("rotated view status = %q, want rotating", rotated.Status)
	}
	stored, err := store.GetCredential(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "rotating" || stored.Generation != 2 {
		t.Errorf("stored row = status %q generation %d, want rotating/2", stored.Status, stored.Generation)
	}

	// 顺向不变量：active 凭据 rotate 后仍是 active。
	if _, err := svc.SetStatus(ctx, testOp(), view.ID, "active", "swap done"); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Rotate(ctx, testOp(), view.ID, "sk-3", "second swap")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "active" {
		t.Errorf("active credential rotated: status = %q, want active", again.Status)
	}
}
