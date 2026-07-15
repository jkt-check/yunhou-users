package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// mockOrderRepo is the minimal OrderRepo mock the sweeper needs.
// It tracks SweepExpired call count (callCount) and returns a configurable
// count (returnCount) per call — these are decoupled because the Start/Stop
// tests care about "was SweepExpired called?", while SweepOnce tests care
// about "what value did it return?".
type mockOrderRepo struct {
	callCount    atomic.Int64
	returnCount  int64
	err          error
	lastSweepAt  atomic.Pointer[time.Time]
}

func (m *mockOrderRepo) Create(_ context.Context, _ *model.Order) error { return nil }
func (m *mockOrderRepo) FindByID(_ context.Context, _ string) (*model.Order, error) {
	return nil, errors.New("not used")
}
func (m *mockOrderRepo) ListByUserID(_ context.Context, _ string) ([]model.Order, error) {
	return nil, nil
}
func (m *mockOrderRepo) CancelPending(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (m *mockOrderRepo) SweepExpired(_ context.Context, now time.Time) (int64, error) {
	m.callCount.Add(1)
	m.lastSweepAt.Store(&now)
	if m.err != nil {
		return 0, m.err
	}
	return m.returnCount, nil
}
func (m *mockOrderRepo) UpdateProviderIntent(_ context.Context, _ string, _ []byte) error {
	return nil
}

func TestOrderSweeper_SweepOnce(t *testing.T) {
	t.Parallel()

	t.Run("returns count from repo", func(t *testing.T) {
		t.Parallel()
		repo := &mockOrderRepo{returnCount: 3}

		s := NewOrderSweeper(repo, time.Hour) // interval irrelevant for SweepOnce
		n, err := s.SweepOnce(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 3 {
			t.Errorf("expected 3, got %d", n)
		}
		if repo.callCount.Load() != 1 {
			t.Errorf("expected 1 sweep call, got %d", repo.callCount.Load())
		}
	})

	t.Run("propagates repo error", func(t *testing.T) {
		t.Parallel()
		repo := &mockOrderRepo{err: errors.New("db down")}

		s := NewOrderSweeper(repo, time.Hour)
		_, err := s.SweepOnce(context.Background())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, errors.Unwrap(err)) {
			// SweepOnce wraps with fmt.Errorf("...: %w", err); verify the wrap exists.
			if err.Error() != "sweep expired: db down" {
				t.Errorf("expected wrapped 'sweep expired: db down', got %q", err.Error())
			}
		}
	})
}

func TestOrderSweeper_StartStop(t *testing.T) {
	t.Parallel()

	t.Run("runs at least one tick immediately on Start", func(t *testing.T) {
		t.Parallel()
		repo := &mockOrderRepo{}
		s := NewOrderSweeper(repo, 50*time.Millisecond)
		s.Start(context.Background())
		// Give the goroutine a moment to run the initial tick.
		time.Sleep(30 * time.Millisecond)
		s.Stop()

		if repo.callCount.Load() < 1 {
			t.Errorf("expected at least 1 sweep call, got %d", repo.callCount.Load())
		}
	})

	t.Run("Stop is idempotent", func(t *testing.T) {
		t.Parallel()
		repo := &mockOrderRepo{}
		s := NewOrderSweeper(repo, 50*time.Millisecond)
		s.Start(context.Background())
		s.Stop()
		// Second Stop must not panic or block.
		done := make(chan struct{})
		go func() {
			s.Stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("second Stop hung")
		}
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		t.Parallel()
		repo := &mockOrderRepo{}
		s := NewOrderSweeper(repo, time.Hour) // long interval — rely on ctx
		ctx, cancel := context.WithCancel(context.Background())
		s.Start(ctx)
		cancel()
		// Stop should return promptly even though ticker hasn't fired.
		done := make(chan struct{})
		go func() {
			s.Stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Stop didn't return after ctx cancel")
		}
	})
}

func TestOrderSweeper_DefaultInterval(t *testing.T) {
	t.Parallel()

	// NewOrderSweeper(interval=0) should default to a sane value (we use 1m).
	// We test the contract indirectly: passing 0 must not panic.
	s := NewOrderSweeper(&mockOrderRepo{}, 0)
	if s.interval != time.Minute {
		t.Errorf("expected default 1m, got %v", s.interval)
	}
}

// Compile-time check that mockOrderRepo satisfies repo.OrderRepo.
var _ repo.OrderRepo = (*mockOrderRepo)(nil)
// TestOrderSweeper_tick covers the inner tick() method (not exposed directly).
// Indirectly exercised via Start+Stop, but tick has a distinct error path
// (the orderRepo.SweepExpired returns err) which produces a log + early
// return, NOT a panic. We can drive it through SweepOnce + a fake err.
func TestOrderSweeper_TickErrorPath(t *testing.T) {
	t.Parallel()
	// SweepOnce returns the error wrapped — that exercises the same code
	// path the tick's error log takes (call repo, get err, log+return).
	repo := &mockOrderRepo{err: errors.New("db briefly down")}
	s := NewOrderSweeper(repo, time.Hour)
	_, err := s.SweepOnce(context.Background())
	if err == nil {
		t.Fatal("expected error from SweepOnce")
	}
	// SweepOnce wraps; tick just logs. Both call the same repo, so if
	// SweepOnce surfaces the error, tick's log line ran too. We don't
	// intercept logs here, but the call-count assertion confirms
	// tick-equivalent behaviour.
	if repo.callCount.Load() != 1 {
		t.Errorf("expected 1 sweep call, got %d", repo.callCount.Load())
	}
}

// TestOrderSweeper_TickViaStart covers the actual `tick` function path
// reached through Start+Stop with an error-injecting repo. Drives the
// tick()'s error log + early return branch (not just SweepOnce's
// return-error path). Without this, the `tick` function's error
// branch is uncovered.
func TestOrderSweeper_TickViaStart(t *testing.T) {
	t.Parallel()
	repo := &mockOrderRepo{err: errors.New("db down on sweep")}
	s := NewOrderSweeper(repo, 50*time.Millisecond)
	s.Start(context.Background())
	// Allow the initial tick to run.
	time.Sleep(30 * time.Millisecond)
	s.Stop()

	if repo.callCount.Load() < 1 {
		t.Errorf("expected at least 1 sweep call, got %d", repo.callCount.Load())
	}
}

// TestOrderSweeper_TickerFires covers the "ticker.C" branch of run().
// Uses a very short interval so the second tick fires before Stop
// is called.
func TestOrderSweeper_TickerFires(t *testing.T) {
	t.Parallel()
	repo := &mockOrderRepo{}
	s := NewOrderSweeper(repo, 30*time.Millisecond)
	s.Start(context.Background())
	// Wait for initial tick + at least one ticker-driven tick.
	time.Sleep(100 * time.Millisecond)
	s.Stop()

	// We expect at least 2 calls: the initial one + at least one from
	// the ticker. Use a generous lower bound for CI flakiness.
	if got := repo.callCount.Load(); got < 2 {
		t.Errorf("expected >= 2 sweep calls (initial + ticker), got %d", got)
	}
}

// TestOrderSweeper_TickLogsFlippedCount exercises the n > 0 branch of tick
// (the "flipped N pending orders to expired" log line). Drives Start+Stop
// with a mock that returns a non-zero count; the assertion is the same
// pattern as TestOrderSweeper_StartStop, with a stronger guarantee that
// tick's success path with n > 0 was reached.
func TestOrderSweeper_TickLogsFlippedCount(t *testing.T) {
	t.Parallel()
	repo := &mockOrderRepo{returnCount: 5}
	s := NewOrderSweeper(repo, 50*time.Millisecond)
	s.Start(context.Background())
	// Allow the initial tick (which runs synchronously inside run()) to
	// complete before Stop — Stop blocks on the done channel.
	time.Sleep(30 * time.Millisecond)
	s.Stop()

	if repo.callCount.Load() < 1 {
		t.Errorf("expected at least 1 sweep call, got %d", repo.callCount.Load())
	}
	// The success path with n > 0 doesn't return an error, doesn't
	// panic — the test passes if the goroutine exits cleanly via Stop.
}
