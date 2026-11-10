package postgres

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

func TestWalletSpendLimit_IntegerOverflowRejectsAdmission(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initialHold  int64
		charge       int64
		inFlightHold int64
	}{
		{"new hold exceeds cap", math.MaxInt64 - 5, math.MaxInt64 - 5, 0},
		{"in-flight total exceeds cap after overage", math.MaxInt64 - 10, math.MaxInt64, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, s := testDB(t)
			ctx := context.Background()
			f := seedFixture(t, s, false)
			priceID := seedMoneyPrice(t, s, f.modelID, "CNY", 1, time.Now().Add(-time.Hour))
			fundWallet(t, s, f.accountID, "CNY", 0, math.MaxInt64)
			enableOverage(t, s, f.accountID, "CNY", math.MaxInt64)
			wallet, err := s.GetWalletByAccount(ctx, f.accountID, "CNY")
			if err != nil {
				t.Fatal(err)
			}
			uow, err := s.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer uow.Rollback(ctx)
			if tc.inFlightHold > 0 {
				if _, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, wallet.ID, priceID, tc.inFlightHold)); err != nil {
					t.Fatal(err)
				}
			}
			adm, err := s.ReserveWallet(ctx, uow, reserveWalletCmd(t, s, f, wallet.ID, priceID, tc.initialHold))
			if err != nil {
				t.Fatal(err)
			}
			attemptID := uuid.NewString()
			if err := insertAttempt(ctx, mustTx(t, uow), &domain.Attempt{
				ID: attemptID, RequestID: adm.RequestID, AttemptNo: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if err := uow.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			uow, err = s.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer uow.Rollback(ctx)
			settlement := walletSettleCmd(t, adm.RequestID, attemptID, tc.charge)
			zero := int64(0)
			settlement.Usage.Buckets = domain.UsageBuckets{InputTokens: &tc.charge, OutputTokens: &zero}
			if err := s.Settle(ctx, uow, settlement); err != nil {
				t.Fatal(err)
			}
			if err := uow.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			fundWallet(t, s, f.accountID, "CNY", 20, 0)
			before, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if before.MonthSpentMicros != tc.charge || before.Balance.BonusHeld != tc.inFlightHold {
				t.Fatalf("fixture spend/hold = %d/%d, want %d/%d",
					before.MonthSpentMicros, before.Balance.BonusHeld, tc.charge, tc.inFlightHold)
			}

			cmd := reserveWalletCmd(t, s, f, wallet.ID, priceID, 10)
			uow, err = s.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer uow.Rollback(ctx)
			_, admissionErr := s.ReserveWallet(ctx, uow, cmd)
			var requests, holds, reservations int
			tx := mustTx(t, uow)
			if err := tx.Get(&requests, `SELECT COUNT(*) FROM inference_requests WHERE id = $1`, cmd.Request.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Get(&holds, `SELECT COUNT(*) FROM inference_wallet_holds WHERE request_id = $1`, cmd.Request.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Get(&reservations, `SELECT COUNT(*) FROM inference_reservations WHERE request_id = $1`, cmd.Request.ID); err != nil {
				t.Fatal(err)
			}
			if requests != 0 || holds != 0 || reservations != 0 {
				t.Errorf("rejected admission left request/hold/reservations = %d/%d/%d", requests, holds, reservations)
			}
			if err := uow.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(admissionErr, accounting.ErrSpendLimitExceeded) || domain.CodeOf(admissionErr) != domain.CodeQuotaExceeded {
				t.Errorf("admission = %v, want quota_exceeded/ErrSpendLimitExceeded", admissionErr)
			}
			after, err := s.WalletBalance(ctx, f.accountID, "CNY", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if after.Balance != before.Balance || after.MonthSpentMicros != before.MonthSpentMicros {
				t.Errorf("rejected admission changed wallet: before=%+v after=%+v", before, after)
			}
			var entries int
			if err := db.Get(&entries, `SELECT COUNT(*) FROM inference_wallet_entries WHERE request_id = $1`, cmd.Request.ID); err != nil {
				t.Fatal(err)
			}
			if entries != 0 {
				t.Errorf("rejected admission wrote %d wallet entries", entries)
			}
		})
	}
}
