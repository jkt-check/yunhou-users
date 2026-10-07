package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	paddleerr "github.com/PaddleHQ/paddle-go-sdk/v5/pkg/paddleerr"
	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// ============================================================================
// Review fixes (a02ad45..2c917a5 "With fixes"): change-plan post-paddle error
// mapping, cancel race discipline, already-canceled heal.
// ============================================================================

// subFillingTx is a fake dbTx whose lock read fills a *model.Subscription
// and whose ExecContext reports configurable RowsAffected / errors per call.
type subFillingTx struct {
	fakeTx
	sub           *model.Subscription
	rowsAffected  int64
	execCallCount int
	execErrAt     map[int]error
}

type subResult struct{ n int64 }

func (subResult) LastInsertId() (int64, error)   { return 0, nil }
func (r subResult) RowsAffected() (int64, error) { return r.n, nil }

func (f *subFillingTx) GetContext(_ context.Context, dest interface{}, _ string, _ ...interface{}) error {
	if f.sub != nil {
		if s, ok := dest.(*model.Subscription); ok {
			*s = *f.sub
		}
	}
	return nil
}
func (f *subFillingTx) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	f.execCallCount++
	if err, ok := f.execErrAt[f.execCallCount]; ok {
		return nil, err
	}
	return subResult{n: f.rowsAffected}, nil
}

// Important 1: the post-Paddle RowsAffected==0 branch (the sub flipped
// non-active between the lock and the plan write) charged the user
// channel-side — surfacing 404 "subscription not found" is wrong. It must
// be the channel-unavailable class (502 bucket) with the divergence audit.
func TestChangePlanByID_PostPaddleZeroRows_ChannelUnavailable(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_zr_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	next := time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second)
	svc.SetPaddleClient(&stubPaddle{updateNext: &next})
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &subFillingTx{
			sub: &model.Subscription{
				ID: subID, UserID: uid, PlanID: "monthly", Status: "active",
			},
			rowsAffected: 0, // the plan write matches no row
		}, nil
	}

	_, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly")
	if !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("expected ErrChannelUnavailable (502 bucket), got %v", err)
	}
	if errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatal("must NOT surface ErrSubscriptionNotFound after a channel-side charge")
	}
	if countAudit(t, db, "paddle_change_plan_local_sync_failed") != 1 {
		t.Error("expected divergence audit paddle_change_plan_local_sync_failed")
	}
}

// Important 3: two concurrent cancels of the same subscription must yield a
// SINGLE Paddle call; the loser gets the idempotent 200 path.
type lockedStubPaddle struct {
	mu sync.Mutex
	stubPaddle
}

func (s *lockedStubPaddle) CancelSubscription(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stubPaddle.CancelSubscription(ctx, id)
}

func TestCancelSubscriptionByID_ConcurrentDoubleCancel_SinglePaddleCall(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_race_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	stub := &lockedStubPaddle{}
	svc.SetPaddleClient(stub)

	const callers = 2
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.CancelSubscriptionByID(context.Background(), uid, subID, "next_billing_period")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v (both must succeed — loser is idempotent)", i, err)
		}
	}
	if got := stub.cancelCalls; got != 1 {
		t.Errorf("paddle cancel calls = %d, want exactly 1", got)
	}
	if _, _, autoRenew, _ := readChannelState(t, db, subID); autoRenew {
		t.Error("auto_renew still true after concurrent cancels")
	}
}

