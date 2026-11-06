package workers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// rate_reservation_reaper_test.go — T4 速率预占保留期清理。

// fakeRateStore records the purge calls.
type fakeRateStore struct {
	calls   []time.Time
	removed int64
	err     error
}

func (s *fakeRateStore) PurgeRateReservations(ctx context.Context, cutoff time.Time) (int64, error) {
	s.calls = append(s.calls, cutoff)
	if s.err != nil {
		return 0, s.err
	}
	return s.removed, nil
}

func TestRateReaper_PurgesTerminalReservationsOlderThanTheRetention(t *testing.T) {
	store := &fakeRateStore{removed: 3}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	reaper := NewRateReservationReaper(store, domain.FixedClock{T: now}, RateReaperConfig{Retention: 24 * time.Hour})

	removed, err := reaper.RunPass(context.Background())
	if err != nil {
		t.Fatalf("run pass: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}
	if len(store.calls) != 1 {
		t.Fatalf("purge calls = %d, want 1", len(store.calls))
	}
	want := now.Add(-24 * time.Hour)
	if !store.calls[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v (now - retention)", store.calls[0], want)
	}
}

func TestRateReaper_DefaultsKeepTheRetentionPositive(t *testing.T) {
	store := &fakeRateStore{}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	// 零值配置 → 默认保留期 24 小时（不得因为保留期非正就把审计痕迹清空）。
	reaper := NewRateReservationReaper(store, domain.FixedClock{T: now}, RateReaperConfig{})
	if _, err := reaper.RunPass(context.Background()); err != nil {
		t.Fatalf("run pass: %v", err)
	}
	want := now.Add(-24 * time.Hour)
	if len(store.calls) != 1 || !store.calls[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", store.calls, want)
	}
}

func TestRateReaper_PropagatesTheStoreFailure(t *testing.T) {
	store := &fakeRateStore{err: errors.New("connection refused")}
	reaper := NewRateReservationReaper(store, nil, RateReaperConfig{})
	if _, err := reaper.RunPass(context.Background()); err == nil {
		t.Fatal("a store failure must be reported (the sweep is retried on the next tick)")
	}
}
