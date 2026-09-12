package management

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// operations_service_edges_test.go — Task 16 覆盖率补强：运营统计服务的
// 校验与编排分支（group_by 校验、provider_id 收敛、异常筛选三种 kind、
// 共享账号时间窗）。

type fakeOpsStore struct {
	filter   OpsUsageFilter
	excKind  string
	excLimit int
	adjLimit int
	shared   struct {
		from, to time.Time
		limit    int
	}
}

func (f *fakeOpsStore) SummarizeOpsUsage(ctx context.Context, flt OpsUsageFilter) ([]OpsGroup, error) {
	f.filter = flt
	return []OpsGroup{}, nil
}
func (f *fakeOpsStore) ListStuckReservations(ctx context.Context, olderThan time.Time, limit int) ([]StuckReservation, error) {
	f.excKind, f.excLimit = "stuck", limit
	return []StuckReservation{}, nil
}
func (f *fakeOpsStore) ListReauthAccounts(ctx context.Context, limit int) ([]ReauthAccount, error) {
	f.excKind, f.excLimit = "reauth", limit
	return []ReauthAccount{}, nil
}
func (f *fakeOpsStore) ListSettlementBacklog(ctx context.Context, now time.Time, limit int) ([]BacklogJob, error) {
	f.excKind, f.excLimit = "backlog", limit
	return []BacklogJob{}, nil
}
func (f *fakeOpsStore) ListSharedDeploymentAccounts(ctx context.Context, from, to time.Time, limit int) ([]SharedDeploymentAccount, error) {
	f.shared.from, f.shared.to, f.shared.limit = from, to, limit
	return []SharedDeploymentAccount{}, nil
}
func (f *fakeOpsStore) ListAdjustments(ctx context.Context, accountID string, limit int) ([]AdjustmentView, error) {
	f.adjLimit = limit
	return []AdjustmentView{}, nil
}

func TestOpsSummary_ValidationBranches(t *testing.T) {
	fs := &fakeOpsStore{}
	svc := NewOperationsService(fs, nil)
	ctx := context.Background()

	// 非法 group_by → invalid_input。
	if _, err := svc.Summary(ctx, OpsUsageFilter{GroupBy: "region"}); err == nil ||
		domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("bad group_by = %v", err)
	}
	// provider_id 只在 provider 分组下合法。
	if _, err := svc.Summary(ctx, OpsUsageFilter{GroupBy: OpsGroupModel, ProviderID: "p1"}); err == nil {
		t.Error("provider_id outside provider grouping accepted")
	}
	// 默认 group_by=model + 默认时间范围（30 天）。
	out, err := svc.Summary(ctx, OpsUsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if out.GroupBy != OpsGroupModel {
		t.Errorf("default group_by = %s", out.GroupBy)
	}
	if fs.filter.From.IsZero() || fs.filter.To.IsZero() {
		t.Errorf("default range not resolved: %+v", fs.filter)
	}
	if out.AsOf != out.CompleteThrough {
		t.Errorf("as_of/complete_through = %v/%v (直读权威表二者相等)", out.AsOf, out.CompleteThrough)
	}
	// provider 分组带收敛。
	if _, err := svc.Summary(ctx, OpsUsageFilter{GroupBy: OpsGroupProvider, ProviderID: "p1"}); err != nil {
		t.Fatal(err)
	}
	// 非法时间范围（from >= to）→ invalid。
	from := time.Now()
	if _, err := svc.Summary(ctx, OpsUsageFilter{From: from, To: from.Add(-time.Hour)}); err == nil {
		t.Error("inverted range accepted")
	}
}

func TestOpsExceptions_AllKindsAndClamp(t *testing.T) {
	fs := &fakeOpsStore{}
	svc := NewOperationsService(fs, nil)
	ctx := context.Background()

	// 三种 kind 全覆盖 + limit 钳制（9999 → 500）。
	for _, kind := range []string{ExceptionStuckReservations, ExceptionReauthAccounts, ExceptionSettlementBacklog} {
		out, err := svc.Exceptions(ctx, kind, 9999)
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != kind {
			t.Errorf("kind = %s", out.Kind)
		}
		if fs.excLimit != 500 {
			t.Errorf("limit = %d, want clamped 500", fs.excLimit)
		}
	}
	// 非法 kind → invalid。
	if _, err := svc.Exceptions(ctx, "mystery", 10); err == nil {
		t.Error("bad kind accepted")
	}

	// SharedDeploymentAccounts：默认 7 天窗 + 非法范围。
	to := time.Now().UTC()
	if _, err := svc.SharedDeploymentAccounts(ctx, time.Time{}, time.Time{}, 9999); err != nil {
		t.Fatal(err)
	}
	if fs.shared.limit != 500 || fs.shared.to.Sub(fs.shared.from) > 8*24*time.Hour {
		t.Errorf("shared defaults = %+v", fs.shared)
	}
	if _, err := svc.SharedDeploymentAccounts(ctx, to, to.Add(-time.Hour), 10); err == nil {
		t.Error("inverted shared range accepted")
	}
	if _, err := svc.SharedDeploymentAccounts(ctx, to.Add(-200*24*time.Hour), to, 10); err == nil {
		t.Error(">92d shared range accepted")
	}

	// Adjustments：limit 钳制。
	if _, err := svc.Adjustments(ctx, "", 9999); err != nil {
		t.Fatal(err)
	}
	if fs.adjLimit != 500 {
		t.Errorf("adjustments limit = %d, want 500", fs.adjLimit)
	}
}
