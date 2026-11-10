package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"unicode/utf8"

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
	RawUsageBase64        []byte             `json:"raw_usage_base64,omitempty"`
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
	// AdmissionResult's zero value denotes the plan/quota path.
	if source == "" {
		source = domain.ChargeSourcePlan
	}
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
	var textOnly bool
	if raw := u.RawUsage.Raw; len(raw) > 0 {
		if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
			observation.RawUsageBase64 = raw
		} else if json.Valid(raw) {
			canonical, duplicateKeys, err := canonicalUsageJSON(raw)
			if err != nil {
				return settlementEvidence{}, err
			}
			observation.RawUsage = canonical
			textOnly = duplicateKeys
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
	evidence := settlementEvidence{
		SchemaVersion: 1,
		Observations:  map[string]json.RawMessage{hex.EncodeToString(id[:]): raw},
	}
	if textOnly {
		return evidence.withTextUsage(u.RawUsage.Raw)
	}
	return evidence, nil
}

// PostgreSQL may reject otherwise valid JSON. Change only its storage
// representation, never the content identity calculated before that rejection.
func (e settlementEvidence) withTextUsage(raw []byte) (settlementEvidence, error) {
	out := settlementEvidence{SchemaVersion: e.SchemaVersion, Observations: make(map[string]json.RawMessage, len(e.Observations))}
	for id, value := range e.Observations {
		var observation settlementObservation
		if err := json.Unmarshal(value, &observation); err != nil {
			return settlementEvidence{}, err
		}
		observation.RawUsage = nil
		if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
			observation.RawUsageText = nil
			observation.RawUsageBase64 = raw
		} else {
			text := string(raw)
			observation.RawUsageText = &text
			observation.RawUsageBase64 = nil
		}
		encoded, err := json.Marshal(observation)
		if err != nil {
			return settlementEvidence{}, err
		}
		out.Observations[id] = encoded
	}
	return out, nil
}

// Decode container boundaries only: decoding string/number values would
// replace lone surrogates or lose numeric lexemes. Duplicate keys retain
// their order and require text storage, since jsonb keeps only the last one.
func canonicalUsageJSON(raw json.RawMessage) (json.RawMessage, bool, error) {
	raw = bytes.TrimSpace(raw)
	switch raw[0] {
	case '{':
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if _, err := decoder.Token(); err != nil {
			return nil, false, err
		}
		type member struct {
			key   []byte
			value json.RawMessage
		}
		var members []member
		seen := map[string]bool{}
		duplicate, textOnly := false, false
		for decoder.More() {
			start := decoder.InputOffset()
			key, err := decoder.Token()
			if err != nil {
				return nil, false, err
			}
			rawKey := bytes.TrimSpace(raw[start:decoder.InputOffset()])
			if rawKey[0] == ',' {
				rawKey = bytes.TrimSpace(rawKey[1:])
			}
			name := key.(string)
			duplicate = duplicate || seen[name]
			seen[name] = true
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return nil, false, err
			}
			value, nestedText, err := canonicalUsageJSON(value)
			if err != nil {
				return nil, false, err
			}
			textOnly = textOnly || nestedText
			members = append(members, member{rawKey, value})
		}
		if _, err := decoder.Token(); err != nil {
			return nil, false, err
		}
		if !duplicate {
			sort.Slice(members, func(i, j int) bool { return bytes.Compare(members[i].key, members[j].key) < 0 })
		}
		var out bytes.Buffer
		out.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(m.key)
			out.WriteByte(':')
			out.Write(m.value)
		}
		out.WriteByte('}')
		return out.Bytes(), duplicate || textOnly, nil
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, false, err
		}
		var textOnly bool
		for i, value := range values {
			canonical, nestedText, err := canonicalUsageJSON(value)
			if err != nil {
				return nil, false, err
			}
			values[i] = canonical
			textOnly = textOnly || nestedText
		}
		encoded, err := json.Marshal(values)
		return encoded, textOnly, err
	default:
		return raw, false, nil
	}
}
