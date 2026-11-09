package access

import (
	"math"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestEvaluateRateLimits_TPMCannotWrapIntoAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		settled, inflight, hold int64
		limit                   int64
		blocked                 bool
	}{
		{"settled_plus_hold", math.MaxInt64, 0, 20, 100, true},
		{"inflight_plus_hold_at_max_limit", 0, math.MaxInt64 - 5, 10, math.MaxInt64, true},
		{"positive_wrapped_sum", math.MaxInt64, math.MaxInt64, 3, 100, true},
		{"exact_max_limit", math.MaxInt64 - 10, 5, 5, math.MaxInt64, false},
		{"disabled", math.MaxInt64, math.MaxInt64, 3, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := EvaluateRateLimits(domain.RateLimits{PolicyTPM: tc.limit},
				domain.RateUsage{SettledTokens: tc.settled, InflightTokens: tc.inflight}, tc.hold)
			if !tc.blocked {
				if err != nil {
					t.Fatalf("admission rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("over-limit tokens admitted after integer overflow")
			}
			if domain.CodeOf(err) != domain.CodeRateLimited || len(err.Blocks) != 1 ||
				err.Blocks[0].Kind != RateKindPolicyTPM || err.Blocks[0].Used != math.MaxInt64 {
				t.Errorf("rejection = %+v, want policy_tpm with saturated diagnostic", err)
			}
			if err.RetryAfterSeconds != 0 {
				t.Errorf("invented recovery time: %d", err.RetryAfterSeconds)
			}
		})
	}
}
