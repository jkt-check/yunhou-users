package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

func walletCorrectionFixture(t *testing.T, s *Store, charge int64) (fixture, string, string, string) {
	t.Helper()
	ctx := context.Background()
	f := seedFixture(t, s, true)
	priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
	fundWallet(t, s, f.accountID, "CNY", 1000, 40)
	enableOverage(t, s, f.accountID, "CNY", 2000)
	wallet, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
	if err != nil {
		t.Fatal(err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(ctx)
	cmd := reserveWalletCmd(t, s, f, wallet.ID, priceID, 100)
	cmd.Request.APIKeyID = &f.keyID
	adm, err := s.ReserveWallet(ctx, uow, cmd)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.NewString()
	if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{
		ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRateTx(ctx, uow, domain.RateReserveCommand{
		RequestID: adm.RequestID, BillingAccountID: f.accountID,
		APIKeyID: &f.keyID, PolicyVersionID: f.policyID,
		Limits: domain.RateLimits{PolicyTPM: 1000}, ReservedTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}
	in, out := int64(40), int64(10)
	if err := s.Settle(ctx, uow, domain.SettleCommand{
		RequestID: adm.RequestID, ChargeMicros: domain.Microcredit(charge), SettledAt: time.Now().UTC(),
		Usage: domain.UsageRecord{RequestID: adm.RequestID, AttemptID: attemptID, Source: domain.UsageEstimated,
			Buckets: domain.UsageBuckets{InputTokens: &in, OutputTokens: &out}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return f, wallet.ID, adm.RequestID, attemptID
}

func walletCorrectionSnapshot(t *testing.T, s *Store, requestID string) string {
	t.Helper()
	var snapshot string
	if err := s.db.Get(&snapshot, `SELECT jsonb_build_object(
		'charge', (SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM inference_ledger_entries e
		    WHERE e.request_id=$1 AND e.entry_type='charge'),
		'consumes', (SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM inference_wallet_entries e
		    WHERE e.request_id=$1 AND e.entry_type='consume'),
		'hold', (SELECT to_jsonb(h) FROM inference_wallet_holds h WHERE h.request_id=$1)
	)::text`, requestID); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCorrectSettlement_WalletSourceDeltas(t *testing.T) {
	_, s := testDB(t)
	f, _, requestID, attemptID := walletCorrectionFixture(t, s, 80)
	original := walletCorrectionSnapshot(t, s, requestID)
	_, originalAt, _, _ := rateTokens(t, s, requestID)
	for i, tc := range []struct{ amount, cash, bonus, entries int64 }{
		{30, 1000, 10, 2}, {120, 920, 0, 4}, {0, 1000, 40, 6},
		{70, 970, 0, 8}, {0, 1000, 40, 10},
	} {
		total := int64(90 + i)
		cmd := correctionFixture(requestID, attemptID, domain.Microcredit(tc.amount), &total, fmt.Sprintf("wallet-corr-%d", i))
		if err := applyCorrection(t, s, cmd); err != nil {
			t.Fatalf("correct to %d: %v", tc.amount, err)
		}
		view, err := s.WalletBalance(context.Background(), f.accountID, "CNY", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if view.Balance.CashAvailable != tc.cash || view.Balance.BonusAvailable != tc.bonus ||
			view.MonthSpentMicros != tc.amount {
			t.Errorf("correction %d balance/spend = %+v/%d; want cash %d bonus %d spend %d",
				i, view.Balance, view.MonthSpentMicros, tc.cash, tc.bonus, tc.amount)
		}
		var entries, wrongUnit, unpaired int64
		if err := s.db.QueryRow(`SELECT count(*),
		    count(*) FILTER (WHERE a.unit <> 'micromoney' OR a.currency <> 'CNY'),
		    count(*) FILTER (WHERE e.id IS NULL OR l.id IS NULL OR e.source <> a.source
		        OR e.direction <> a.direction OR e.amount_micros <> a.amount_micros
		        OR l.amount_micros <> a.amount_micros)
		    FROM inference_adjustments a
		    LEFT JOIN inference_wallet_entries e ON e.adjustment_id=a.id
		    LEFT JOIN inference_ledger_entries l ON l.adjustment_id=a.id
		    WHERE a.request_id=$1`, requestID).Scan(&entries, &wrongUnit, &unpaired); err != nil {
			t.Fatal(err)
		}
		if entries != tc.entries || wrongUnit != 0 || unpaired != 0 {
			t.Errorf("source adjustments = %d, bad units/pairs=%d/%d", entries, wrongUnit, unpaired)
		}
		if walletCorrectionSnapshot(t, s, requestID) != original {
			t.Error("correction rewrote original charge, consume or frozen composition")
		}
		if keyBudgetUsed(t, s, f.keyID) != 0 {
			t.Error("wallet correction changed unreserved Key money budget")
		}
		tokens, at, _, _ := rateTokens(t, s, requestID)
		if tokens != total || at == nil || originalAt == nil || !at.Equal(*originalAt) {
			t.Errorf("TPM = %d/%v, want %d/%v", tokens, at, total, originalAt)
		}
		if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("replay = %v, want conflict", err)
		}
	}
	usage, err := s.LoadUsageRecords(context.Background(), requestID)
	if err != nil || len(usage) != 6 || usage[0].Source != domain.UsageEstimated || usage[5].Source != domain.UsageReported {
		t.Fatalf("usage revisions = %+v, %v", usage, err)
	}
}

func TestCorrectSettlement_WalletMeteringOnly(t *testing.T) {
	_, s := testDB(t)
	_, _, requestID, attemptID := walletCorrectionFixture(t, s, 80)
	before := walletCorrectionSnapshot(t, s, requestID)
	total := int64(90)
	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 80, &total, "wallet-metering")); err != nil {
		t.Fatal(err)
	}
	if walletCorrectionSnapshot(t, s, requestID) != before {
		t.Error("metering-only correction rewrote finance")
	}
	var count int
	if err := s.db.Get(&count, `SELECT count(*) FROM inference_adjustments WHERE request_id=$1`, requestID); err != nil {
		t.Fatal(err)
	}
	tokens, _, _, _ := rateTokens(t, s, requestID)
	if count != 0 || tokens != 90 {
		t.Errorf("financial adjustments/TPM = %d/%d, want 0/90", count, tokens)
	}
}

func TestCorrectSettlement_WalletMonthAttribution(t *testing.T) {
	_, s := testDB(t)
	f, walletID, requestID, attemptID := walletCorrectionFixture(t, s, 80)
	prior := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	current := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if _, err := s.db.Exec(`UPDATE inference_wallet_holds SET settled_at=$2 WHERE request_id=$1`, requestID, prior); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE inference_wallet_entries SET created_at=$2
	    WHERE request_id=$1 AND entry_type='consume'`, requestID, prior); err != nil {
		t.Fatal(err)
	}
	total := int64(90)
	if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 30, &total, "wallet-late-evidence")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at   time.Time
		want int64
	}{{prior, 30}, {current, 0}} {
		view, err := s.WalletBalance(context.Background(), f.accountID, "CNY", tc.at)
		if err != nil {
			t.Fatal(err)
		}
		uow, err := s.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := monthSpendLocked(context.Background(), mustTx(t, uow), walletID, view.MonthStart, view.MonthEnd)
		_ = uow.Rollback(context.Background())
		if err != nil || got != tc.want || view.MonthSpentMicros != tc.want {
			t.Errorf("month %s locked/read=%d/%d err=%v, want %d",
				tc.at, got, view.MonthSpentMicros, err, tc.want)
		}
	}
}

func TestCorrectSettlement_WalletDriftAndRollback(t *testing.T) {
	for _, fault := range []string{"missing_hold", "reversed_consume", "wrong_currency", "wallet_write"} {
		t.Run(fault, func(t *testing.T) {
			db, s := testDB(t)
			f, _, requestID, attemptID := walletCorrectionFixture(t, s, 80)
			switch fault {
			case "missing_hold":
				if _, err := db.Exec(`UPDATE inference_wallet_entries SET hold_id=NULL WHERE request_id=$1`, requestID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DELETE FROM inference_wallet_holds WHERE request_id=$1`, requestID); err != nil {
					t.Fatal(err)
				}
			case "reversed_consume":
				var entryID int64
				if err := db.Get(&entryID, `SELECT id FROM inference_wallet_entries
				    WHERE request_id=$1 AND source='cash'`, requestID); err != nil {
					t.Fatal(err)
				}
				uow, err := s.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if err := s.ReverseWalletEntryTx(context.Background(), uow, f.accountID, entryID, "test"); err != nil {
					t.Fatal(err)
				}
				if err := uow.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "wrong_currency":
				if _, err := db.Exec(`UPDATE inference_ledger_entries SET currency='USD'
				    WHERE request_id=$1 AND entry_type='charge'`, requestID); err != nil {
					t.Fatal(err)
				}
			case "wallet_write":
				if _, err := db.Exec(`CREATE FUNCTION test_wallet_correction_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				    BEGIN RAISE EXCEPTION 'injected wallet correction failure'; END $$`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = db.Exec(`DROP TRIGGER IF EXISTS test_wallet_correction_failure ON inference_wallet_entries`)
					_, _ = db.Exec(`DROP FUNCTION IF EXISTS test_wallet_correction_failure()`)
				})
				if _, err := db.Exec(`CREATE TRIGGER test_wallet_correction_failure BEFORE INSERT ON inference_wallet_entries
				    FOR EACH ROW WHEN (NEW.entry_type='adjustment') EXECUTE FUNCTION test_wallet_correction_failure()`); err != nil {
					t.Fatal(err)
				}
			}
			total := int64(90)
			cmd := correctionFixture(requestID, attemptID, 30, &total, "wallet-fault-"+fault)
			err := applyCorrection(t, s, cmd)
			want := domain.CodeConflict
			if fault == "wallet_write" {
				want = domain.CodeInternal
			}
			if domain.CodeOf(err) != want {
				t.Fatalf("fault %s = %v, want %s", fault, err, want)
			}
			assertCorrectionReceiptCount(t, s, cmd.IdempotencyKey, 0)
			tokens, _, _, _ := rateTokens(t, s, requestID)
			usage, err := s.LoadUsageRecords(context.Background(), requestID)
			req, rerr := s.GetRequest(context.Background(), requestID)
			if err != nil || rerr != nil || len(usage) != 1 || tokens != 50 ||
				req.SettledMicros == nil || *req.SettledMicros != 80 {
				t.Fatalf("failed correction changed usage/TPM/request: %+v/%d/%+v, %v/%v", usage, tokens, req, err, rerr)
			}
			if fault == "wallet_write" {
				if _, err := db.Exec(`DROP TRIGGER test_wallet_correction_failure ON inference_wallet_entries`); err != nil {
					t.Fatal(err)
				}
				if err := applyCorrection(t, s, cmd); err != nil {
					t.Fatalf("same key after fault repair: %v", err)
				}
			}
		})
	}
}