// Important 4: Paddle saying "already canceled" / "not found" means the
// channel-side billing relationship is already over — heal locally (flip
// auto_renew=false + loud audit) instead of wedging the user on 502.
func TestCancelSubscriptionByID_PaddleAlreadyCanceled_Heals(t *testing.T) {
	goneErrs := map[string]error{
		"subscription_is_canceled_action_invalid": &paddleerr.Error{
			Type: paddleerr.ErrorTypeRequestError, Code: "subscription_is_canceled_action_invalid",
		},
		"not_found": &paddleerr.Error{
			Type: paddleerr.ErrorTypeRequestError, Code: "not_found",
		},
	}
	for name, goneErr := range goneErrs {
		t.Run(name, func(t *testing.T) {
			db := setupPaymentDB(t)
			svc := newTestPaymentService(t, db)
			uid := seedUser(t, db)
			subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_gone_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

			svc.SetPaddleClient(&stubPaddle{cancelErr: goneErr})
			sub, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
			if err != nil {
				t.Fatalf("already-canceled class must heal, got %v", err)
			}
			if sub.AutoRenew {
				t.Error("returned sub AutoRenew = true, want false after heal")
			}
			if _, _, autoRenew, _ := readChannelState(t, db, subID); autoRenew {
				t.Error("auto_renew still true after heal")
			}
			if countAudit(t, db, "paddle_cancel_already_canceled_healed") != 1 {
				t.Error("expected heal audit paddle_cancel_already_canceled_healed")
			}
		})
	}
}

// Minor 8: a scheduled-cancel-pending sub (auto_renew=false) must NOT be
// plan-changed — Paddle drops scheduled_change on items update, silently
// resurrecting billing while local reads auto_renew=false. Reject with the
// no-auto-renew class (cancel stays idempotent-200 on the same state).
func TestChangePlanByID_ScheduledCancelPending_Rejected(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_pend_"+mustNewUUID()[:8], "paddle", false, time.Now().Add(15*24*time.Hour))

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.SetPaddlePrices(map[string]string{"yearly": "pri_y"})
	if _, err := svc.ChangePlanByID(context.Background(), uid, subID, "yearly"); !errors.Is(err, ErrSubscriptionNoAutoRenew) {
		t.Fatalf("expected ErrSubscriptionNoAutoRenew, got %v", err)
	}
	if stub.updateCalls != 0 {
		t.Fatal("paddle must not be called for a scheduled-cancel-pending sub")
	}

	// The SAME row state stays idempotent-200 for cancel.
	sub, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
	if err != nil {
		t.Fatalf("cancel on scheduled-cancel-pending sub must stay idempotent-200, got %v", err)
	}
	if sub == nil || stub.cancelCalls != 0 {
		t.Errorf("cancel must not call paddle: sub=%v calls=%d", sub, stub.cancelCalls)
	}
}

// Minor 10: post-Paddle auto_renew flip failure logs + writes a divergence
// audit and still returns success (retry heals — see Important 4), matching
// the legacy CancelChannelSubscription philosophy.
func TestCancelSubscriptionByID_FlipFailure_LogAndContinue(t *testing.T) {
	db := setupPaymentDB(t)
	svc := newTestPaymentService(t, db)
	uid := seedUser(t, db)
	subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_ff_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))

	stub := &stubPaddle{}
	svc.SetPaddleClient(stub)
	svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
		return &subFillingTx{
			sub: &model.Subscription{
				ID: subID, UserID: uid, PlanID: "monthly", Status: "active", AutoRenew: true,
			},
			rowsAffected: 1,
			execErrAt:    map[int]error{1: errors.New("synthetic flip failure")},
		}, nil
	}

	sub, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
	if err != nil {
		t.Fatalf("flip failure must not surface (log-and-continue), got %v", err)
	}
	if stub.cancelCalls != 1 {
		t.Errorf("paddle calls = %d, want 1", stub.cancelCalls)
	}
	if sub == nil || sub.AutoRenew {
		t.Errorf("returned sub = %+v, want AutoRenew=false (channel already cancelled)", sub)
	}
	if countAudit(t, db, "paddle_cancel_local_sync_failed") != 1 {
		t.Error("expected divergence audit paddle_cancel_local_sync_failed")
	}
	// Local row genuinely unflipped — the heal path (retry / webhook) owns it.
	if _, _, autoRenew, _ := readChannelState(t, db, subID); !autoRenew {
		t.Error("test premise broken: local row should be unflipped after synthetic failure")
	}
}

