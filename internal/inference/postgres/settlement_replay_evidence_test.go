package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestSettle_ReplayRequiresSameSourceAndRawEvidence(t *testing.T) {
	const raw = `{"schema_version":1,"usage":{"input_tokens":1000,"output_tokens":500}}`
	cases := []struct {
		name         string
		original     domain.UsageSource
		delivered    domain.UsageSource
		originalRaw  string
		deliveredRaw string
		want         domain.SettleOutcomeKind
	}{
		{"estimated_to_reported", domain.UsageEstimated, domain.UsageReported, raw, raw, domain.SettleOutcomeChargeMismatch},
		{"reported_to_estimated", domain.UsageReported, domain.UsageEstimated, raw, raw, domain.SettleOutcomeChargeMismatch},
		{"different_raw", domain.UsageReported, domain.UsageReported, raw,
			`{"schema_version":1,"usage":{"input_tokens":999,"output_tokens":501}}`, domain.SettleOutcomeChargeMismatch},
		{"malformed_raw", domain.UsageReported, domain.UsageReported, raw,
			`{"schema_version":1,"usage":`, domain.SettleOutcomeChargeMismatch},
		{"nul_escape", domain.UsageReported, domain.UsageReported, raw,
			`{"schema_version":1,"usage":{"value":"\u0000"}}`, domain.SettleOutcomeChargeMismatch},
		{"numeric_overflow", domain.UsageReported, domain.UsageReported, raw,
			`{"schema_version":1,"usage":{"value":1e1000000}}`, domain.SettleOutcomeChargeMismatch},
		{"lone_surrogate", domain.UsageReported, domain.UsageReported, raw,
			`{"schema_version":1,"usage":{"value":"\ud800"}}`, domain.SettleOutcomeChargeMismatch},
		{"invalid_utf8", domain.UsageReported, domain.UsageReported, raw,
			"{\"schema_version\":1,\"usage\":{\"value\":\"\xff\"}}", domain.SettleOutcomeChargeMismatch},
		{"missing_raw", domain.UsageReported, domain.UsageReported, raw, "", domain.SettleOutcomeChargeMismatch},
		{"new_raw", domain.UsageReported, domain.UsageReported, "", raw, domain.SettleOutcomeChargeMismatch},
		{"exact_replay", domain.UsageReported, domain.UsageReported, raw, raw, domain.SettleOutcomeDuplicate},
		{"semantic_json_replay", domain.UsageReported, domain.UsageReported, raw,
			`{ "usage": { "output_tokens": 500.0, "input_tokens": 1000 }, "schema_version": 1.0 }`, domain.SettleOutcomeDuplicate},
		{"implicit_default", domain.UsageReported, domain.UsageReported, "", `{"schema_version":1}`, domain.SettleOutcomeDuplicate},
		{"explicit_default", domain.UsageReported, domain.UsageReported, `{"schema_version":1}`, "", domain.SettleOutcomeDuplicate},
	}
	for _, source := range []string{"plan", "wallet"} {
		t.Run(source, func(t *testing.T) {
			for _, charge := range []int64{0, 60_000} {
				name := "positive"
				if charge == 0 {
					name = "zero"
				}
				t.Run(name, func(t *testing.T) {
					for _, tc := range cases {
						t.Run(tc.name, func(t *testing.T) {
							_, s := testDB(t)
							ctx := context.Background()
							var requestID, attemptID string
							if source == "wallet" {
								requestID, attemptID, _ = walletAdmission(t, s)
							} else {
								f := seedFixture(t, s, true)
								w5, ww, wm := makeWindows(t, s, f.entID)
								var err error
								requestID, err = admitWithRate(t, s, f, w5, ww, wm,
									domain.RateLimits{PolicyTPM: 100_000}, 1000)
								if err != nil {
									t.Fatal(err)
								}
								attemptID = uuid.NewString()
								if err := s.InsertAttempt(ctx, &domain.Attempt{
									ID: attemptID, RequestID: requestID, AttemptNo: 1,
								}); err != nil {
									t.Fatal(err)
								}
							}
							cmd := walletSettleCmd(t, requestID, attemptID, charge)
							if source == "plan" {
								cmd.WalletCharge = nil
							}
							cmd.Usage.Source = tc.original
							cmd.Usage.RawUsage.Raw = json.RawMessage(tc.originalRaw)
							uow, err := s.Begin(ctx)
							if err != nil {
								t.Fatal(err)
							}
							defer uow.Rollback(ctx)
							if err := s.Settle(ctx, uow, cmd); err != nil {
								t.Fatal(err)
							}
							if err := uow.Commit(ctx); err != nil {
								t.Fatal(err)
							}
							replay, err := s.Begin(ctx)
							if err != nil {
								t.Fatal(err)
							}
							defer replay.Rollback(ctx)
							tx := mustTx(t, replay)
							before := settledAccountingSnapshot(t, tx, requestID)
							cmd.Usage.Source = tc.delivered
							cmd.Usage.RawUsage.Raw = json.RawMessage(tc.deliveredRaw)
							for _, revision := range []int{0, 1} {
								cmd.Usage.Revision = revision
								err := s.Settle(ctx, replay, cmd)
								outcome, ok := domain.AsSettleError(err)
								if !ok || outcome.Kind != tc.want {
									t.Errorf("revision %d = %v, want %s", revision, err, tc.want)
								} else if outcome.ExistingChargeMicros == nil ||
									*outcome.ExistingChargeMicros != domain.Microcredit(charge) ||
									outcome.RequestedChargeMicros != domain.Microcredit(charge) ||
									outcome.Status != domain.ReqSettled {
									t.Errorf("replay lost charge/terminal facts: %+v", outcome)
								}
								if after := settledAccountingSnapshot(t, tx, requestID); after != before {
									t.Error("replay changed persisted accounting before returning")
								}
							}
						})
					}
				})
			}
		})
	}
}