func TestCorrectSettlement_WalletAccountLockFirst(t *testing.T) {
	_, s := testDB(t)
	f, _, requestID, attemptID := walletCorrectionFixture(t, s, 80)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if err := lockAccountTx(ctx, mustTx(t, blocker), f.accountID); err != nil {
		t.Fatal(err)
	}
	uow, err := s.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(context.Background())
	var pid int
	if err := mustTx(t, uow).GetContext(ctx, &pid, `SELECT pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	finished, result := make(chan struct{}), make(chan error, 1)
	total := int64(90)
	go func() {
		defer close(finished)
		result <- s.CorrectSettlement(ctx, uow, correctionFixture(requestID, attemptID, 30, &total, "wallet-locked"))
	}()
	if err := waitQuotaRevisionLock(ctx, s, pid, finished); err != nil {
		t.Fatal(err)
	}
	// While the account is locked, the correction must not own the request.
	if _, err := mustTx(t, blocker).ExecContext(ctx,
		`SELECT id FROM inference_requests WHERE id=$1 FOR UPDATE NOWAIT`, requestID); err != nil {
		t.Fatal("correction locked request before account:", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectSettlement_WalletCorrectionLedgerDrift(t *testing.T) {
	for _, tc := range []struct {
		name, mutate, repair string
	}{
		{"generic_amount", `UPDATE inference_ledger_entries SET amount_micros=39 WHERE adjustment_id=$1`,
			`UPDATE inference_ledger_entries SET amount_micros=40 WHERE adjustment_id=$1`},
		{"generic_currency", `UPDATE inference_ledger_entries SET currency='USD' WHERE adjustment_id=$1`,
			`UPDATE inference_ledger_entries SET currency='CNY' WHERE adjustment_id=$1`},
		{"generic_price", `UPDATE inference_ledger_entries SET price_version_id=NULL WHERE adjustment_id=$1`,
			`UPDATE inference_ledger_entries SET price_version_id=(SELECT price_version_id FROM inference_requests WHERE id=$2) WHERE adjustment_id=$1`},
		{"generic_request", `UPDATE inference_ledger_entries SET request_id=NULL WHERE adjustment_id=$1`,
			`UPDATE inference_ledger_entries SET request_id=$2 WHERE adjustment_id=$1`},
		{"generic_missing", `DELETE FROM inference_ledger_entries WHERE adjustment_id=$1`,
			`INSERT INTO inference_ledger_entries
			    (billing_account_id,request_id,entry_type,amount_micros,unit,currency,
			     price_version_id,adjustment_id,created_by)
			    SELECT a.billing_account_id,a.request_id,'adjustment',a.amount_micros,a.unit,a.currency,
			        r.price_version_id,a.id,a.operator_subject
			    FROM inference_adjustments a JOIN inference_requests r ON r.id=a.request_id
			    WHERE a.id=$1`},
		{"wallet_pair_missing", `UPDATE inference_wallet_entries SET adjustment_id=NULL WHERE adjustment_id=$1`,
			`UPDATE inference_wallet_entries SET adjustment_id=$1 WHERE request_id=$2 AND entry_type='adjustment' AND source='cash'`},
		{"adjustment_direction", `UPDATE inference_adjustments SET direction='debit' WHERE id=$1`,
			`UPDATE inference_adjustments SET direction='credit' WHERE id=$1`},
		{"adjustment_source", `UPDATE inference_adjustments SET source='bonus' WHERE id=$1`,
			`UPDATE inference_adjustments SET source='cash' WHERE id=$1`},
		{"adjustment_request", `UPDATE inference_adjustments SET request_id=NULL WHERE id=$1`,
			`UPDATE inference_adjustments SET request_id=$2 WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, s := testDB(t)
			_, _, requestID, attemptID := walletCorrectionFixture(t, s, 80)
			total := int64(90)
			if err := applyCorrection(t, s, correctionFixture(requestID, attemptID, 30, &total, "prior-correction")); err != nil {
				t.Fatal(err)
			}
			var adjID string
			if err := db.Get(&adjID, `SELECT id FROM inference_adjustments
			    WHERE request_id=$1 AND source='cash' AND direction='credit' AND amount_micros=40`, requestID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(tc.mutate, adjID); err != nil {
				t.Fatal(err)
			}
			snapshot := func() string {
				t.Helper()
				uow, err := s.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer uow.Rollback(context.Background())
				return correctionAccountingSnapshot(t, mustTx(t, uow), requestID)
			}
			before := snapshot()
			correctedTokens := int64(91)
			metering := correctionFixture(requestID, attemptID, 30, &correctedTokens, "metering-after-drift")
			if err := applyCorrection(t, s, metering); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("metering correction with historical %s drift: %v, want conflict", tc.name, err)
			}
			if after := snapshot(); after != before {
				t.Fatal("rejected metering correction changed accounting")
			}
			cmd := correctionFixture(requestID, attemptID, 120, &total, "after-drift")
			if err := applyCorrection(t, s, cmd); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("historical %s drift: %v, want conflict", tc.name, err)
			}
			if after := snapshot(); after != before {
				t.Fatal("rejected correction changed accounting")
			}
			args := []any{adjID}
			if tc.name == "generic_price" || tc.name == "generic_request" ||
				tc.name == "wallet_pair_missing" || tc.name == "adjustment_request" {
				args = append(args, requestID)
			}
			if _, err := db.Exec(tc.repair, args...); err != nil {
				t.Fatal(err)
			}
			if err := applyCorrection(t, s, metering); err != nil {
				t.Fatalf("same metering key after repairing history: %v", err)
			}
			if err := applyCorrection(t, s, cmd); err != nil {
				t.Fatalf("same key after repairing history: %v", err)
			}
		})
	}
}