// ============================================================================
// CancelSubscriptionByID — tx-machinery branch coverage (fake dbBeginTx)
// ============================================================================

func TestCancelSubscriptionByID_TxBranches(t *testing.T) {
	newSvc := func(t *testing.T, subID string) (*PaymentService, *sqlx.DB, string) {
		db := setupPaymentDB(t)
		svc := newTestPaymentService(t, db)
		uid := seedUser(t, db)
		seedChannelSub(t, db, uid, "monthly", "active", "sub_txb_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
		svc.SetPaddleClient(&stubPaddle{})
		return svc, db, uid
	}

	t.Run("begin tx error", func(t *testing.T) {
		db := setupPaymentDB(t)
		svc := newTestPaymentService(t, db)
		uid := seedUser(t, db)
		subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_btx_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
		svc.SetPaddleClient(&stubPaddle{})
		svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
			return nil, errors.New("synthetic begin failure")
		}
		_, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
		if err == nil || !strings.Contains(err.Error(), "begin cancel tx") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("row vanished between pre-check and lock → 404", func(t *testing.T) {
		db := setupPaymentDB(t)
		svc := newTestPaymentService(t, db)
		uid := seedUser(t, db)
		subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_van_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
		svc.SetPaddleClient(&stubPaddle{})
		svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
			return &fakeTx{getErr: sql.ErrNoRows}, nil
		}
		if _, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, ""); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("lock read error wrapped", func(t *testing.T) {
		db := setupPaymentDB(t)
		svc := newTestPaymentService(t, db)
		uid := seedUser(t, db)
		subID := seedChannelSub(t, db, uid, "monthly", "active", "sub_lerr_"+mustNewUUID()[:8], "paddle", true, time.Now().Add(15*24*time.Hour))
		svc.SetPaddleClient(&stubPaddle{})
		svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
			return &fakeTx{getErr: errors.New("db down")}, nil
		}
		_, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
		if err == nil || !strings.Contains(err.Error(), "lock subscription") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("locked row no longer active → already ended", func(t *testing.T) {
		svc, _, uid := newSvc(t, "")
		var subID string
		err := svc.db.GetContext(context.Background(), &subID,
			`SELECT id FROM subscriptions WHERE user_id = $1`, uid)
		if err != nil {
			t.Fatal(err)
		}
		svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
			return &subFillingTx{
				sub: &model.Subscription{ID: subID, UserID: uid, PlanID: "monthly", Status: "cancelled", AutoRenew: false},
			}, nil
		}
		if _, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, ""); !errors.Is(err, ErrSubscriptionAlreadyEnded) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("race loser: locked auto_renew=false → idempotent 200, no paddle call", func(t *testing.T) {
		svc, _, uid := newSvc(t, "")
		var subID string
		if err := svc.db.GetContext(context.Background(), &subID,
			`SELECT id FROM subscriptions WHERE user_id = $1`, uid); err != nil {
			t.Fatal(err)
		}
		stub := &stubPaddle{}
		svc.SetPaddleClient(stub)
		svc.dbBeginTx = func(_ context.Context) (dbTx, error) {
			return &subFillingTx{
				sub: &model.Subscription{ID: subID, UserID: uid, PlanID: "monthly", Status: "active", AutoRenew: false},
			}, nil
		}
		sub, err := svc.CancelSubscriptionByID(context.Background(), uid, subID, "")
		if err != nil {
			t.Fatalf("race loser must get idempotent 200, got %v", err)
		}
		if sub == nil || sub.AutoRenew {
			t.Errorf("sub = %+v", sub)
		}
		if stub.cancelCalls != 0 {
			t.Errorf("paddle calls = %d, want 0 for the race loser", stub.cancelCalls)
		}
	})
}
