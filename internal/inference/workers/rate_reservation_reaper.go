// rate_reservation_reaper.go — 速率预占的保留期清理（Task 4）。
//
// 清理策略（设计 §7.2：禁止仅凭 TTL 释放未结算预占）：
//
//   - 只删**已终结**的速率预占行（state='settled' / 'released'），且必须
//     早于保留期截止点；
//   - state='reserved' 的行**永不删除**：在途预占仍占着 TPM 额度，长流跨
//     60 秒窗口也必须继续占用（不能靠 TTL 释放）；
//   - 保留期默认 24 小时（远超 60 秒滚动窗口）：窗口内的计数与额度占用只
//     由滚动窗口决定，保留期只影响审计留痕与表体积。
//
// 单独成 worker（而不是塞进结算恢复的 pass）：结算恢复的 store 契约是
// 「结算/核对」，速率保留期是「表体积维护」，两者生命周期不同。

package workers

import (
	"context"
	"log"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// RateReservationStore is the narrow persistence surface the reaper
// needs (satisfied by *postgres.Store).
type RateReservationStore interface {
	// PurgeRateReservations deletes the terminal rate reservations older
	// than the cutoff and returns how many rows were removed. It never
	// deletes an unsettled (state='reserved') reservation.
	PurgeRateReservations(ctx context.Context, cutoff time.Time) (int64, error)
}

// RateReaperConfig tunes the reaper; zero values take the defaults.
type RateReaperConfig struct {
	// Interval between sweeps (default 1h).
	Interval time.Duration
	// Retention is how long a terminal rate reservation is kept (default
	// 24h). It MUST be positive: a non-positive retention would purge the
	// audit trail immediately.
	Retention time.Duration
}

func (c RateReaperConfig) withDefaults() RateReaperConfig {
	out := c
	if out.Interval <= 0 {
		out.Interval = time.Hour
	}
	if out.Retention <= 0 {
		out.Retention = 24 * time.Hour
	}
	return out
}

// RateReservationReaper removes the rate reservations that fell out of the
// retention period.
type RateReservationReaper struct {
	store RateReservationStore
	clock domain.Clock
	cfg   RateReaperConfig
}

// NewRateReservationReaper builds the reaper; a nil clock uses the system
// clock (UTC).
func NewRateReservationReaper(store RateReservationStore, clock domain.Clock, cfg RateReaperConfig) *RateReservationReaper {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &RateReservationReaper{store: store, clock: clock, cfg: cfg.withDefaults()}
}

// Start sweeps immediately and then ticks until ctx is done. A failing
// sweep is logged and retried on the next tick; it never panics the
// process (the same guard as the settlement recovery worker).
func (r *RateReservationReaper) Start(ctx context.Context) {
	log.Printf("inference rate reservation reaper started (interval=%s retention=%s)",
		r.cfg.Interval, r.cfg.Retention)
	sweep := func() {
		if _, err := r.RunPass(ctx); err != nil {
			log.Printf("ERROR inference rate reaper: pass failed: %v", err)
		}
	}
	sweep()
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("inference rate reservation reaper stopped")
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// RunPass executes one sweep and returns the number of removed rows.
func (r *RateReservationReaper) RunPass(ctx context.Context) (int64, error) {
	cutoff := r.clock.Now().Add(-r.cfg.Retention)
	removed, err := r.store.PurgeRateReservations(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if removed > 0 {
		log.Printf("inference rate reaper: purged %d terminal rate reservations older than %s",
			removed, cutoff.Format(time.RFC3339))
	}
	return removed, nil
}
