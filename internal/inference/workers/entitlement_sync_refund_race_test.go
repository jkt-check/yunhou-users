package workers

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/model"
)

type pausedSyncBeginStore struct {
	*postgres.Store
	once    sync.Once
	reached chan struct{}
	resume  chan struct{}
}

func (s *pausedSyncBeginStore) Begin(ctx context.Context) (domain.UnitOfWork, error) {
	s.once.Do(func() {
		close(s.reached)
		select {
		case <-s.resume:
		case <-ctx.Done():
		}
	})
	return s.Store.Begin(ctx)
}

func TestEntitlementSync_RefundBeforeApplyCannotLeaveStaleGrant(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "outbox"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			f := newSyncFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f.seedPlan(t, "cp_basic", model.ProductCodingPlan, 30, 29.9)
			orderID := f.seedPaidOrderFull(t, "cp_basic", &f.policyID,
				[]string{f.modelID}, model.BenefitGrantModeSubscription, model.OrderKindNew)
			subID := f.seedSubscription(t, "cp_basic", model.ProductCodingPlan, "active", nil)
			account, err := f.store.EnsureBillingAccount(ctx, f.userID)
			if err != nil {
				t.Fatal(err)
			}
			f.enqueue(t, access.EntitlementSyncMessage{
				UserID: f.userID, ProductCode: model.ProductCodingPlan,
				SubscriptionID: subID, Reason: access.SyncReasonPaymentPaid, OrderID: orderID,
			}, nil)

			paused := &pausedSyncBeginStore{
				Store: f.store, reached: make(chan struct{}), resume: make(chan struct{}),
			}
			worker := NewEntitlementSync(paused, nil, EntitlementSyncConfig{})
			done := make(chan error, 1)
			go func() {
				if direct {
					_, err := worker.SyncUserProduct(ctx, f.userID, model.ProductCodingPlan, subID)
					done <- err
					return
				}
				stats, err := worker.RunPass(ctx)
				if err == nil && stats.Failed != 0 {
					err = domain.NewError(domain.CodeInternal, "paused worker failed")
				}
				done <- err
			}()
			defer func() {
				close(paused.resume)
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("paused sync: %v", err)
					}
				case <-ctx.Done():
					t.Error("paused sync did not finish")
				}
				active, err := f.store.ListActiveEntitlements(ctx, account.ID, time.Now().UTC())
				if err != nil {
					t.Error(err)
				} else if len(active) != 0 {
					t.Errorf("refunded subscription regained usable entitlements: %+v", active)
				}
			}()
			select {
			case <-paused.reached:
			case <-ctx.Done():
				t.Fatal("sync never reached its applying transaction")
			}

			// Match the payment domain's atomic refund/cancel + outbox effect.
			tx, err := f.db.BeginTxx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback() //nolint:errcheck
			if _, err := tx.ExecContext(ctx, `UPDATE orders SET status = 'refunded' WHERE id = $1`, orderID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, subID); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(access.EntitlementSyncMessage{
				UserID: f.userID, ProductCode: model.ProductCodingPlan,
				SubscriptionID: subID, Reason: access.SyncReasonRefundFull, OrderID: orderID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.EnqueueOutboxSQLTx(ctx, tx, access.TopicEntitlementSync, payload, nil); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			stats, err := f.worker.RunPass(ctx)
			if err != nil || stats.Delivered != 2 || stats.Noops != 2 {
				t.Fatalf("refund converger: stats=%+v err=%v; want both messages noop-delivered", stats, err)
			}
			var pending int
			if err := f.db.GetContext(ctx, &pending,
				`SELECT count(*) FROM inference_outbox WHERE status = 'pending'`); err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Fatalf("pending messages = %d; expected no later message to repair a stale grant", pending)
			}
		})
	}
}
