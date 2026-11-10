package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

type terminalObservation struct {
	RequestID             string                     `json:"request_id"`
	AttemptID             string                     `json:"attempt_id"`
	Source                domain.UsageSource         `json:"source"`
	Buckets               map[string]*int64          `json:"buckets"`
	RawUsageSchemaVersion int                        `json:"raw_usage_schema_version"`
	RawUsage              json.RawMessage            `json:"raw_usage"`
	RawUsageText          *string                    `json:"raw_usage_text"`
	RawUsageBase64        []byte                     `json:"raw_usage_base64"`
	Price                 terminalEvidencePriceTest  `json:"price"`
	Charge                terminalEvidenceChargeTest `json:"charge"`
}

type terminalEvidencePriceTest struct {
	ID       string `json:"id"`
	ModelID  string `json:"model_id"`
	Kind     string `json:"kind"`
	Revision int    `json:"revision"`
	Currency string `json:"currency"`
}

type terminalEvidenceChargeTest struct {
	Source       string `json:"source"`
	AmountMicros int64  `json:"amount_micros"`
	Unit         string `json:"unit"`
	Currency     string `json:"currency"`
}

func loadTerminalEvidence(t *testing.T, f *fixture, requestID string, want int) map[string]terminalObservation {
	t.Helper()
	var raw []byte
	if err := f.db.Get(&raw, `SELECT detail FROM inference_reconciliation_jobs WHERE request_id=$1`, requestID); err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Evidence struct {
			SchemaVersion int                            `json:"schema_version"`
			Observations  map[string]terminalObservation `json:"observations"`
		} `json:"settlement_evidence"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Evidence.SchemaVersion != 1 || len(detail.Evidence.Observations) != want {
		t.Fatalf("incoming evidence missing or overwritten: want %d observations, got %s", want, raw)
	}
	for id := range detail.Evidence.Observations {
		if bytes, err := hex.DecodeString(id); err != nil || len(bytes) != 32 {
			t.Errorf("observation identity = %q, want SHA-256", id)
		}
	}
	return detail.Evidence.Observations
}

func assertTerminalBucket(t *testing.T, buckets map[string]*int64, name string, want *int64) {
	t.Helper()
	got, exists := buckets[name]
	if !exists || !reflect.DeepEqual(got, want) {
		t.Errorf("bucket %s = %v (present=%t), want %v", name, got, exists, want)
	}
}

func terminalUsage(t *testing.T, f *fixture, requestID string) domain.UsageRecord {
	t.Helper()
	records, err := f.store.LoadUsageRecords(context.Background(), requestID)
	if err != nil || len(records) != 1 {
		t.Fatalf("original usage = %v, %v", records, err)
	}
	return records[0]
}

func TestGateway_TerminalEvidenceRetainsDistinctNormalizedObservations(t *testing.T) {
	for _, wallet := range []bool{false, true} {
		name := "plan"
		if wallet {
			name = "wallet"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, newUpstream(t, nonStreamUsage(10, 5)))
			ctx := context.Background()
			priceID, kind, unit, currency, wantCharge := f.priceID, "sale_credit", "microcredit", "", int64(31)
			if wallet {
				priceID = seedWalletPrice(t, f)
				fundAndEnable(t, f, 100_000_000, 100_000_000)
				shrinkPlanToExhausted(t, f, "allow_overage")
				kind, unit, currency, wantCharge = "sale_money", "micromoney", "CNY", 85
			}
			p, key := f.principal()
			out, err := f.gateway.ChatCompletions(ctx, p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			before := terminalFinalizerSnapshot(t, f, out.RequestID)
			delivery := terminalUsage(t, f, out.RequestID)
			delivery.Buckets = domain.UsageBuckets{
				InputTokens: int64Ptr(20), CacheReadTokens: int64Ptr(5), CacheWriteTokens: int64Ptr(2),
				OutputTokens: int64Ptr(8), ReasoningTokens: int64Ptr(3),
			}
			delivery.RawUsage.Raw = json.RawMessage(`{"schema_version":1,"usage":{"prompt_tokens":20,"completion_tokens":8,"audit_counter":9007199254740993}}`)
			f.replaySettlement(t, delivery)
			first := loadTerminalEvidence(t, f, out.RequestID, 1)
			for _, observation := range first {
				if observation.RequestID != out.RequestID || observation.AttemptID != delivery.AttemptID ||
					observation.Source != domain.UsageReported || observation.RawUsageSchemaVersion != 1 ||
					observation.Price.ID != priceID || observation.Price.ModelID != f.modelID ||
					observation.Price.Kind != kind || observation.Price.Revision != 1 || observation.Price.Currency != currency ||
					observation.Charge.Source != name || observation.Charge.Unit != unit ||
					observation.Charge.Currency != currency || observation.Charge.AmountMicros != wantCharge {
					t.Errorf("normalized evidence = %+v", observation)
				}
				assertTerminalBucket(t, observation.Buckets, "input_tokens", int64Ptr(15))
				assertTerminalBucket(t, observation.Buckets, "cache_read_tokens", int64Ptr(5))
				assertTerminalBucket(t, observation.Buckets, "cache_write_tokens", int64Ptr(2))
				assertTerminalBucket(t, observation.Buckets, "output_tokens", int64Ptr(8))
				assertTerminalBucket(t, observation.Buckets, "reasoning_tokens", int64Ptr(3))
				var raw struct {
					Usage struct {
						Counter int64 `json:"audit_counter"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(observation.RawUsage, &raw); err != nil || raw.Usage.Counter != 9007199254740993 {
					t.Errorf("raw usage lost integer precision: %s, %v", observation.RawUsage, err)
				}
			}
			// A retry's timestamp and JSON key order are not distinct evidence.
			delivery.RecordedAt = time.Now().Add(time.Hour)
			delivery.RawUsage.Raw = json.RawMessage(`{ "usage": { "audit_counter":9007199254740993,"completion_tokens":8,"prompt_tokens":20 }, "schema_version":1 }`)
			f.replaySettlement(t, delivery)
			if got := loadTerminalEvidence(t, f, out.RequestID, 1); !reflect.DeepEqual(got, first) {
				t.Error("exact evidence retry changed identity or content")
			}
			delivery.Source = domain.UsageEstimated
			f.replaySettlement(t, delivery)
			all := loadTerminalEvidence(t, f, out.RequestID, 2)
			for id, observation := range first {
				if !reflect.DeepEqual(all[id], observation) {
					t.Error("a distinct observation overwrote the earlier reported evidence")
				}
			}
			// Existing jobs may also be refreshed by workers without new evidence.
			tx, err := f.store.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := f.store.EnqueueReconciliationJobTx(ctx, tx, postgres.EnqueueReconciliationCommand{
				RequestID: &out.RequestID, Reason: postgres.ReasonUnknownUsage,
				Detail: json.RawMessage(`{"schema_version":1,"worker_audit":"kept"}`), Deadline: time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if got := loadTerminalEvidence(t, f, out.RequestID, 2); !reflect.DeepEqual(got, all) {
				t.Error("unrelated refresh changed evidence")
			}
			reason, status, found := f.reconciliationJob(t, out.RequestID)
			if !found || reason != "settlement_mismatch" || status != "pending" {
				t.Errorf("refresh downgraded manual anomaly: %t/%s/%s", found, reason, status)
			}
			f.replaySettlement(t, delivery)
			var audit string
			if err := f.db.Get(&audit, `SELECT detail->>'worker_audit' FROM inference_reconciliation_jobs WHERE request_id=$1`, out.RequestID); err != nil || audit != "kept" {
				t.Errorf("worker audit lost on evidence refresh: %q, %v", audit, err)
			}
			loadTerminalEvidence(t, f, out.RequestID, 2)
			if after := terminalFinalizerSnapshot(t, f, out.RequestID); after != before {
				t.Error("evidence retention changed terminal accounting, TPM or wallet facts")
			}
		})
	}
}

