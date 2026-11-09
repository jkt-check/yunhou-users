package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

func TestChatFacade_ModelCapabilityRefusalBeforeAdmission(t *testing.T) {
	for _, capability := range []string{"tools", "thinking", "protocol"} {
		t.Run(capability, func(t *testing.T) {
			up := newChatStubUpstream(t)
			engine, db, store := buildChatFacadeE2E(t, up, nil, false, func(snap *catalog.Snapshot) {
				m := snap.Models["deepseek-chat"]
				switch capability {
				case "tools":
					m.SupportsTools = false
				case "thinking":
					m.SupportsReasoning = false
				case "protocol":
					m.Protocols = []domain.Protocol{domain.ProtocolAnthropicMessage}
				}
				snap.Models[m.ID] = m
			})
			login := loginAndGetTokens(t, engine, "chatcapability"+capability, "yundian")
			grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")
			body := map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}}
			switch capability {
			case "tools":
				body["tools"] = []map[string]any{{
					"type": "function", "function": map[string]any{
						"name": "lookup", "parameters": map[string]any{"type": "object"},
					},
				}}
			case "thinking":
				body["thinking_enabled"] = true
			}
			got := chatPost(t, engine, login.AccessToken, body)
			if got.Code != http.StatusBadRequest {
				t.Errorf("capability refusal: status=%d body=%s", got.Code, got.Body.String())
			}
			if !strings.Contains(got.Body.String(), `"code":400`) {
				t.Errorf("capability envelope=%s", got.Body.String())
			}
			for _, table := range []string{
				"inference_requests", "inference_attempts", "inference_reservations",
				"inference_rate_reservations", "inference_concurrency_leases", "inference_ledger_entries",
			} {
				var count int
				if err := db.Get(&count, "SELECT count(*) FROM "+table); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Errorf("%s rows=%d, want zero before admission", table, count)
				}
			}
			select {
			case sent := <-up.lastBody:
				t.Errorf("refused request reached upstream: %s", sent)
			default:
			}
		})
	}
}

func TestChatFacade_UpstreamInvalidInputRemainsStructuredRejection(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up, nil)
	login := loginAndGetTokens(t, engine, "chatupstreaminvalidinput", "yundian")
	grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")
	got := chatPost(t, engine, login.AccessToken, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": chatStubMarker400}},
	})
	if got.Code != http.StatusBadGateway ||
		!strings.Contains(got.Body.String(), `"upstream_status":400`) ||
		!strings.Contains(got.Body.String(), `"upstream_code":"context_length_exceeded"`) {
		t.Fatalf("upstream rejection: status=%d body=%s", got.Code, got.Body.String())
	}
}
