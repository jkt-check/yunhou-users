package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
)

// updateAppConfig drives one PATCH /apps/:id config call against a bare
// handler mounted with withCallerApp, returning the status and whatever
// config the repo holds afterwards.
func updateAppConfig(t *testing.T, appRepo *mockAppRepo, appID, body string) (int, json.RawMessage) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewAppHandler(appRepo, nil, nil)
	router := gin.New()
	router.PATCH("/apps/:id", withCallerApp(appID), h.UpdateApp)

	req := httptest.NewRequest(http.MethodPatch, "/apps/"+appID, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	stored, err := appRepo.FindByID(context.Background(), appID)
	if err != nil {
		t.Fatalf("find after update: %v", err)
	}
	return w.Code, stored.Config
}

func storedConfigField(t *testing.T, cfg json.RawMessage, path ...string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		t.Fatalf("stored config not JSON: %v (%s)", err, cfg)
	}
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %v is not an object", path, cur)
		}
		cur = mm[p]
	}
	return cur
}

// TestAppHandler_UpdateApp_PreservesMaskedSecret (audit I-9): the GET
// masking shows "***" for secret config values; a read-modify-write client
// that PATCHes the masked document back must NOT overwrite the stored
// secret with the sentinel.
func TestAppHandler_UpdateApp_PreservesMaskedSecret(t *testing.T) {
	const realSecret = "real-paypal-client-secret-abc123"
	appRepo := &mockAppRepo{apps: []model.App{{
		AppID:  "site",
		Name:   "Site",
		Config: json.RawMessage(`{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"` + realSecret + `","webhook_id":"w","mode":"live"}}}`),
	}}}

	// The masked round-trip: exactly what a GET → PATCH-back client sends.
	body := `{"config":{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"***","webhook_id":"w","mode":"live"}}}}`
	code, stored := updateAppConfig(t, appRepo, "site", body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body", code)
	}
	if got := storedConfigField(t, stored, "payment_providers", "paypal", "client_secret"); got != realSecret {
		t.Errorf("stored client_secret = %v, want the original secret preserved (sentinel must not be persisted)", got)
	}

	// Same preservation for the other three masked-secret fields.
	appRepo = &mockAppRepo{apps: []model.App{{
		AppID:  "site",
		Name:   "Site",
		Config: json.RawMessage(`{"payment_providers":{"wechat_pay":{"api_v3_key":"wechatpay-real-key-32bytes-pad!!"}},"oauth_providers":{"github":{"client_id":"g","client_secret":"gh-real-secret","callback_urls":["https://x.example/cb"]},"wechat":{"app_id":"wx0123456789abcdef","app_secret":"wx-real-secret-32byte-pad-000001","callback_urls":["https://x.example/cb"]}}}`),
	}}}
	body = `{"config":{"payment_providers":{"wechat_pay":{"api_v3_key":"***"}},"oauth_providers":{"github":{"client_id":"g","client_secret":"***","callback_urls":["https://x.example/cb"]},"wechat":{"app_id":"wx0123456789abcdef","app_secret":"***","callback_urls":["https://x.example/cb"]}}}}`
	code, stored = updateAppConfig(t, appRepo, "site", body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := storedConfigField(t, stored, "payment_providers", "wechat_pay", "api_v3_key"); got != "wechatpay-real-key-32bytes-pad!!" {
		t.Errorf("stored api_v3_key = %v, want preserved", got)
	}
	if got := storedConfigField(t, stored, "oauth_providers", "github", "client_secret"); got != "gh-real-secret" {
		t.Errorf("stored github client_secret = %v, want preserved", got)
	}
	if got := storedConfigField(t, stored, "oauth_providers", "wechat", "app_secret"); got != "wx-real-secret-32byte-pad-000001" {
		t.Errorf("stored wechat app_secret = %v, want preserved", got)
	}
}

// TestAppHandler_UpdateApp_NewSecretStillApplied: a genuinely new secret
// value replaces the stored one (preservation must only trigger on the
// exact sentinel).
func TestAppHandler_UpdateApp_NewSecretStillApplied(t *testing.T) {
	appRepo := &mockAppRepo{apps: []model.App{{
		AppID:  "site",
		Name:   "Site",
		Config: json.RawMessage(`{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"old-secret","webhook_id":"w","mode":"live"}}}`),
	}}}

	body := `{"config":{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"genuinely-new-secret","webhook_id":"w","mode":"live"}}}}`
	code, stored := updateAppConfig(t, appRepo, "site", body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := storedConfigField(t, stored, "payment_providers", "paypal", "client_secret"); got != "genuinely-new-secret" {
		t.Errorf("stored client_secret = %v, want the new secret applied", got)
	}
}

// TestAppHandler_UpdateApp_NonSecretSentinelWrittenAsIs: the preservation
// applies ONLY to the four masked-secret keys; a non-secret key set to the
// literal "***" is stored verbatim.
func TestAppHandler_UpdateApp_NonSecretSentinelWrittenAsIs(t *testing.T) {
	appRepo := &mockAppRepo{apps: []model.App{{
		AppID:  "site",
		Name:   "Site",
		Config: json.RawMessage(`{"brand":{"name":"Site"}}`),
	}}}

	body := `{"config":{"brand":{"name":"***"}}}`
	code, stored := updateAppConfig(t, appRepo, "site", body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := storedConfigField(t, stored, "brand", "name"); got != "***" {
		t.Errorf("stored brand.name = %v, want literal *** written as-is", got)
	}
}

// TestAppHandler_UpdateApp_SentinelWithoutStoredValueRejected: a masked
// sentinel for a secret that has no stored value can never be right —
// persist nothing, demand the real value.
func TestAppHandler_UpdateApp_SentinelWithoutStoredValueRejected(t *testing.T) {
	appRepo := &mockAppRepo{apps: []model.App{{
		AppID:  "site",
		Name:   "Site",
		Config: json.RawMessage(`{"brand":{"name":"Site"}}`),
	}}}

	body := `{"config":{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"***","webhook_id":"w","mode":"live"}}}}`
	code, stored := updateAppConfig(t, appRepo, "site", body)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if bytes.Contains(stored, []byte("paypal")) {
		t.Errorf("rejected PATCH must not persist config; stored = %s", stored)
	}
}

// TestAppHandler_CreateApp_RejectsMaskedSentinel: on create there is no
// stored value to preserve — a literal "***" secret would be persisted as
// the app's real (broken) provider credential.
func TestAppHandler_CreateApp_RejectsMaskedSentinel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &mockAppRepo{}
	h := NewAppHandler(appRepo, nil, nil)
	router := gin.New()
	router.POST("/apps", withCallerApp("creator"), h.CreateApp)

	body := `{"app_id":"site","name":"Site","config":{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"***","webhook_id":"w","mode":"live"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if len(appRepo.apps) != 0 {
		t.Errorf("rejected create must not persist an app; got %d", len(appRepo.apps))
	}
}
