package postgres

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// correction_rate_test.go — T4 修复轮：金额修正与 token 预算（TPM）修正
// **同一个事务**（设计 §7.2/§7.3：估算被真实证据修正时，钱与 token 必须
// 一起被修正 —— 否则账本与速率预算会各说各话）。
//
// 覆盖：
//   - 估算→真实证据：账本补差 + TPM 总量一起更新，且只更新一次；
//   - 缺速率行（迁移前的老请求 / 被保留期清理掉的终态行）= 良性缺席，
//     不得回滚有效的金额修正；
//   - 存在但畸形的速率行 ≠ 良性缺席（必须响亮地失败）；
//   - 任一半边注入失败 → 两半都回滚；
//   - 重复修正幂等；原结算时刻不动；窗口外的修正不制造本分钟消费；
//   - unknown ≠ 0：没有已知 token 证据时保留原总量，绝不写 0。

// correctedFixture admits one request (with its rate reservation) and
// settles it with an ESTIMATE; it returns the request and attempt ids.
func correctedFixture(t *testing.T, s *Store, f fixture, w5, ww, wm string,
	in, out int64) (requestID, attemptID string) {
	t.Helper()
	return correctedFixtureWithCharge(t, s, f, w5, ww, wm, in, out, 60_000)
}

func correctedFixtureWithCharge(t *testing.T, s *Store, f fixture, w5, ww, wm string,
	in, out int64, charge domain.Microcredit) (requestID, attemptID string) {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := s.Reserve(ctx, uow, reserveCmd(f, w5, ww, wm, 100_000))
	if err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	attemptID = uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow),
		&domain.Attempt{ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1}); err != nil {
		t.Fatal(err)
	}
	// The admission's rate reservation (the TPM hold).
	if err := s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID:        adm.RequestID,
		BillingAccountID: f.accountID,
		APIKeyID:         &f.keyID,
		PolicyVersionID:  f.policyID,
		Limits:           domain.RateLimits{PolicyTPM: 1_000_000},
		ReservedTokens:   in + out,
	}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID,
		Usage: domain.UsageRecord{
			RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageEstimated,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		ChargeMicros: charge, AttemptID: &attemptID,
		SettledAt: time.Now().UTC(),
	}); err != nil {
		_ = uow.Rollback(ctx)
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID, attemptID
}

// correctionFixture is the correction command builder shared by the cases.
func correctionFixture(requestID, attemptID string, correctedMicros domain.Microcredit,
	correctedTokens *int64, key string) CorrectionCommand {
	in, out := int64(800), int64(200)
	return CorrectionCommand{
		RequestID: requestID,
		Corrected: domain.UsageRecord{
			RequestID: requestID, AttemptID: attemptID, Source: domain.UsageReported,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out},
		},
		CorrectedMicros: correctedMicros,
		CorrectedTokens: correctedTokens,
		Reason:          "上游账单核对",
		OperatorSubject: "user:ops@app:test",
		IdempotencyKey:  key,
	}
}

// applyCorrection runs one correction in its own transaction.
func applyCorrection(t *testing.T, s *Store, cmd CorrectionCommand) error {
	t.Helper()
	ctx := context.Background()
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CorrectSettlement(ctx, uow, cmd); err != nil {
		_ = uow.Rollback(ctx)
		return err
	}
	return uow.Commit(ctx)
}

// rateTokens reads the account-scope rate row's token total + settlement
// instant.
func rateTokens(t *testing.T, s *Store, requestID string) (tokens int64, settledAt *time.Time, state string, present bool) {
	t.Helper()
	var row struct {
		SettledTokens int64      `db:"settled_tokens"`
		SettledAt     *time.Time `db:"settled_at"`
		State         string     `db:"state"`
	}
	err := s.db.QueryRow(
		`SELECT settled_tokens, settled_at, state FROM inference_rate_reservations
		 WHERE request_id = $1 AND scope = 'account'`, requestID).
		Scan(&row.SettledTokens, &row.SettledAt, &row.State)
	if err != nil {
		return 0, nil, "", false
	}
	return row.SettledTokens, row.SettledAt, row.State, true
}

