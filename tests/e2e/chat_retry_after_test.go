package e2e

import (
	"net/http"
	"strconv"
	"testing"
)

func TestChatFacade_RateRetryAfter(t *testing.T) {
	up := newChatStubUpstream(t)
	engine, db, store := setupChatFacadeE2E(t, up, nil)
	login := loginAndGetTokens(t, engine, "chatretryafter", "yundian")
	grantKayaModelEntitlement(t, db, store, login.User.ID, "deepseek-chat")
	if _, err := db.Exec(`UPDATE inference_policy_versions SET rpm_limit = 1 WHERE name = 'kaya-gift'`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}}
	first := chatPost(t, engine, login.AccessToken, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", first.Code, first.Body.String())
	}
	select {
	case <-up.lastBody:
	default:
		t.Fatal("first request did not reach upstream")
	}

	blocked := chatPost(t, engine, login.AccessToken, body)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("RPM rejection: %d %s", blocked.Code, blocked.Body.String())
	}
	seconds, err := strconv.Atoi(blocked.Header().Get("Retry-After"))
	if err != nil || seconds < 1 || seconds > 60 {
		t.Errorf("RPM Retry-After=%q, want 1..60", blocked.Header().Get("Retry-After"))
	}
	const wantBody = `{"code":429,"data":null,"message":"chat request rejected by a usage limit (rate_limited)"}`
	if blocked.Body.String() != wantBody {
		t.Errorf("RPM envelope=%s, want %s", blocked.Body.String(), wantBody)
	}

	// A request larger than the TPM cap cannot recover by waiting.
	if _, err := db.Exec(`UPDATE inference_policy_versions SET rpm_limit = NULL, tpm_limit = 1 WHERE name = 'kaya-gift'`); err != nil {
		t.Fatal(err)
	}
	unknown := chatPost(t, engine, login.AccessToken, body)
	if unknown.Code != http.StatusTooManyRequests || unknown.Header().Get("Retry-After") != "" {
		t.Errorf("unknown recovery: status=%d retry=%q body=%s",
			unknown.Code, unknown.Header().Get("Retry-After"), unknown.Body.String())
	}
	if unknown.Body.String() != wantBody {
		t.Errorf("TPM envelope=%s, want %s", unknown.Body.String(), wantBody)
	}
	select {
	case sent := <-up.lastBody:
		t.Errorf("rejected request reached upstream: %s", sent)
	default:
	}
}
