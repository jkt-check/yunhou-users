package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/quota"
)

// The storage operation consumes the real finalization budget; cleanup
// must still reach PostgreSQL with its own live context.
type expiredFinalizationStore struct {
	*postgres.Store
	release bool
}

func (s *expiredFinalizationStore) UpdateRequestStatus(ctx context.Context, id string, status domain.RequestStatus, detail string) error {
	if s.release && status == domain.ReqDispatching {
		return errors.New("dispatch status storage unavailable")
	}
	if !s.release && status == domain.ReqNonStreaming {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.Store.UpdateRequestStatus(ctx, id, status, detail)
}

func (s *expiredFinalizationStore) Release(ctx context.Context, w domain.UnitOfWork, id string) error {
	if s.release {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.Store.Release(ctx, w, id)
}

func TestCleanup_UsesFreshDeadlineAfterFinalizationTimeout(t *testing.T) {
	for _, release := range []bool{false, true} {
		name := "nonstream_settlement"
		if release {
			name = "zero_consumption_release"
		}
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t, nonStreamUsage(10, 5))
			f := newFixture(t, up, withKeyConcurrencyLimit(1))
			if _, err := f.db.Exec(`UPDATE inference_policy_versions SET concurrency_limit=2 WHERE id=$1`, f.policyID); err != nil {
				t.Fatal(err)
			}
			store := &expiredFinalizationStore{Store: f.store, release: release}
			f.gateway.store = store
			f.gateway.quotaSvc = quota.NewService(store, nil)
			p, key := f.principal()
			_, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if release && err == nil {
				t.Fatal("dispatch storage failure must reject")
			}
			if !release && err != nil {
				t.Fatal(err)
			}
			var held, reserved, rates int
			if err := f.db.Get(&held, `SELECT COUNT(*) FROM inference_concurrency_leases WHERE state='held'`); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Get(&reserved, `SELECT COUNT(*) FROM inference_reservations WHERE state='held'`); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Get(&rates, `SELECT COUNT(*) FROM inference_rate_reservations WHERE state='reserved' AND scope='account'`); err != nil {
				t.Fatal(err)
			}
			if held != 0 {
				t.Fatalf("held leases after finalization timeout = %d, want 0 (account and key cleanup)", held)
			}
			if reserved == 0 {
				t.Fatal("failed finalization must retain monetary holds for recovery")
			}
			if rates != 1 {
				t.Fatalf("failed finalization must retain TPM hold for recovery, got %d", rates)
			}
		})
	}
}
