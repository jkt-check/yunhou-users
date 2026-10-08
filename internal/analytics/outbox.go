package analytics

import (
	"context"
	"log"
	"time"
)

// Retry-scheduling constants. Backoff is exponential off a 1-minute base,
// capped at 6 hours; after retryMaxAttempts failures the event is
// dead-lettered (parked with next_attempt_at = infinity, kept for
// inspection, never picked up again).
const (
	retryBaseBackoff = time.Minute
	retryMaxBackoff  = 6 * time.Hour
	retryMaxAttempts = 10
	retryBatchLimit  = 50
)

// retryInfinity parks dead-lettered rows past every real timestamp.
var retryInfinity = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// OutboxEvent is a stored event plus its retry state.
type OutboxEvent struct {
	Event
	Attempts int
}

// OutboxStore is the persistence contract RetryWorker drives. SQL lives
// in the implementation (internal/analytics/postgres_store.go).
type OutboxStore interface {
	FailureStore
	// ListDue returns up to limit unsent events whose next attempt is due.
	ListDue(ctx context.Context, limit int) ([]OutboxEvent, error)
	// MarkSent records a successful redelivery.
	MarkSent(ctx context.Context, eventUUID string) error
	// Reschedule records a failed attempt: attempts is the new count and
	// nextAttemptAt the (possibly far-future) next due time.
	Reschedule(ctx context.Context, eventUUID string, attempts int, nextAttemptAt time.Time) error
}

// retryBackoff computes the delay before the next attempt after failure
// #attempts (1-based): base * 2^(attempts-1), capped.
func retryBackoff(attempts int) time.Duration {
	d := retryBaseBackoff
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= retryMaxBackoff {
			return retryMaxBackoff
		}
	}
	return d
}

// RetryWorker redelivers persisted delivery failures on a fixed interval.
// The send func is injected so tests don't need HTTP; production wires
// Emitter.Redeliver, which shares sendOne's wire format with first
// attempts — a redelivery carries the same UUID, so PostHog dedupes any
// overlap with the original attempt.
type RetryWorker struct {
	store    OutboxStore
	send     func(ctx context.Context, evt Event) error
	interval time.Duration
	now      func() time.Time // test hook
}

// NewRetryWorker builds the worker. interval is the poll cadence.
func NewRetryWorker(store OutboxStore, send func(ctx context.Context, evt Event) error, interval time.Duration) *RetryWorker {
	return &RetryWorker{store: store, send: send, interval: interval, now: time.Now}
}

// Run polls until ctx is cancelled: each tick claims the due batch,
// redelivers every event, and marks it sent or reschedules it with
// backoff. The store's row locks are released right after the scan, so
// overlapping instances can claim the same row — harmless, because
// PostHog dedupes on the event UUID.
func (w *RetryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// callTimeout bounds each individual outboard call (store query/update
// and redelivery POST). A wedged DB or a hung endpoint must stall at most
// this long per call — the worker is a single goroutine, and an unbounded
// hang would silently stop redeliveries (same defensive posture as
// persistFailure's 5s cap on the write path).
const callTimeout = 10 * time.Second

// boundedCall detaches one call from the caller's ctx and caps it at
// callTimeout: shutdown cancellation must not abort an in-flight
// UPDATE/POST mid-flight (miscounting a maybe-successful redelivery as a
// failure), and a hung dependency must not wedge the worker.
func boundedCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
}

// drain processes one batch of due events. ctx errors abort the pass.
func (w *RetryWorker) drain(ctx context.Context) {
	listCtx, cancel := boundedCall(ctx)
	rows, err := w.store.ListDue(listCtx, retryBatchLimit)
	cancel()
	if err != nil {
		log.Printf("analytics: outbox list due: %v", err)
		return
	}
	for _, row := range rows {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sendCtx, cancel := boundedCall(ctx)
		err := w.send(sendCtx, row.Event)
		cancel()
		if err != nil {
			w.handleFailure(ctx, row)
			continue
		}
		markCtx, cancel := boundedCall(ctx)
		err = w.store.MarkSent(markCtx, row.UUID)
		cancel()
		if err != nil {
			log.Printf("analytics: outbox mark sent %s: %v", row.UUID, err)
		}
	}
}

// handleFailure reschedules a failed redelivery. Attempts counts failures:
// the 10th failure dead-letters the row — kept in the table for inspection
// but parked with next_attempt_at = infinity so ListDue never picks it up
// again.
func (w *RetryWorker) handleFailure(ctx context.Context, row OutboxEvent) {
	attempts := row.Attempts + 1
	if attempts >= retryMaxAttempts {
		log.Printf("analytics: outbox DEAD-LETTER %q (%s) after %d attempts — manual inspection required",
			row.Name, row.UUID, attempts)
		ctx, cancel := boundedCall(ctx)
		defer cancel()
		if err := w.store.Reschedule(ctx, row.UUID, attempts, retryInfinity); err != nil {
			log.Printf("analytics: outbox dead-letter %s: %v", row.UUID, err)
		}
		return
	}
	next := w.now().Add(retryBackoff(attempts))
	ctx, cancel := boundedCall(ctx)
	defer cancel()
	if err := w.store.Reschedule(ctx, row.UUID, attempts, next); err != nil {
		log.Printf("analytics: outbox reschedule %s: %v", row.UUID, err)
	}
}
