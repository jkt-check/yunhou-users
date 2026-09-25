package catalog_test

import (
	"context"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// lifecycle_i1_test.go — 安全审查 I-1：catalog service 层的生命周期状态机
// 兜底。httpapi 层拒收只是第一道门；service 层必须保证任何调用方（bulk
// import、未来新端点）都无法直建 active 或经 UpdateModel 重置生命周期。

func TestCreateModel_RejectsNonDraftLifecycle(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()

	for _, lc := range []domain.Lifecycle{
		domain.LifecycleActive, domain.LifecycleDeprecated, domain.LifecycleRetired,
	} {
		m := sampleModel("i1-create-" + string(lc))
		m.Lifecycle = lc
		if err := svc.CreateModel(ctx, &m); domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("create with lifecycle=%s: err = %v, want invalid_input", lc, err)
		}
	}

	// 显式 draft 与缺省都合法，且落库即 draft。
	for name, lc := range map[string]domain.Lifecycle{"draft": domain.LifecycleDraft, "omit": ""} {
		m := sampleModel("i1-create-d-" + name)
		m.Lifecycle = lc
		if err := svc.CreateModel(ctx, &m); err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
		if m.Lifecycle != domain.LifecycleDraft {
			t.Errorf("%s: lifecycle = %s, want draft", name, m.Lifecycle)
		}
	}
}

func TestUpdateModel_RejectsMissingLifecycle(t *testing.T) {
	_, _, svc := testDB(t)
	ctx := context.Background()

	m := sampleModel("i1-update-nolc")
	if err := svc.CreateModel(ctx, &m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 部分对象（生命周期字段为空）不再被静默重置为 draft——原先这正是
	// 「改个 display_name 就把在售模型打回草稿」的漏洞根因。
	m.Lifecycle = ""
	if err := svc.UpdateModel(ctx, &m); domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("update without lifecycle: err = %v, want invalid_input", err)
	}

	// 带齐生命周期（read-modify-write 调用方契约）仍然可写。
	stored, err := svc.GetModel(ctx, "i1-update-nolc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	stored.DisplayName = "Renamed"
	if err := svc.UpdateModel(ctx, stored); err != nil {
		t.Errorf("update with lifecycle: %v", err)
	}
}
