package e2e

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/model"
)

func TestE2E_ProviderToken_LemonSqueezy(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appID := "e2e-ls-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"e2e","config":{"payment_providers":{"lemonsqueezy":{"api_key":"lsq_e2e_key","store_id":"12345"}}}}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, map[string]string{"X-App-ID": "yundian"})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}

	resp := doRequest(t, engine, http.MethodGet,
		"/apps/"+appID+"/provider-token/lemonsqueezy",
		"", map[string]string{"X-App-ID": "yundian"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get provider token: %d %s", resp.StatusCode, string(resp.Body))
	}
	var body struct {
		Code int                  `json:"code"`
		Data *model.ProviderToken `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Data == nil || body.Data.APIKey != "lsq_e2e_key" {
		t.Errorf("data = %+v", body.Data)
	}
}

func TestE2E_ProviderToken_UnsupportedChannel(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appID := "e2e-unsup-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"e2e"}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, map[string]string{"X-App-ID": "yundian"})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}

	resp := doRequest(t, engine, http.MethodGet,
		"/apps/"+appID+"/provider-token/stripe",
		"", map[string]string{"X-App-ID": "yundian"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", resp.StatusCode, string(resp.Body))
	}
}

func TestE2E_ProviderToken_MissingConfig(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appID := "e2e-noconf-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"e2e"}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, map[string]string{"X-App-ID": "yundian"})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}

	resp := doRequest(t, engine, http.MethodGet,
		"/apps/"+appID+"/provider-token/paypal",
		"", map[string]string{"X-App-ID": "yundian"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", resp.StatusCode, string(resp.Body))
	}
}

// randomSuffix returns a short hex string safe for use in app_id.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err) // tests don't recover from this
	}
	return hexEncode(b)
}

func hexEncode(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = hexChars[x>>4]
		out[i*2+1] = hexChars[x&0xf]
	}
	return string(out)
}