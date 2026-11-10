package access

// apikey_concurrency_test.go — Task 5：Key 级并发上限（concurrency_limit）
// 在管理服务层的字段语义。与 rpm_limit 同构：缺省不加限制、PATCH 缺席不
// 修改、显式 null 清除、非正整数拒绝；所有权检查不变。

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// TestCreateKey_ConcurrencyLimitSemantics: 创建时的缺省/设置/拒绝。
func TestCreateKey_ConcurrencyLimitSemantics(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()

	// 缺省：不增加 Key 级并发限制。
	plain, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if plain.Key.ConcurrencyLimit != nil {
		t.Errorf("concurrency limit = %d, want nil (缺省不加 Key 限制)", *plain.Key.ConcurrencyLimit)
	}

	// 显式设置：落库。
	limit := 3
	capped, err := svc.CreateKey(ctx, "user-a", CreateParams{ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatalf("CreateKey with a limit: %v", err)
	}
	if capped.Key.ConcurrencyLimit == nil || *capped.Key.ConcurrencyLimit != 3 {
		t.Errorf("concurrency limit = %v, want 3", capped.Key.ConcurrencyLimit)
	}
	stored, err := svc.GetKey(ctx, "user-a", capped.Key.ID)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if stored.ConcurrencyLimit == nil || *stored.ConcurrencyLimit != 3 {
		t.Errorf("stored concurrency limit = %v, want 3", stored.ConcurrencyLimit)
	}

	// 非正整数拒绝（0 与负数）。
	for _, bad := range []int{0, -1} {
		b := bad
		if _, err := svc.CreateKey(ctx, "user-a", CreateParams{ConcurrencyLimit: &b}); err == nil ||
			domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("create with concurrency_limit=%d: err = %v, want invalid_input", bad, err)
		}
	}
}

// TestUpdateKey_ConcurrencyLimitPresenceSemantics: PATCH 的三种存在形态。
func TestUpdateKey_ConcurrencyLimitPresenceSemantics(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()
	limit := 4
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{ConcurrencyLimit: &limit})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	id := created.Key.ID

	// PATCH 缺席该字段：不修改现值。
	updated, err := svc.UpdateKey(ctx, "user-a", id, KeyPatch{Name: strPtr("renamed")})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if updated.ConcurrencyLimit == nil || *updated.ConcurrencyLimit != 4 {
		t.Errorf("concurrency limit after an unrelated patch = %v, want 4 (PATCH 缺席不修改)", updated.ConcurrencyLimit)
	}

	// PATCH 显式设置。
	five := 5
	updated, err = svc.UpdateKey(ctx, "user-a", id, KeyPatch{ConcurrencyLimit: &five})
	if err != nil {
		t.Fatalf("UpdateKey with a limit: %v", err)
	}
	if updated.ConcurrencyLimit == nil || *updated.ConcurrencyLimit != 5 {
		t.Errorf("concurrency limit = %v, want 5", updated.ConcurrencyLimit)
	}

	// PATCH 显式 null（清除）：解除 Key 级限制。
	updated, err = svc.UpdateKey(ctx, "user-a", id, KeyPatch{ClearConcurrency: true})
	if err != nil {
		t.Fatalf("UpdateKey clearing the limit: %v", err)
	}
	if updated.ConcurrencyLimit != nil {
		t.Errorf("concurrency limit after an explicit clear = %d, want nil", *updated.ConcurrencyLimit)
	}

	// 非正整数拒绝，且现值不变。
	zero := 0
	if _, err := svc.UpdateKey(ctx, "user-a", id, KeyPatch{ConcurrencyLimit: &zero}); err == nil ||
		domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("patch with concurrency_limit=0: err = %v, want invalid_input", err)
	}
	stored, err := svc.GetKey(ctx, "user-a", id)
	if err != nil {
		t.Fatalf("GetKey: %v", err)
	}
	if stored.ConcurrencyLimit != nil {
		t.Errorf("concurrency limit after a rejected patch = %d, want nil (被拒的写入不改现值)", *stored.ConcurrencyLimit)
	}

	// 所有权检查不变：别人的 Key 一律 not_found。
	if _, err := svc.UpdateKey(ctx, "user-b", id, KeyPatch{ConcurrencyLimit: &five}); err == nil ||
		domain.CodeOf(err) != domain.CodeNotFound {
		t.Errorf("cross-customer patch: err = %v, want not_found", err)
	}
}

func strPtr(s string) *string { return &s }
