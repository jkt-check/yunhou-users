package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/workers"
)

type nonstreamFinalizationStore struct {
	*postgres.Store
	beforeFinish func(context.Context, string)
}

func (s *nonstreamFinalizationStore) FinishAttempt(ctx context.Context, id, status, kind, upstreamID string, at time.Time) error {
	s.beforeFinish(ctx, id)
	return s.Store.FinishAttempt(ctx, id, status, kind, upstreamID, at)
}

func TestGateway_NonstreamFinalizesBeforeReleasingLeases(t *testing.T) {
	for _, requestLeases := range []bool{false, true} {
		name, wantLeases := "upstream_only", 1
		var opts []fixtureOpt
		if requestLeases {
			name, wantLeases = "upstream_account_and_key", 3
			opts = []fixtureOpt{withNoWindows(), withKeyConcurrencyLimit(1)}
		}
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t, nonStreamUsage(10, 5))
			f := newFixture(t, up, opts...)
			f.routing.LeaseTTL = 150 * time.Millisecond
			dep := f.snap.Deployments[f.deploymentID]
			// A legal timeout just below the 15m recovery grace can leave
			// less than one finalization operation's budget before recovery.
			dep.RequestTimeout = 15*time.Minute - 100*time.Millisecond
			f.snap.Deployments[f.deploymentID] = dep
			f.gateway.store = &nonstreamFinalizationStore{
				Store: f.store,
				beforeFinish: func(ctx context.Context, attemptID string) {
					var requestID string
					if err := f.db.GetContext(ctx, &requestID,
						`SELECT request_id FROM inference_attempts WHERE id=$1`, attemptID); err != nil {
						t.Fatal(err)
					}
					// Model a dispatch that just completed inside its timeout.
					// Finalization then crosses the grace boundary while the
					// gateway still holds the actual reported usage in memory.
					if _, err := f.db.ExecContext(ctx, `UPDATE inference_requests
						SET updated_at=now()-interval '15 minutes'+interval '200 milliseconds'
						WHERE id=$1`, requestID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.db.ExecContext(ctx, `UPDATE inference_attempts
						SET started_at=now()-interval '15 minutes'+interval '200 milliseconds'
						WHERE id=$1`, attemptID); err != nil {
						t.Fatal(err)
					}
					select {
					case <-time.After(350 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					var live int
					if err := f.db.GetContext(ctx, &live, `SELECT COUNT(*)
						FROM inference_concurrency_leases
						WHERE request_id=$1 AND state='held' AND expires_at>now()`, requestID); err != nil {
						t.Fatal(err)
					}
					if live != wantLeases {
						t.Errorf("live leases before finalization = %d, want %d (including renewed upstream)", live, wantLeases)
					}
					recovery := workers.NewSettlementRecovery(f.store, nil, workers.RecoveryConfig{}, nil)
					stats, err := recovery.RunPass(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if stats.Scanned != 0 {
						t.Errorf("recovery scanned live finalization: %+v", stats)
					}
				},
			}
			p, key := f.principal()
			out, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			status, source, _, charge := f.requestRow(t, out.RequestID)
			if status != "settled" || source != "reported" || charge != 20 {
				t.Errorf("request=%s/%s charge=%d, want settled/reported/20", status, source, charge)
			}
			if got := heldLeases(t, f); got != 0 {
				t.Errorf("held leases after finalization = %d, want 0", got)
			}
			var charges, jobs, tokens int
			for _, check := range []struct {
				dest *int
				sql  string
			}{
				{&charges, `SELECT COUNT(*) FROM inference_ledger_entries WHERE request_id=$1 AND entry_type='charge'`},
				{&jobs, `SELECT COUNT(*) FROM inference_reconciliation_jobs WHERE request_id=$1`},
				{&tokens, `SELECT settled_tokens FROM inference_rate_reservations WHERE request_id=$1 AND scope='account'`},
			} {
				if err := f.db.Get(check.dest, check.sql, out.RequestID); err != nil {
					t.Fatal(err)
				}
			}
			if charges != 1 || jobs != 0 || tokens != 15 {
				t.Errorf("charges=%d jobs=%d settled_tokens=%d, want 1/0/15", charges, jobs, tokens)
			}
		})
	}
}

func TestGateway_NonstreamFinalizationPanicReleasesAllLeases(t *testing.T) {
	up := newUpstream(t, nonStreamUsage(10, 5))
	f := newFixture(t, up, withNoWindows(), withKeyConcurrencyLimit(1))
	f.gateway.store = &nonstreamFinalizationStore{
		Store: f.store,
		beforeFinish: func(context.Context, string) {
			if got := heldLeases(t, f); got != 3 {
				t.Errorf("held leases before finalization panic = %d, want 3", got)
			}
			panic("injected nonstream finalization panic")
		},
	}
	p, key := f.principal()
	func() {
		defer func() {
			if got := recover(); got != "injected nonstream finalization panic" {
				t.Errorf("panic = %v, want injected finalization panic", got)
			}
		}()
		_, _ = f.gateway.ChatCompletions(context.Background(), p, key,
			domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	}()
	if got := heldLeases(t, f); got != 0 {
		t.Errorf("held leases after finalization panic = %d, want 0", got)
	}
}
