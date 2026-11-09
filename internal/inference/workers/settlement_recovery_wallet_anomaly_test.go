package workers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

func (f *workerFixture) reserveWallet(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	price := &postgres.PriceVersion{
		ModelID: f.modelID, Kind: postgres.PriceSaleMoney, Unit: "micromoney", Currency: "CNY",
		InputPerMtok: 1_000_000, OutputPerMtok: 1_000_000,
		Revision: 1, EffectiveFrom: now.Add(-time.Hour),
	}
	if err := f.store.InsertPriceVersion(ctx, price); err != nil {
		t.Fatal(err)
	}
	uow, err := f.store.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(ctx)
	if err := f.store.CreditTopupTx(ctx, uow, postgres.WalletTopupCommand{
		AccountID: f.accountID, Currency: "CNY", AmountMicros: 10_000_000,
		PaymentID: "pay-" + uuid.NewString(), OrderID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	limit := int64(10_000_000)
	wallet, err := f.store.SetOverageTx(ctx, uow, f.accountID, "CNY", true, &limit, "user:test")
	if err != nil {
		t.Fatal(err)
	}
	start, end := accounting.MonthBoundsUTC(now)
	adm, err := f.store.ReserveWallet(ctx, uow, domain.ReserveWalletCommand{
		Request: domain.Request{
			ID: uuid.NewString(), BillingAccountID: f.accountID, APIKeyID: &f.keyID,
			EntitlementID: f.entID, ModelID: f.modelID,
			Protocol: domain.ProtocolOpenAIChat, PolicyVersionID: f.policyID,
		},
		WalletID: wallet.ID, HoldMicros: 50_000, PriceVersionID: price.ID,
		AdmittedAt: now, MonthStart: start, MonthEnd: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return adm.RequestID
}

func TestRecover_WalletFreezeAnomaly_ParksAndDisposesRate(t *testing.T) {
	for _, fault := range []struct {
		name   string
		sql    string
		reason string
	}{
		{"missing", `DELETE FROM inference_wallet_holds WHERE request_id = $1`, postgres.ReasonMissingReservation},
		{"released", `UPDATE inference_wallet_holds SET state = 'released', released_at = now() WHERE request_id = $1`, postgres.ReasonReservationDrift},
		{"settled", `UPDATE inference_wallet_holds SET state = 'settled', settled_at = now() WHERE request_id = $1`, postgres.ReasonReservationDrift},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newWorkerFixture(t)
			ctx := context.Background()
			reqID := f.reserveWallet(t)
			f.fabricateRateReservation(t, reqID, 60)
			if _, err := f.db.Exec(fault.sql, reqID); err != nil {
				t.Fatal(err)
			}

			stats, err := f.newWorker().RunPass(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Errors == 0 || stats.RacingSkipped != 0 || stats.ReleasedNotSent != 0 {
				t.Errorf("stats = %+v, want an accounting alarm, not a race or successful release", stats)
			}
			status, _, _, settled := f.requestState(t, reqID)
			if status != "reconciliation_required" || settled != 0 {
				t.Errorf("request = %s/%d, want reconciliation_required/0", status, settled)
			}
			reason, jobStatus, found := f.openJob(t, reqID)
			if !found || reason != fault.reason || jobStatus != "pending" {
				t.Errorf("job = %t/%s/%s, want true/%s/pending", found, reason, jobStatus, fault.reason)
			}
			for _, state := range f.rateReservationStates(t, reqID) {
				if state != "anomaly" {
					t.Errorf("rate state = %s, want anomaly", state)
				}
			}
			if got := f.accountInflightTokens(t); got != 0 {
				t.Errorf("in-flight tokens = %d, want 0", got)
			}
			var held, charges, consumes, rateRows int
			for _, check := range []struct {
				query string
				dest  *int
				want  int
			}{
				{`SELECT COUNT(*) FROM inference_reservations WHERE request_id = $1 AND state = 'held'`, &held, 1},
				{`SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id = $1`, &charges, 0},
				{`SELECT COUNT(*) FROM inference_wallet_entries WHERE request_id = $1`, &consumes, 0},
				{`SELECT COUNT(*) FROM inference_rate_reservations WHERE request_id = $1`, &rateRows, 2},
			} {
				if err := f.db.Get(check.dest, check.query, reqID); err != nil {
					t.Fatal(err)
				}
				if *check.dest != check.want {
					t.Errorf("%s: got %d, want %d", check.query, *check.dest, check.want)
				}
			}

			stats2, err := f.newWorker().RunPass(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stats2.Errors != 0 || stats2.RacingSkipped != 0 || stats2.ReleasedNotSent != 0 ||
				stats2.SettledConservative != 0 || stats2.SettledVerified != 0 {
				t.Errorf("second pass = %+v, want no repeated processing", stats2)
			}
		})
	}
}
