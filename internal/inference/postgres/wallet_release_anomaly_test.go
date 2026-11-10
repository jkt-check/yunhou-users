package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestReleaseWallet_FreezeAnomaly(t *testing.T) {
	for _, fromReconciliation := range []bool{false, true} {
		entry := "reserved"
		if fromReconciliation {
			entry = "reconciliation"
		}
		for _, fault := range []struct {
			name string
			sql  string
			kind domain.SettleOutcomeKind
		}{
			{"missing", `DELETE FROM inference_wallet_holds WHERE request_id = $1`, domain.SettleOutcomeMissingReservation},
			{"released", `UPDATE inference_wallet_holds SET state = 'released', released_at = now() WHERE request_id = $1`, domain.SettleOutcomeReservationDrift},
			{"settled", `UPDATE inference_wallet_holds SET state = 'settled', settled_at = now() WHERE request_id = $1`, domain.SettleOutcomeReservationDrift},
		} {
			t.Run(entry+"/"+fault.name, func(t *testing.T) {
				_, s := testDB(t)
				ctx := context.Background()
				requestID, _, _ := walletAdmission(t, s)
				wantStatus := string(domain.ReqReserved)
				release := s.Release
				if fromReconciliation {
					uow, err := s.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer uow.Rollback(ctx)
					if err := s.MarkReconciliationRequired(ctx, uow, requestID, ReasonUnknownUsage, time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					if err := uow.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					wantStatus = string(domain.ReqReconciliationRequired)
					release = s.ReleaseFromReconciliation
				}
				if _, err := s.db.Exec(fault.sql, requestID); err != nil {
					t.Fatal(err)
				}

				uow, err := s.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer uow.Rollback(ctx)
				err = release(ctx, uow, requestID)
				se, ok := domain.AsSettleError(err)
				if !ok {
					t.Errorf("release: got %v, want typed %s", err, fault.kind)
				} else if se.Kind != fault.kind {
					t.Errorf("outcome = %s, want %s", se.Kind, fault.kind)
				}

				tx := mustTx(t, uow)
				assertNoPartialSettlement(t, tx, requestID)
				if got := txRequestStatus(t, tx, requestID); got != wantStatus {
					t.Errorf("request status = %s, want %s", got, wantStatus)
				}
				if got := txHeldReservations(t, tx, requestID); got != 1 {
					t.Errorf("held reservations = %d, want 1", got)
				}
				if fault.name == "missing" {
					if n := txCount(t, tx, `SELECT COUNT(*) FROM inference_wallet_holds WHERE request_id = $1`, requestID); n != 0 {
						t.Errorf("wallet holds = %d, want 0", n)
					}
				} else {
					var state string
					if err := tx.QueryRowxContext(ctx, `SELECT state FROM inference_wallet_holds WHERE request_id = $1`, requestID).Scan(&state); err != nil {
						t.Fatal(err)
					}
					if state != fault.name {
						t.Errorf("wallet freeze state = %s, want unchanged %s", state, fault.name)
					}
				}
				if fromReconciliation {
					if n := txCount(t, tx,
						`SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id = $1 AND status = 'pending'`,
						requestID); n != 1 {
						t.Errorf("pending jobs = %d, want 1", n)
					}
				}
			})
		}
	}
}
