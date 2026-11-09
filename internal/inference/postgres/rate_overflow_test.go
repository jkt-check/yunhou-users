package postgres

import (
	"errors"
	"math"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestReserveRateTx_TPMOverflowCannotBypassLimit(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		settled, inflight, hold int64
		limit                   int64
		wantRecovery            bool
	}{
		{"settled_plus_hold", math.MaxInt64, 0, 20, 100, true},
		{"inflight_plus_hold_at_max_limit", 0, math.MaxInt64 - 5, 10, math.MaxInt64, false},
		{"positive_wrapped_sum", math.MaxInt64, math.MaxInt64, 3, 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := testDB(t)
			f := seedFixture(t, s, true)
			w5, ww, wm := makeWindows(t, s, f.entID)
			var admitted int
			if tc.settled > 0 {
				requestID, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{}, 1)
				if err != nil {
					t.Fatal(err)
				}
				settleRequest(t, s, requestID, domain.UsageReported, tc.settled, 0)
				admitted++
			}
			if tc.inflight > 0 {
				if _, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{}, tc.inflight); err != nil {
					t.Fatal(err)
				}
				admitted++
			}
			type effects struct {
				Requests, MoneyHolds, RateRows  int64
				WindowUsed, WindowHeld, KeyUsed int64
			}
			snapshot := func() effects {
				t.Helper()
				var e effects
				if err := s.db.QueryRowx(
					`SELECT
					   (SELECT COUNT(*) FROM inference_requests),
					   (SELECT COUNT(*) FROM inference_reservations),
					   (SELECT COUNT(*) FROM inference_rate_reservations),
					   (SELECT COALESCE(SUM(used_micros),0) FROM inference_quota_windows),
					   (SELECT COALESCE(SUM(reserved_micros),0) FROM inference_quota_windows),
					   (SELECT budget_used_micros FROM inference_api_keys WHERE id=$1)`, f.keyID).
					Scan(&e.Requests, &e.MoneyHolds, &e.RateRows, &e.WindowUsed, &e.WindowHeld, &e.KeyUsed); err != nil {
					t.Fatal(err)
				}
				return e
			}
			before := snapshot()
			if before.Requests != int64(admitted) {
				t.Fatalf("fixture requests = %d, want %d", before.Requests, admitted)
			}
			_, err := admitWithRate(t, s, f, w5, ww, wm, domain.RateLimits{PolicyTPM: tc.limit}, tc.hold)
			var rle *domain.RateLimitError
			if !errors.As(err, &rle) {
				t.Errorf("overflow admission = %v, want typed rate rejection", err)
			} else {
				if len(rle.Blocks) != 1 || rle.Blocks[0].Kind != "policy_tpm" {
					t.Errorf("blocks = %+v, want policy_tpm only", rle.Blocks)
				}
				if tc.wantRecovery {
					if rle.RetryAfterSeconds <= 0 || rle.RetryAfterSeconds > 60 {
						t.Errorf("settled-only recovery = %d, want 1..60", rle.RetryAfterSeconds)
					}
				} else if rle.RetryAfterSeconds != 0 {
					t.Errorf("inflight recovery = %d, want unknown", rle.RetryAfterSeconds)
				}
			}
			if after := snapshot(); after != before {
				t.Errorf("refused admission changed state: before=%+v after=%+v", before, after)
			}
		})
	}
}