// ledgerTotals sums the request's ledger entries per type.
func ledgerTotals(t *testing.T, s *Store, requestID string) (charge, reversal, adjustment int64) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='charge'),0),
		        COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='reversal'),0),
		        COALESCE(SUM(amount_micros) FILTER (WHERE entry_type='adjustment'),0)
		   FROM inference_ledger_entries WHERE request_id = $1`, requestID).
		Scan(&charge, &reversal, &adjustment); err != nil {
		t.Fatalf("ledger totals: %v", err)
	}
	return
}

func TestCorrectSettlement_EqualMoneyUpdatesReportedTokens(t *testing.T) {
	for _, charge := range []domain.Microcredit{60_000, 0} {
		t.Run(strconv.FormatInt(int64(charge), 10), func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			requestID, attemptID := correctedFixtureWithCharge(t, s, f, w5, ww, wm, 40, 10, charge)
			_, beforeAt, _, _ := rateTokens(t, s, requestID)
			total, input, output := int64(90), int64(80), int64(10)
			cmd := correctionFixture(requestID, attemptID, charge, &total, "equal-money-tokens")
			cmd.Corrected.Buckets = domain.UsageBuckets{InputTokens: &input, OutputTokens: &output}
			if err := applyCorrection(t, s, cmd); err != nil {
				t.Fatalf("equal-money estimated-to-reported correction: %v", err)
			}
			usage, err := s.LoadUsageRecords(context.Background(), requestID)
			if err != nil {
				t.Fatal(err)
			}
			if len(usage) != 2 || usage[0].Source != domain.UsageEstimated ||
				usage[1].Source != domain.UsageReported || usage[1].Revision != 2 ||
				usage[1].Buckets.InputTokens == nil || *usage[1].Buckets.InputTokens != 80 {
				t.Fatalf("usage revisions = %+v, want preserved estimate and corrected report", usage)
			}
			tokens, afterAt, _, _ := rateTokens(t, s, requestID)
			if tokens != 90 || beforeAt == nil || afterAt == nil || !beforeAt.Equal(*afterAt) {
				t.Errorf("rate tokens/time = %d/%v, want 90/%v", tokens, afterAt, beforeAt)
			}
			req, err := s.GetRequest(context.Background(), requestID)
			if err != nil {
				t.Fatal(err)
			}
			if req.UsageStatus != domain.UsageReported || req.SettledMicros == nil || *req.SettledMicros != charge {
				t.Errorf("request usage/amount = %s/%v, want reported/%d", req.UsageStatus, req.SettledMicros, charge)
			}
			var ledgerCount, adjustmentCount int
			if err := s.db.Get(&ledgerCount, `SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id=$1`, requestID); err != nil {
				t.Fatal(err)
			}
			if err := s.db.Get(&adjustmentCount, `SELECT COUNT(*) FROM inference_adjustments WHERE request_id=$1`, requestID); err != nil {
				t.Fatal(err)
			}
			if ledgerCount != 1 || adjustmentCount != 0 {
				t.Errorf("ledger/adjustment rows = %d/%d, want 1/0 (no zero financial entries)", ledgerCount, adjustmentCount)
			}
			for _, windowID := range []string{w5, ww, wm} {
				if used, _ := windowState(t, s, windowID); used != charge {
					t.Errorf("window used = %d, want unchanged %d", used, charge)
				}
			}
			if got := keyBudgetUsed(t, s, f.keyID); got != charge {
				t.Errorf("key budget = %d, want unchanged %d", got, charge)
			}
			if _, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: 100}, 20); domain.CodeOf(err) != domain.CodeRateLimited {
				t.Errorf("corrected 90 + new 20: err = %v, want rate_limited", err)
			}
			if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeConflict {
				t.Errorf("metering-only replay = %v, want conflict", err)
			}
		})
	}
}

// TestCorrectSettlement_UpdatesTheLedgerAndTheTokenBudgetOnce: 估算→真实
// 证据的修正必须**同时**改账本与 token 预算（同一事务），且只改一次：账本
// 只有一条 charge（一次结算语义）+ 一条补差分录，速率行的 token 总量换成
// 真实值，原结算时刻不动。
func TestCorrectSettlement_UpdatesTheLedgerAndTheTokenBudgetOnce(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	before, settledAtBefore, _, present := rateTokens(t, s, requestID)
	if !present {
		t.Fatal("the settled request must carry a rate row")
	}
	if before != 1000 {
		t.Fatalf("settled tokens = %d, want 1000 (the estimate)", before)
	}
	// 真实证据：token 总量 1200（绝对值）。
	corrected := int64(1200)

	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-tpm-1")); err != nil {
		t.Fatalf("correction: %v", err)
	}

	// 账本：一条 charge + 一条补差（净额 = 40_000），绝不出现第二条 charge。
	charge, reversal, adjustment := ledgerTotals(t, s, requestID)
	if charge != 60_000 || reversal != 0 || adjustment != 20_000 {
		t.Errorf("ledger = %d/%d/%d, want 60000/0/20000 (一结算一 charge + 补差)", charge, reversal, adjustment)
	}
	// Token 预算：总量换成真实值（净差额效果），原结算时刻不动。
	after, settledAtAfter, state, _ := rateTokens(t, s, requestID)
	if after != 1200 {
		t.Errorf("settled tokens = %d, want 1200 (the token budget must be corrected with the money)", after)
	}
	if state != "settled" {
		t.Errorf("rate row state = %s, want settled", state)
	}
	if settledAtAfter == nil || settledAtBefore == nil || !settledAtAfter.Equal(*settledAtBefore) {
		t.Errorf("settled_at = %v, want %v (修正不重置窗口归属)", settledAtAfter, settledAtBefore)
	}
	// 窗口 used 跟随金额修正（既有语义不变）。
	for _, id := range []string{w5, ww, wm} {
		used, _ := windowState(t, s, id)
		if used != 40_000 {
			t.Errorf("window %s used = %d, want 40000", id, used)
		}
	}
}

// TestCorrectSettlement_MissingRateRowIsABenignAbsence: 迁移 052 之前的老
// 请求、或终态速率行已被保留期清理掉的请求 → 速率行**缺失**是良性缺席：
// 金额修正照常生效（绝不因为缺行而回滚有效的账本修正）。
func TestCorrectSettlement_MissingRateRowIsABenignAbsence(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	corrected := int64(1200)
	// 模拟保留期清理（终态行被删）/ 迁移前的老请求（从来没有速率行）。
	if _, err := s.db.Exec(
		`DELETE FROM inference_rate_reservations WHERE request_id = $1`, requestID); err != nil {
		t.Fatal(err)
	}

	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-legacy-1")); err != nil {
		t.Fatalf("a missing rate row must not roll back the money correction: %v", err)
	}
	charge, _, adjustment := ledgerTotals(t, s, requestID)
	if charge != 60_000 || adjustment != 20_000 {
		t.Errorf("ledger = %d/%d, want 60000/20000 (the money correction must apply)", charge, adjustment)
	}
	if _, _, _, present := rateTokens(t, s, requestID); present {
		t.Error("the correction must not create a rate row for a legacy request")
	}
}

// TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence: 存在
// 但畸形的速率行（已结算请求的速率行仍是在途状态；或只剩 Key 行而没有账户
// 行）**不是**良性缺席 —— 必须响亮地失败，且不得留下半截账本修正。
func TestCorrectSettlement_PresentButMalformedRateRowIsNotABenignAbsence(t *testing.T) {
	t.Run("not_settled_row", func(t *testing.T) {
		_, s := testDB(t)
		f := seedFixture(t, s, true)
		w5, ww, wm := makeWindows(t, s, f.entID)
		requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
		corrected := int64(1200)
		if _, err := s.db.Exec(
			`UPDATE inference_rate_reservations SET state='reserved' WHERE request_id=$1 AND scope='account'`,
			requestID); err != nil {
			t.Fatal(err)
		}
		err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-malformed-1"))
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("err = %v, want conflict (a malformed rate row is not a benign absence)", err)
		}
		charge, _, adjustment := ledgerTotals(t, s, requestID)
		if charge != 60_000 || adjustment != 0 {
			t.Errorf("ledger = %d/%d, want 60000/0 (the failed correction must roll back)", charge, adjustment)
		}
	})

	t.Run("key_row_without_account_row", func(t *testing.T) {
		_, s := testDB(t)
		f := seedFixture(t, s, true)
		w5, ww, wm := makeWindows(t, s, f.entID)
		requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
		corrected := int64(1200)
		if _, err := s.db.Exec(
			`DELETE FROM inference_rate_reservations WHERE request_id=$1 AND scope='account'`,
			requestID); err != nil {
			t.Fatal(err)
		}
		err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-drift-1"))
		if domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("err = %v, want conflict (a key row without its account row is drift)", err)
		}
		charge, _, adjustment := ledgerTotals(t, s, requestID)
		if charge != 60_000 || adjustment != 0 {
			t.Errorf("ledger = %d/%d, want 60000/0 (the failed correction must roll back)", charge, adjustment)
		}
	})
}

// TestCorrectSettlement_LedgerHalfFailureRollsBackTheTokenCorrection: 任一
// 半边失败 → 两半都回滚。这里注入**账本半边**的失败（窗口 used 被外部改动
// 成 0，补差会把聚合变成负数 → 守卫拒绝）：已经写入的 token 修正必须被回滚。
func TestCorrectSettlement_LedgerHalfFailureRollsBackTheTokenCorrection(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	corrected := int64(1200)
	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-first-1")); err != nil {
		t.Fatalf("first correction: %v", err)
	}
	afterFirst, _, _, _ := rateTokens(t, s, requestID)
	if afterFirst != 1200 {
		t.Fatalf("settled tokens = %d, want 1200 after the first correction", afterFirst)
	}

	// 注入账本半边的失败：窗口 used 被外部改成 0（漂移），补差会让它变负。
	if _, err := s.db.Exec(`UPDATE inference_quota_windows SET used_micros = 0 WHERE id = $1`, w5); err != nil {
		t.Fatal(err)
	}
	err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 0, &corrected, "corr-fail-1"))
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("err = %v, want conflict (the window guard must fire)", err)
	}
	// token 修正必须被回滚（与账本半边同生共死）。
	afterFail, _, _, _ := rateTokens(t, s, requestID)
	if afterFail != afterFirst {
		t.Errorf("settled tokens = %d, want %d (the failed correction must roll the token half back)", afterFail, afterFirst)
	}
	_, _, adjustment := ledgerTotals(t, s, requestID)
	if adjustment != 20_000 {
		t.Errorf("adjustment = %d, want 20000 (the failed correction must not append a second delta)", adjustment)
	}
}

// TestCorrectSettlement_RateHalfFailureRollsBackTheLedgerCorrection: 反向
// 注入**速率半边**的失败（畸形速率行）：账本修正必须被回滚（同一个事务）。
func TestCorrectSettlement_RateHalfFailureRollsBackTheLedgerCorrection(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	corrected := int64(1200)
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET state='released' WHERE request_id=$1 AND scope='account'`,
		requestID); err != nil {
		t.Fatal(err)
	}
	err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-ratefail-1"))
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("err = %v, want conflict (a malformed rate row must fail the correction)", err)
	}
	charge, _, adjustment := ledgerTotals(t, s, requestID)
	if charge != 60_000 || adjustment != 0 {
		t.Errorf("ledger = %d/%d, want 60000/0 (the rate half failure must roll the money half back)", charge, adjustment)
	}
	for _, id := range []string{w5, ww, wm} {
		used, _ := windowState(t, s, id)
		if used != 60_000 {
			t.Errorf("window %s used = %d, want 60000 (the money half must be rolled back)", id, used)
		}
	}
}

