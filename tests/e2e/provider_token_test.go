package e2e

import (
	"crypto/rand"
	"io"
	"net/http"
	"testing"
)

func TestE2E_ProviderToken_UnsupportedChannel(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appID := "e2e-unsup-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"e2e"}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, appAuthHeaders(superAppID))
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}

	resp := doRequest(t, engine, http.MethodGet,
		"/apps/"+appID+"/provider-token/stripe",
		"", appAuthHeaders(superAppID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", resp.StatusCode, string(resp.Body))
	}
}

func TestE2E_ProviderToken_MissingConfig(t *testing.T) {
	engine, _, _ := setupE2EServer(t)

	appID := "e2e-noconf-" + randomSuffix()
	createBody := `{"app_id":"` + appID + `","name":"e2e"}`
	createResp := doRequest(t, engine, http.MethodPost, "/admin/apps", createBody, appAuthHeaders(superAppID))
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", createResp.StatusCode, string(createResp.Body))
	}

	resp := doRequest(t, engine, http.MethodGet,
		"/apps/"+appID+"/provider-token/paypal",
		"", appAuthHeaders(superAppID))
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
