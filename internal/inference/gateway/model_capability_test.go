package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

func TestGateway_ModelCapabilityRefusalBeforeAdmission(t *testing.T) {
	for _, capability := range []string{"tools", "thinking", "effort", "images", "protocol"} {
		t.Run(capability, func(t *testing.T) {
			up := newUpstream(t, completionHandler)
			f := newFixture(t, up)
			m := f.snap.Models[f.modelID]
			req := chatReq(false, "hi")
			switch capability {
			case "tools":
				m.SupportsTools = false
				req.Tools = []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`)}
			case "thinking":
				m.SupportsReasoning = false
				on := true
				req.ThinkingEnabled = &on
			case "effort":
				m.SupportsReasoning = false
				req.ReasoningEffort = "high"
			case "images":
				m.InputModalities = []string{"text"}
				req = imageRequest(f.modelID)
			case "protocol":
				m.Protocols = []domain.Protocol{domain.ProtocolAnthropicMessage}
			}
			f.snap.Models[f.modelID] = m
			p, key := f.principal()
			_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolKayaChat, req)
			var capabilityErr *ErrCapabilityUnsupported
			if !errors.As(err, &capabilityErr) || domain.CodeOf(err) != domain.CodeInvalidInput {
				t.Errorf("error=%v, want typed capability invalid_input", err)
			}
			for _, table := range []string{
				"inference_requests", "inference_attempts", "inference_reservations",
				"inference_rate_reservations", "inference_concurrency_leases", "inference_ledger_entries",
			} {
				if n := countRows(t, f, table); n != 0 {
					t.Errorf("%s rows=%d, want zero before admission", table, n)
				}
			}
			if n := up.calls.Load(); n != 0 {
				t.Errorf("upstream calls=%d, want zero", n)
			}
		})
	}
}
