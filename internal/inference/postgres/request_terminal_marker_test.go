package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestUpdateRequestStatus_PreservesTerminalAccounting(t *testing.T) {
	for _, source := range []string{"plan", "wallet"} {
		t.Run(source, func(t *testing.T) {
			for _, charge := range []int64{0, 60_000} {
				price := "positive"
				if charge == 0 {
					price = "zero"
				}
				t.Run(price, func(t *testing.T) {
					for _, terminal := range []domain.RequestStatus{domain.ReqSettled, domain.ReqReleased} {
						t.Run(string(terminal), func(t *testing.T) {
							for _, marker := range []domain.RequestStatus{domain.ReqDispatching, domain.ReqStreaming, domain.ReqNonStreaming} {
								t.Run(string(marker), func(t *testing.T) {
									_, s := testDB(t)
									ctx := context.Background()
									var requestID, attemptID string
									if source == "wallet" {
										requestID, attemptID, _ = walletAdmission(t, s)
									} else {
										f := seedFixture(t, s, true)
										w5, ww, wm := makeWindows(t, s, f.entID)
										var err error
										requestID, err = admitWithRate(t, s, f, w5, ww, wm,
											domain.RateLimits{PolicyTPM: 100_000}, 1000)
										if err != nil {
											t.Fatal(err)
										}
										attemptID = uuid.NewString()
										if err := s.InsertAttempt(ctx, &domain.Attempt{
											ID: attemptID, RequestID: requestID, AttemptNo: 1,
										}); err != nil {
											t.Fatal(err)
										}
									}
									uow, err := s.Begin(ctx)
									if err != nil {
										t.Fatal(err)
									}
									defer uow.Rollback(ctx)
									if terminal == domain.ReqReleased {
										err = s.Release(ctx, uow, requestID)
									} else {
										cmd := walletSettleCmd(t, requestID, attemptID, charge)
										if source == "plan" {
											cmd.WalletCharge = nil
										}
										err = s.Settle(ctx, uow, cmd)
									}
									if err != nil {
										t.Fatal(err)
									}
									if err := uow.Commit(ctx); err != nil {
										t.Fatal(err)
									}
									observed, err := s.GetRequest(ctx, requestID)
									if err != nil || observed.Status != terminal {
										t.Fatalf("terminal setup = %+v, %v", observed, err)
									}
									snapshot, err := s.Begin(ctx)
									if err != nil {
										t.Fatal(err)
									}
									defer snapshot.Rollback(ctx)
									tx := mustTx(t, snapshot)
									before := settledAccountingSnapshot(t, tx, requestID)
									if err := s.UpdateRequestStatus(ctx, requestID, marker, "late dispatch marker"); err != nil {
										t.Fatal(err)
									}
									if after := settledAccountingSnapshot(t, tx, requestID); after != before {
										t.Errorf("%s marker rewrote %s accounting or terminal facts", marker, terminal)
									}
								})
							}
						})
					}
				})
			}
		})
	}
}

func TestUpdateRequestStatus_NonterminalAndMissing(t *testing.T) {
	_, s := testDB(t)
	ctx := context.Background()
	requestID, _, _ := walletAdmission(t, s)
	for _, marker := range []domain.RequestStatus{domain.ReqDispatching, domain.ReqStreaming, domain.ReqNonStreaming} {
		t.Run(string(marker), func(t *testing.T) {
			if err := s.UpdateRequestStatus(ctx, requestID, marker, "dispatch detail"); err != nil {
				t.Fatal(err)
			}
			request, err := s.GetRequest(ctx, requestID)
			if err != nil || request.Status != marker || request.LastError != "dispatch detail" {
				t.Fatalf("nonterminal marker = %+v, %v", request, err)
			}
			if err := s.UpdateRequestStatus(ctx, uuid.NewString(), marker, ""); domain.CodeOf(err) != domain.CodeNotFound {
				t.Errorf("missing request = %v, want not_found", err)
			}
		})
	}
}
