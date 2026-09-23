package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/yunhou/users/internal/repo"
)

// OrderSweeper periodically flips long-pending orders to 'expired'.
// The sweeper interval MUST be much shorter than the order expiry window
// (default 30 min) so state changes propagate quickly — see design doc
// §"v1 decisions on order lifecycle".
//
// Trade-off: running this in-process means N instances each scan the
// table on their own interval. The UPDATE is idempotent
// (WHERE status='pending') so duplicate work is harmless; for higher
// scale, swap to an external cron / k8s CronJob later.
//
// Task 10: the same janitor cadence also flips entitlements whose
// effective_to has passed to status='expired' (EntitlementExpirer). That
// is status hygiene only — authorization is already cut off by the
// effective_to bound in the resolver — but it keeps the stored state
// honest for ops views and the drift scan.
type OrderSweeper struct {
	orderRepo repo.OrderRepo
	interval  time.Duration
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once

	// entitlements, when wired (production), receives the periodic
	// expiry-marking pass. Nil in tests that only exercise order sweeping.
	entitlements EntitlementExpirer
}

// EntitlementExpirer flips naturally-lapsed entitlements to 'expired'.
// Implemented by inference/postgres.Store.
type EntitlementExpirer interface {
	MarkExpiredEntitlements(ctx context.Context, now time.Time) (int64, error)
}

// NewOrderSweeper returns a sweeper that runs Run() loop. interval is
// typically 1 minute (much shorter than the 30-minute order expiry).
func NewOrderSweeper(orderRepo repo.OrderRepo, interval time.Duration) *OrderSweeper {
	if interval == 0 {
		interval = 1 * time.Minute
	}
	return &OrderSweeper{
		orderRepo: orderRepo,
		interval:  interval,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// SetEntitlementExpirer wires the entitlement lapse marker (Task 10).
func (s *OrderSweeper) SetEntitlementExpirer(e EntitlementExpirer) { s.entitlements = e }

// Start kicks off the sweeper goroutine. It returns immediately; call
// Stop to terminate. Safe to call once; subsequent calls are no-ops.
func (s *OrderSweeper) Start(ctx context.Context) {
	go s.run(ctx)
}

// Stop signals the sweeper to exit and waits for the goroutine to
// finish its current tick. Idempotent.
func (s *OrderSweeper) Stop() {
	s.once.Do(func() {
		close(s.stop)
	})
	<-s.done
}

func (s *OrderSweeper) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()

	// Run once immediately so a fresh start picks up any backlog
	// without waiting `interval`.
	s.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick performs one sweep pass. Errors are logged but do not stop the
// loop — transient DB errors should not kill the sweeper.
func (s *OrderSweeper) tick(ctx context.Context) {
	// Use a fresh background context if the parent is already cancelled,
	// so a single failed tick doesn't prevent subsequent ticks.
	tickCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	n, err := s.orderRepo.SweepExpired(tickCtx, time.Now())
	if err != nil {
		log.Printf("sweeper: tick failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("sweeper: flipped %d pending orders to expired", n)
	}

	// Task 10: mark naturally-lapsed entitlements 'expired'. Authorization
	// never depended on this flip (effective_to bounds selection), so a
	// failure here is logged and retried next tick, never fatal.
	if s.entitlements != nil {
		m, err := s.entitlements.MarkExpiredEntitlements(tickCtx, time.Now())
		if err != nil {
			log.Printf("sweeper: entitlement expiry pass failed: %v", err)
		} else if m > 0 {
			log.Printf("sweeper: marked %d lapsed entitlements expired", m)
		}
	}
}

// SweepOnce runs a single sweep pass synchronously. Useful for tests
// and one-shot CLI commands.
func (s *OrderSweeper) SweepOnce(ctx context.Context) (int64, error) {
	n, err := s.orderRepo.SweepExpired(ctx, time.Now())
	if err != nil {
		return 0, fmt.Errorf("sweep expired: %w", err)
	}
	return n, nil
}
