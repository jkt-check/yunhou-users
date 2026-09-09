// account_pool_test.go — Task 12 账号池额度感知调度（纯逻辑，内存 fake）。
// 口径：已知耗尽（observed remaining=0 且 reset 未到）的账号不参与调度；
// 未知额度照常调度（未知保持未知）；reset 已过的旧零值重新可调度（健康
// worker 会复测）。

package routing

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

func accountWithQuota(id, providerID string, remaining *int64, resetAt *time.Time) domain.UpstreamAccount {
	a := account(id, providerID, 1)
	if remaining != nil {
		m := domain.Microcredit(*remaining)
		a.Quota.RemainingMicros = &m
	}
	a.Quota.ResetAt = resetAt
	return a
}

func int64p(v int64) *int64 { return &v }

func candidatesForAccounts(t *testing.T, accounts []domain.UpstreamAccount) []Candidate {
	t.Helper()
	fs := newFakeStore()
	fs.accounts["prov"] = accounts
	svc := NewService(fs, map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat: providers.NewOpenAIChat(),
	}, nil)
	route := domain.ModelRoute{
		ID: "r1", ModelID: "m", DeploymentID: "dep1",
		Priority: 1, Weight: 1, Enabled: true,
	}
	dep := deployment("dep1", "prov", domain.ProtocolOpenAIChat)
	snap := testSnapshot("m", []domain.ModelRoute{route}, dep)
	out, err := svc.Candidates(context.Background(), snap, "m", Needs{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAccountPool_QuotaExhaustedSkipped(t *testing.T) {
	future := time.Now().Add(time.Hour)
	out := candidatesForAccounts(t, []domain.UpstreamAccount{
		accountWithQuota("a-exhausted", "prov", int64p(0), &future), // 耗尽且未到 reset
		accountWithQuota("a-live", "prov", int64p(100), &future),
	})
	if len(out) != 1 || out[0].Account.ID != "a-live" {
		ids := []string{}
		for _, c := range out {
			ids = append(ids, c.Account.ID)
		}
		t.Fatalf("exhausted account must be skipped, got %v", ids)
	}
}

func TestAccountPool_UnknownQuotaSchedulable(t *testing.T) {
	out := candidatesForAccounts(t, []domain.UpstreamAccount{
		accountWithQuota("a-unknown", "prov", nil, nil), // 未知保持未知 → 可调度
	})
	if len(out) != 1 {
		t.Fatalf("unknown quota must stay schedulable, got %d", len(out))
	}
}

func TestAccountPool_StaleZeroAfterResetSchedulable(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	out := candidatesForAccounts(t, []domain.UpstreamAccount{
		accountWithQuota("a-stale-zero", "prov", int64p(0), &past), // reset 已过 → 旧观测失效
	})
	if len(out) != 1 {
		t.Fatalf("stale zero after reset must be schedulable, got %d", len(out))
	}
}

func TestAccountPool_ZeroNoResetExhausted(t *testing.T) {
	out := candidatesForAccounts(t, []domain.UpstreamAccount{
		accountWithQuota("a-zero-no-reset", "prov", int64p(0), nil), // 无 reset 信息的零余额 = 耗尽
	})
	if len(out) != 0 {
		t.Fatalf("zero remaining without reset must be exhausted, got %d", len(out))
	}
}

func TestAccountHealth_SchedulableComposition(t *testing.T) {
	fs := newFakeStore()
	svc := NewService(fs, nil, nil)
	future := time.Now().Add(time.Hour)

	ok := svc.HealthOf(accountWithQuota("a1", "prov", int64p(5), &future))
	if !ok.Schedulable || ok.QuotaRemaining == nil || *ok.QuotaRemaining != 5 {
		t.Fatalf("healthy account must be schedulable: %+v", ok)
	}
	exhausted := svc.HealthOf(accountWithQuota("a2", "prov", int64p(0), &future))
	if exhausted.Schedulable {
		t.Fatalf("exhausted account must not be schedulable: %+v", exhausted)
	}
	inactive := account("a3", "prov", 1)
	inactive.Status = domain.AccountReauthRequired
	if svc.HealthOf(inactive).Schedulable {
		t.Fatal("reauth_required account must not be schedulable")
	}
	cooled := account("a4", "prov", 1)
	svc.Cool("a4", time.Now().Add(time.Minute))
	h := svc.HealthOf(cooled)
	if h.Schedulable || h.CoolingUntil == nil {
		t.Fatalf("cooling account must report cooling window: %+v", h)
	}
}
