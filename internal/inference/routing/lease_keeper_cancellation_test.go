package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

type blockedRenewalStore struct {
	*fakeStore
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (s *blockedRenewalStore) RenewLease(ctx context.Context, leaseID, ownerToken string, fencing int64, newExpiresAt time.Time) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.unblock:
		return errors.New("renewal unblocked by test cleanup")
	}
}

func TestLeaseKeeper_CleanupInterruptsBlockedRenewal(t *testing.T) {
	for _, pause := range []bool{false, true} {
		name := "stop"
		if pause {
			name = "pause"
		}
		t.Run(name, func(t *testing.T) {
			store := &blockedRenewalStore{
				fakeStore: newFakeStore(),
				started:   make(chan struct{}),
				unblock:   make(chan struct{}),
			}
			svc := NewService(store, nil, nil)
			svc.LeaseTTL = 30 * time.Millisecond
			lease, err := svc.AcquireUpstreamLease(context.Background(), nil,
				account("acct-1", "prov-a", 2), "req-1", svc.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			dispatchCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			keeper := svc.NewLeaseKeeperWithCancel(cancel, lease)
			t.Cleanup(func() {
				close(store.unblock)
				keeper.Stop(context.Background())
			})
			select {
			case <-store.started:
			case <-time.After(2 * time.Second):
				t.Fatal("renewal did not start")
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				if pause {
					keeper.Pause()
				} else {
					ctx, stopCancel := context.WithTimeout(context.Background(), time.Second)
					defer stopCancel()
					keeper.Stop(ctx)
				}
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("lease cleanup stayed blocked on renewal")
			}
			if keeper.Err() != nil {
				t.Fatalf("normal cleanup reported fencing: %v", keeper.Err())
			}
			if dispatchCtx.Err() != nil {
				t.Fatal("normal cleanup cancelled the upstream dispatch")
			}

			store.mu.Lock()
			defer store.mu.Unlock()
			wantState := domain.LeaseReleased
			wantReleases := 1
			if pause {
				wantState = domain.LeaseHeld
				wantReleases = 0
			}
			if store.leases[lease.ID].State != wantState {
				t.Errorf("lease state = %s, want %s", store.leases[lease.ID].State, wantState)
			}
			if len(store.released) != wantReleases {
				t.Errorf("lease releases = %d, want %d", len(store.released), wantReleases)
			}
		})
	}
}
