package access

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// apikey_edges_test.go — Task 16 覆盖率补强：Create/Update 的校验分支
// （fake store；生命周期与越权矩阵已在 apikey_test.go）。

func TestCreateKey_ValidationBranches(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()

	neg := int64(-1)
	zeroRPM := 0
	past := time.Now().Add(-time.Hour)
	for name, p := range map[string]CreateParams{
		"name too long":   {Name: strings.Repeat("n", 200)},
		"negative budget": {BudgetMicros: &neg},
		"zero rpm":        {RPMLimit: &zeroRPM},
		"past expiry":     {ExpiresAt: &past},
	} {
		if _, err := svc.CreateKey(ctx, "user-a", p); err == nil || domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("%s: err = %v, want invalid_input", name, err)
		}
	}
	// 超出权益的模型范围 → model_not_allowed。
	acct, _ := svc.EnsureAccount(ctx, "user-a")
	fs.ents = append(fs.ents, domain.Entitlement{
		ID: uuid.NewString(), BillingAccountID: acct.ID, Status: domain.EntitlementActive,
		ModelIDs: []string{"glm-4.6"},
	})
	if _, err := svc.CreateKey(ctx, "user-a", CreateParams{ModelAllow: []string{"gpt-x"}}); err == nil ||
		domain.CodeOf(err) != domain.CodeModelNotAllowed {
		t.Errorf("scope widening: err = %v, want model_not_allowed", err)
	}
	// 权益内范围 → 放行。
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{ModelAllow: []string{"glm-4.6"}})
	if err != nil || created.Plaintext == "" {
		t.Fatalf("in-scope create: %v", err)
	}
}

func TestUpdateKey_ValidationBranches(t *testing.T) {
	fs := newFakeStore()
	svc := NewKeyService(fs, nil)
	ctx := context.Background()
	acct, _ := svc.EnsureAccount(ctx, "user-a")
	fs.ents = append(fs.ents, domain.Entitlement{
		ID: uuid.NewString(), BillingAccountID: acct.ID, Status: domain.EntitlementActive,
		ModelIDs: []string{"glm-4.6"},
	})
	created, err := svc.CreateKey(ctx, "user-a", CreateParams{})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Key.ID

	long := strings.Repeat("n", 200)
	neg := int64(-1)
	zeroRPM := 0
	past := time.Now().Add(-time.Hour)
	for name, patch := range map[string]KeyPatch{
		"name too long":   {Name: &long},
		"negative budget": {BudgetMicros: &neg},
		"zero rpm":        {RPMLimit: &zeroRPM},
		"past expiry":     {ExpiresAt: &past},
	} {
		if _, err := svc.UpdateKey(ctx, "user-a", id, patch); err == nil || domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("%s: err = %v, want invalid_input", name, err)
		}
	}
	// ClearRPM / ClearExpires 生效。
	rpm := 30
	exp := time.Now().Add(time.Hour)
	if _, err := svc.UpdateKey(ctx, "user-a", id, KeyPatch{RPMLimit: &rpm, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdateKey(ctx, "user-a", id, KeyPatch{ClearRPM: true, ClearExpires: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.RPMLimit != nil || updated.ExpiresAt != nil {
		t.Errorf("clears not applied: %+v", updated)
	}
	// ListKeys 分页形状。
	page, err := svc.ListKeys(ctx, "user-a", 10, 0)
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("list = %+v/%v", page, err)
	}
	if _, err := svc.ListKeys(ctx, "user-b", 10, 0); err != nil {
		t.Fatalf("list for empty account must be empty page: %v", err)
	}
}
