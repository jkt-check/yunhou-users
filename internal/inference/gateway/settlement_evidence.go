package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

type settlementEvidence struct {
	SchemaVersion int                        `json:"schema_version"`
	Observations  map[string]json.RawMessage `json:"observations"`
}

type settlementObservation struct {
	RequestID             string             `json:"request_id"`
	AttemptID             string             `json:"attempt_id"`
	Source                domain.UsageSource `json:"source"`
	Buckets               settlementBuckets  `json:"buckets"`
	RawUsageSchemaVersion int                `json:"raw_usage_schema_version"`
	RawUsage              json.RawMessage    `json:"raw_usage,omitempty"`
	RawUsageText          *string            `json:"raw_usage_text,omitempty"`
	Price                 settlementPrice    `json:"price"`
	Charge                settlementCharge   `json:"charge"`
}

type settlementBuckets struct {
	Input      *int64 `json:"input_tokens"`
	CacheRead  *int64 `json:"cache_read_tokens"`
	CacheWrite *int64 `json:"cache_write_tokens"`
	Output     *int64 `json:"output_tokens"`
	Reasoning  *int64 `json:"reasoning_tokens"`
}

type settlementPrice struct {
	ID       string               `json:"id"`
	ModelID  string               `json:"model_id"`
	Kind     accounting.PriceKind `json:"kind"`
	Revision int                  `json:"revision"`
	Currency string               `json:"currency"`
}

type settlementCharge struct {
	Source       domain.ChargeSource `json:"source"`
	AmountMicros int64               `json:"amount_micros"`
	Unit         string              `json:"unit"`
	Currency     string              `json:"currency"`
}

// The command is already normalized and priced. Observations are evidence,
// not authoritative usage revisions; delivery times must not defeat dedup.
func newSettlementEvidence(cmd domain.SettleCommand, price accounting.PriceVersion, source domain.ChargeSource) (settlementEvidence, error) {
	u := cmd.Usage
	observation := settlementObservation{
		RequestID: cmd.RequestID, AttemptID: u.AttemptID, Source: u.Source,
		Buckets: settlementBuckets{
			Input: u.Buckets.InputTokens, CacheRead: u.Buckets.CacheReadTokens,
			CacheWrite: u.Buckets.CacheWriteTokens, Output: u.Buckets.OutputTokens,
			Reasoning: u.Buckets.ReasoningTokens,
		},
		RawUsageSchemaVersion: u.RawUsage.SchemaVersion,
		Price: settlementPrice{
			ID: price.ID, ModelID: price.ModelID, Kind: price.Kind,
			Revision: price.Revision, Currency: price.Currency,
		},
		Charge: settlementCharge{
			Source: source, AmountMicros: int64(cmd.ChargeMicros), Unit: "microcredit",
		},
	}
	if source == domain.ChargeSourceWallet {
		observation.Charge.Unit = "micromoney"
		observation.Charge.Currency = price.Currency
	}
	if raw := u.RawUsage.Raw; len(raw) > 0 {
		if json.Valid(raw) {
			// Sort object keys without rounding counters through float64.
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				return settlementEvidence{}, err
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return settlementEvidence{}, err
			}
			observation.RawUsage = canonical
		} else {
			text := string(raw)
			observation.RawUsageText = &text
		}
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return settlementEvidence{}, err
	}
	id := sha256.Sum256(raw)
	return settlementEvidence{
		SchemaVersion: 1,
		Observations:  map[string]json.RawMessage{hex.EncodeToString(id[:]): raw},
	}, nil
}