func TestGateway_TerminalEvidenceRetainsNullableAndMalformedUsage(t *testing.T) {
	for _, raw := range []struct {
		name   string
		value  json.RawMessage
		binary bool
	}{
		{"absent", nil, false},
		{"malformed", json.RawMessage(`{"schema_version":1,"usage":`), false},
		{"duplicate_keys", json.RawMessage(`{"schema_version":1,"usage":{"value":1,"value":2}}`), false},
		{"duplicate_escaped_keys", json.RawMessage(`{"schema_version":1,"usage":{"a":1,"\u0061":2}}`), false},
		{"nul_escape", json.RawMessage(`{"schema_version":1,"usage":{"value":"\u0000"}}`), false},
		{"numeric_overflow", json.RawMessage(`{"schema_version":1,"usage":{"value":1e1000000}}`), false},
		{"lone_surrogate", json.RawMessage(`{"schema_version":1,"usage":{"value":"\ud800"}}`), false},
		{"invalid_utf8", json.RawMessage("{\"schema_version\":1,\"usage\":{\"value\":\"\xff\"}}"), true},
		{"literal_nul", json.RawMessage("{\"schema_version\":1,\"usage\":\"\x00\"}"), true},
	} {
		t.Run(raw.name, func(t *testing.T) {
			f := newFixture(t, newUpstream(t, nonStreamUsage(10, 5)), withZeroPrice())
			// No procurement list: cmd.AttemptID is nil, but usage still identifies the attempt.
			if _, err := f.db.Exec(`DELETE FROM inference_price_versions WHERE kind='upstream_cost'`); err != nil {
				t.Fatal(err)
			}
			p, key := f.principal()
			out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			before := terminalFinalizerSnapshot(t, f, out.RequestID)
			delivery := terminalUsage(t, f, out.RequestID)
			delivery.Source = domain.UsageEstimated
			delivery.Buckets = domain.UsageBuckets{OutputTokens: int64Ptr(0), CacheWriteTokens: int64Ptr(0)}
			delivery.RawUsage.Raw = raw.value
			f.replaySettlement(t, delivery)
			for _, observation := range loadTerminalEvidence(t, f, out.RequestID, 1) {
				if observation.AttemptID != delivery.AttemptID || observation.Source != domain.UsageEstimated ||
					observation.Charge.AmountMicros != 0 || observation.Charge.Unit != "microcredit" {
					t.Errorf("zero-price evidence = %+v", observation)
				}
				assertTerminalBucket(t, observation.Buckets, "input_tokens", nil)
				assertTerminalBucket(t, observation.Buckets, "cache_read_tokens", nil)
				assertTerminalBucket(t, observation.Buckets, "cache_write_tokens", int64Ptr(0))
				assertTerminalBucket(t, observation.Buckets, "output_tokens", int64Ptr(0))
				assertTerminalBucket(t, observation.Buckets, "reasoning_tokens", nil)
				if len(observation.RawUsage) != 0 && string(observation.RawUsage) != "null" {
					t.Errorf("unexpected raw JSON = %s", observation.RawUsage)
				}
				if raw.binary {
					if observation.RawUsageText != nil || !bytes.Equal(observation.RawUsageBase64, raw.value) {
						t.Errorf("nontext raw evidence lost: %+v", observation)
					}
				} else if len(raw.value) > 0 {
					if observation.RawUsageText == nil || *observation.RawUsageText != string(raw.value) {
						t.Errorf("nonrepresentable raw evidence lost: %+v", observation)
					}
				} else if observation.RawUsageText != nil {
					t.Errorf("absent raw usage fabricated: %q", *observation.RawUsageText)
				}
			}
			f.replaySettlement(t, delivery)
			loadTerminalEvidence(t, f, out.RequestID, 1)
			if after := terminalFinalizerSnapshot(t, f, out.RequestID); after != before {
				t.Error("nullable/malformed evidence changed authoritative facts")
			}
		})
	}
}

