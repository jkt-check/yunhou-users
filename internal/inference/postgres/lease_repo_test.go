// lease_repo_test.go — 评审修复批次8：终态租约收割（DeleteTerminalLeases）
// 与收割后准入/所有权语义的保持。

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// 终态行（released/expired）且终态时间早于窗口 → 收割；held 行与窗口内
// 的终态行 → 保留；批上限逐批生效；收割后准入与所有权语义不变。
func TestDeleteTerminalLeases(t *testing.T) {
	db, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedPrice(t, s, f.modelID)
	svc := newQuotaService(s)
	big := domain.Microcredit(1 << 40)
	now := time.Now().UTC().Truncate(time.Microsecond)

	mkReq := func() string {
		res, err := svc.Admit(ctx, admitCmd(f, priceID, 10_000, big, big, big, nil))
		if err != nil {
			t.Fatal(err)
		}
		return res.RequestID
	}
	scope := domain.LeaseScopeUpstreamAccount
	scopeID := uuid.NewString()
	acquire := func(owner string) *domain.ConcurrencyLease {
		l, err := s.AcquireLease(ctx, domain.AcquireLeaseCommand{
			Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
			OwnerToken: owner, Limit: 10, TTL: time.Hour, Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	// l1：released 且 released_at 早于窗口 → 应收割。
	l1 := acquire("owner-a")
	if err := s.ReleaseLease(ctx, l1.ID, "owner-a", l1.FencingToken); err != nil {
		t.Fatal(err)
	}
	// l2：expired（超时回收口径：released_at 为 NULL，终态时刻取 expires_at）
	// 且 expires_at 早于窗口 → 应收割。
	l2 := acquire("owner-b")
	// l3：held —— 永不在收割范围内。
	l3 := acquire("owner-c")
	// l4：released 但终态时刻在窗口内 → 保留。
	l4 := acquire("owner-d")
	if err := s.ReleaseLease(ctx, l4.ID, "owner-d", l4.FencingToken); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-7 * 24 * time.Hour)
	if _, err := db.Exec(`UPDATE inference_concurrency_leases SET released_at = $1 WHERE id = $2`,
		cutoff.Add(-time.Hour), l1.ID); err != nil {
		t.Fatal(err)
	}
	// CHECK(expires_at > acquired_at)：回拨 expires_at 必须连同 acquired_at。
	if _, err := db.Exec(`UPDATE inference_concurrency_leases
		SET state = 'expired', acquired_at = $1, expires_at = $2 WHERE id = $3`,
		cutoff.Add(-2*time.Hour), cutoff.Add(-time.Hour), l2.ID); err != nil {
		t.Fatal(err)
	}

	// 批上限：limit=1 每次至多收割一行，逐批推进。
	n, err := s.DeleteTerminalLeases(ctx, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first bounded reap = %d, want 1", n)
	}
	n, err = s.DeleteTerminalLeases(ctx, cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("second reap = %d, want 1 (remaining old terminal row)", n)
	}

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases
		WHERE scope = $1 AND scope_id = $2`, string(scope), scopeID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("remaining leases = %d, want 2 (held + recent released)", remaining)
	}

	// held 租约的所有权与使用点检查不受收割影响。
	if err := s.CheckLease(ctx, l3.ID, "owner-c", l3.FencingToken, now); err != nil {
		t.Fatalf("held lease must survive the reaper: %v", err)
	}
	// 已收割的 released 行：旧持有者的一切所有权断言天然失败（行不存在）。
	if err := s.CheckLease(ctx, l1.ID, "owner-a", l1.FencingToken, now); err == nil {
		t.Fatal("reaped lease must fail every ownership assertion")
	}

	// 收割后准入正常：fencing 取现存行最大值 + 1（l4 的 4 → 新租约 5）。
	l5, err := s.AcquireLease(ctx, domain.AcquireLeaseCommand{
		Scope: scope, ScopeID: scopeID, RequestID: mkReq(),
		OwnerToken: "owner-e", Limit: 10, TTL: time.Hour, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if l5.FencingToken != l4.FencingToken+1 {
		t.Fatalf("post-reap fencing = %d, want %d", l5.FencingToken, l4.FencingToken+1)
	}
}
