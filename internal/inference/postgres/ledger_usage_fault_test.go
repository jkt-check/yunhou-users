package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// ledger_usage_fault_test.go — 重复投递时的 **usage 查询故障** 必须响亮。
//
// 幂等重复成功的口径（设计 §7.2）：只有"请求已 settled 且账本上存在唯一
// 且**一致**的 charge/usage"才算重复成功。usage 查询发生真实的存储故障
// 时，账本金额一致并不能证明 usage 一致 —— 此时必须把错误向上抛，绝不能
// 当成"一致的重复成功"被网关静默吸收（那会让一次存储故障伪装成一次幂等
// 重投，账务异常被吞掉）。

// TestSettle_DuplicateDelivery_UsageLookupFailure_Propagates — 已结算请求的
// 重复投递：账本金额一致，但 usage 查询撞上真实的 PostgreSQL 故障时，
// Settle 必须返回该存储错误，而不是 SettleOutcomeDuplicate。
func TestSettle_DuplicateDelivery_UsageLookupFailure_Propagates(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	reqID, attID := settledRequestWithCharge(t, s, f, w5, ww, wm, 100_000, 60_000)

	// 可控故障：在**本会话**（pg_temp）里用一张形状不兼容的临时表遮蔽
	// inference_usage_records，让重复投递的 usage 查询撞上真实的
	// PostgreSQL 错误（列不存在 = 42703），而不是"查不到行"。临时表只
	// 存在于本会话，不干扰其它测试。
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx := mustTx(t, uow)
	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE inference_usage_records (attempt_id text)`); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatalf("create temp table: %v", err)
	}

	in, out := int64(10), int64(5)
	err = s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: reqID,
		Usage: domain.UsageRecord{
			RequestID: reqID, AttemptID: attID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: 60_000, SettledAt: time.Now().UTC(),
	})
	_ = uow.Rollback(ctx)

	if err == nil {
		t.Fatal("settle: want the usage lookup failure to surface, got nil")
	}
	// 存储故障绝不能被分类成"一致的重复成功"（幂等重投）。
	if se, ok := domain.AsSettleError(err); ok && se.Kind == domain.SettleOutcomeDuplicate {
		t.Fatalf("settle: usage lookup failure was classified as %q — a storage failure must never be a duplicate", se.Kind)
	}
	// 必须是真实的 PostgreSQL 故障（列不存在），不是别的失败。
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("settle: err = %v, want the underlying postgres error to be propagated", err)
	}
	if pqErr.Code != "42703" {
		t.Fatalf("postgres error code = %q, want 42703 (undefined_column)", pqErr.Code)
	}
}