func TestGateway_ReleasedEvidenceRetainsJSONBIncompatibleUsage(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1,"usage":{"value":"\u0000"}}`,
		`{"schema_version":1,"usage":{"value":1e1000000}}`,
		`{"schema_version":1,"usage":{"value":"\ud800"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFixture(t, newUpstream(t, nonStreamUsage(10, 5)))
			var attemptID string
			f.gateway.store = &nonstreamFinalizationStore{
				Store: f.store,
				beforeFinish: func(ctx context.Context, id string) {
					attemptID = id
					var requestID string
					if err := f.db.GetContext(ctx, &requestID,
						`SELECT request_id FROM inference_attempts WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
					tx, err := f.store.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(ctx)
					if err := f.store.Release(ctx, tx, requestID); err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
				},
			}
			p, key := f.principal()
			out, err := f.gateway.ChatCompletions(context.Background(), p, key,
				domain.ProtocolOpenAIChat, chatReq(false, "hi"))
			if err != nil {
				t.Fatal(err)
			}
			loadTerminalEvidence(t, f, out.RequestID, 1)
			before := terminalFinalizerSnapshot(t, f, out.RequestID)
			delivery := domain.UsageRecord{
				RequestID: out.RequestID, AttemptID: attemptID, Source: domain.UsageReported,
				Buckets:  domain.UsageBuckets{InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5)},
				RawUsage: domain.ExtensionConfig{SchemaVersion: 1, Raw: json.RawMessage(raw)},
			}
			for i := 0; i < 2; i++ {
				f.replaySettlement(t, delivery)
				observations := loadTerminalEvidence(t, f, out.RequestID, 2)
				found := false
				for _, observation := range observations {
					if observation.RawUsageText != nil && *observation.RawUsageText == raw {
						found = true
					}
				}
				if !found {
					t.Errorf("released raw evidence lost: %+v", observations)
				}
			}
			reason, status, found := f.reconciliationJob(t, out.RequestID)
			if !found || reason != "late_usage_after_release" || status != "pending" {
				t.Errorf("job = %t/%s/%s", found, reason, status)
			}
			if after := terminalFinalizerSnapshot(t, f, out.RequestID); after != before {
				t.Error("late evidence changed terminal accounting")
			}
		})
	}
}

func TestGateway_TerminalEvidenceConcurrentObservations(t *testing.T) {
	f := newFixture(t, newUpstream(t, nonStreamUsage(10, 5)))
	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	before := terminalFinalizerSnapshot(t, f, out.RequestID)
	original := terminalUsage(t, f, out.RequestID)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := int64(0); i < 4; i++ {
		wg.Add(1)
		go func(input int64) {
			defer wg.Done()
			<-start
			delivery := original
			delivery.Buckets.InputTokens = int64Ptr(input)
			f.replaySettlement(t, delivery)
		}(20 + i)
	}
	close(start)
	wg.Wait()
	seen := map[int64]bool{}
	for _, observation := range loadTerminalEvidence(t, f, out.RequestID, 4) {
		if value := observation.Buckets["input_tokens"]; value != nil {
			seen[*value] = true
		}
	}
	if !reflect.DeepEqual(seen, map[int64]bool{20: true, 21: true, 22: true, 23: true}) {
		t.Errorf("concurrent evidence lost: %v", seen)
	}
	if after := terminalFinalizerSnapshot(t, f, out.RequestID); after != before {
		t.Error("concurrent observations changed authoritative facts")
	}
}