// TestCorrectSettlement_RepeatedCorrectionIsIdempotent: 同一个幂等键重复
// 投递 → 冲突（调用方视为已应用），账本与 token 预算都不再变。
func TestCorrectSettlement_RepeatedCorrectionIsIdempotent(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	corrected := int64(1200)
	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-idem-1")); err != nil {
		t.Fatalf("first correction: %v", err)
	}
	first, _, _, _ := rateTokens(t, s, requestID)

	err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-idem-1"))
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replay = %v, want conflict (already applied)", err)
	}
	after, _, _, _ := rateTokens(t, s, requestID)
	if after != first {
		t.Errorf("settled tokens = %d, want %d (a replay must not change the token budget)", after, first)
	}
	charge, _, adjustment := ledgerTotals(t, s, requestID)
	if charge != 60_000 || adjustment != 20_000 {
		t.Errorf("ledger = %d/%d, want 60000/20000 (a replay must not append a second delta)", charge, adjustment)
	}
}

// TestCorrectSettlement_CorrectionKeepsTheOriginalSettlementInstant: 修正不
// 重置 60 秒窗口归属：原结算已在窗口外时，修正只改请求事实，不制造本分钟
// 消费。
func TestCorrectSettlement_CorrectionKeepsTheOriginalSettlementInstant(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	corrected := int64(5000)
	// 把结算时刻推到窗口外（模拟很久之后才拿到真实证据）。
	if _, err := s.db.Exec(
		`UPDATE inference_rate_reservations SET settled_at = now() - interval '10 minutes'
		 WHERE request_id=$1 AND scope='account'`, requestID); err != nil {
		t.Fatal(err)
	}
	_, settledAtBefore, _, _ := rateTokens(t, s, requestID)

	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, &corrected, "corr-window-1")); err != nil {
		t.Fatalf("correction: %v", err)
	}
	after, settledAtAfter, _, _ := rateTokens(t, s, requestID)
	if after != 5000 {
		t.Errorf("settled tokens = %d, want 5000 (the request fact is corrected)", after)
	}
	if settledAtAfter == nil || settledAtBefore == nil || !settledAtAfter.Equal(*settledAtBefore) {
		t.Errorf("settled_at = %v, want %v (a correction never resets the window attribution)", settledAtAfter, settledAtBefore)
	}
	// 窗口外的修正不得制造本分钟消费。
	if got := settledTokensInWindow(t, s, f.accountID); got != 0 {
		t.Errorf("settled tokens in window = %d, want 0 (an out-of-window correction must not consume the current minute)", got)
	}
}

