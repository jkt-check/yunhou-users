package gateway

import (
	"encoding/json"
	"testing"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

func TestSettlementEvidence_RawIdentityPreservesUnicodeAndNumbers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		canonical  string
		equivalent string
		distinct   string
	}{
		{
			"surrogate_value",
			`{"schema_version":1,"usage":{"b":"\ud800","a":1e1000000}}`,
			`{"schema_version":1,"usage":{"a":1e1000000,"b":"\ud800"}}`,
			`{ "usage": { "a":1e1000000, "b":"\ud800" }, "schema_version":1 }`,
			`{"schema_version":1,"usage":{"b":"\ud801","a":1e1000000}}`,
		},
		{
			"surrogate_key",
			`{"schema_version":1,"usage":{"\ud800":1,"nested":[{"b":9007199254740993,"a":"x"}]}}`,
			`{"schema_version":1,"usage":{"\ud800":1,"nested":[{"a":"x","b":9007199254740993}]}}`,
			`{"usage":{"nested":[{"a":"x","b":9007199254740993}],"\ud800":1},"schema_version":1}`,
			`{"schema_version":1,"usage":{"\ud801":1,"nested":[{"b":9007199254740993,"a":"x"}]}}`,
		},
		{
			"escaped_key",
			`{"schema_version":1,"usage":{"b,\"a":null,"nested":[true,false,{"b":2,"a":1}]}}`,
			`{"schema_version":1,"usage":{"b,\"a":null,"nested":[true,false,{"a":1,"b":2}]}}`,
			`{"usage":{"nested":[true,false,{"a":1,"b":2}],"b,\"a":null},"schema_version":1}`,
			`{"schema_version":1,"usage":{"b,\"a":null,"nested":[false,true,{"b":2,"a":1}]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := func(raw string) (string, terminalObservation) {
				t.Helper()
				cmd := domain.SettleCommand{Usage: domain.UsageRecord{
					RawUsage: domain.ExtensionConfig{SchemaVersion: 1, Raw: json.RawMessage(raw)},
				}}
				evidence, err := newSettlementEvidence(cmd, accounting.PriceVersion{}, "")
				if err != nil {
					t.Fatal(err)
				}
				for id, value := range evidence.Observations {
					var observation terminalObservation
					if err := json.Unmarshal(value, &observation); err != nil {
						t.Fatal(err)
					}
					return id, observation
				}
				t.Fatal("missing observation")
				return "", terminalObservation{}
			}
			id, observation := build(tc.raw)
			if string(observation.RawUsage) != tc.canonical {
				t.Errorf("raw evidence lost content: got %s, want %s", observation.RawUsage, tc.canonical)
			}
			if equivalentID, _ := build(tc.equivalent); equivalentID != id {
				t.Error("object ordering or whitespace changed evidence identity")
			}
			if distinctID, _ := build(tc.distinct); distinctID == id {
				t.Error("distinct raw content collapsed into the same observation")
			}
		})
	}
}

func TestSettlementEvidence_DuplicateKeysRetainRawText(t *testing.T) {
	var previous string
	for _, raw := range []string{
		`{"usage":{"a":1,"\u0061":2}}`,
		`{"usage":{"\u0061":2,"a":1}}`,
		`{"usage":{"a":1,"a":2}}`,
		`{"usage":{"a":9,"a":2}}`,
	} {
		cmd := domain.SettleCommand{Usage: domain.UsageRecord{
			RawUsage: domain.ExtensionConfig{SchemaVersion: 1, Raw: json.RawMessage(raw)},
		}}
		evidence, err := newSettlementEvidence(cmd, accounting.PriceVersion{}, "")
		if err != nil {
			t.Fatal(err)
		}
		for id, value := range evidence.Observations {
			var observation terminalObservation
			if err := json.Unmarshal(value, &observation); err != nil {
				t.Fatal(err)
			}
			if observation.RawUsageText == nil || *observation.RawUsageText != raw {
				t.Errorf("duplicate-key evidence lost: %s", value)
			}
			if id == previous {
				t.Error("different duplicate-key evidence collapsed")
			}
			previous = id
		}
	}
}