// TestCorrectSettlement_UnknownTokenEvidenceKeepsThePreviousTotal: 修正证据
// 里没有已知 token 总量（unknown）→ token 预算**保留原值**，绝不记零；金额
// 修正照常生效。
func TestCorrectSettlement_UnknownTokenEvidenceKeepsThePreviousTotal(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	before, _, _, _ := rateTokens(t, s, requestID)

	// 修正证据：没有任何已知的 token 桶（unknown），也没有修正后的总量。
	cmd := correctionFixture(requestID, attemptID, 40_000, nil, "corr-unknown-1")
	cmd.Corrected.Buckets = domain.UsageBuckets{}
	cmd.Corrected.Source = domain.UsageUnknown
	if err := applyCorrection(t, s, cmd); err != nil {
		t.Fatalf("correction: %v", err)
	}
	after, _, _, _ := rateTokens(t, s, requestID)
	if after != before {
		t.Errorf("settled tokens = %d, want %d (unknown never collapses into zero)", after, before)
	}
	charge, _, adjustment := ledgerTotals(t, s, requestID)
	if charge != 60_000 || adjustment != 20_000 {
		t.Errorf("ledger = %d/%d, want 60000/20000 (the money correction still applies)", charge, adjustment)
	}
}

// TestCorrectSettlement_KnownTokenEvidenceRequiresTheCorrectedTotal: 修正
// 证据带了已知 token 桶却没有给出修正后的总量 → 明确拒绝（金额修正不得
// 静默漏掉 token 预算）。
func TestCorrectSettlement_KnownTokenEvidenceRequiresTheCorrectedTotal(t *testing.T) {
	_, s := testDB(t)
	f := seedFixture(t, s, true)
	w5, ww, wm := makeWindows(t, s, f.entID)

	requestID, attemptID := correctedFixture(t, s, f, w5, ww, wm, 800, 200)
	before, _, _, _ := rateTokens(t, s, requestID)

	err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 40_000, nil, "corr-nototal-1"))
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Fatalf("err = %v, want invalid_input (the token total must not be omitted)", err)
	}
	after, _, _, _ := rateTokens(t, s, requestID)
	if after != before {
		t.Errorf("settled tokens = %d, want %d (the rejected correction must change nothing)", after, before)
	}
	charge, _, adjustment := ledgerTotals(t, s, requestID)
	if charge != 60_000 || adjustment != 0 {
		t.Errorf("ledger = %d/%d, want 60000/0 (the rejected correction must write nothing)", charge, adjustment)
	}
}
