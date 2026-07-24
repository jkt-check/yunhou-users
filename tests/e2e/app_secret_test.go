package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yunhou/users/internal/util"
)

// TestE2E_AppSecret_Auth verifies that InternalAppAuth rejects requests
// missing or carrying a wrong X-App-Secret, and accepts the matching one.
// Also covers RotateSecret: the new plaintext must work, the old plaintext
// must stop working after rotation.
func TestE2E_AppSecret_Auth(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	hdrs := appAuthHeaders(superAppID)

	// Missing X-App-Secret → 401 (even though X-App-ID is valid).
	t.Run("missing X-App-Secret", func(t *testing.T) {
		resp := doRequest(t, engine, http.MethodGet, "/apps", "", map[string]string{"X-App-ID": superAppID})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401; body = %s", resp.StatusCode, string(resp.Body))
		}
	})

	// Wrong X-App-Secret → 401.
	t.Run("wrong X-App-Secret", func(t *testing.T) {
		hdr := map[string]string{"X-App-ID": superAppID, "X-App-Secret": "not-the-real-secret"}
		resp := doRequest(t, engine, http.MethodGet, "/apps", "", hdr)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401; body = %s", resp.StatusCode, string(resp.Body))
		}
	})

	// Correct X-App-Secret → 200.
	t.Run("correct X-App-Secret", func(t *testing.T) {
		resp := doRequest(t, engine, http.MethodGet, "/apps", "", hdrs)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200; body = %s", resp.StatusCode, string(resp.Body))
		}
	})

	// RotateSecret: returns a fresh plaintext, the old plaintext no longer
	// authenticates, the new one does.
	t.Run("rotate-secret invalidates old", func(t *testing.T) {
		oldSecret := e2eAppSecret
		resp := doRequest(t, engine, http.MethodPost, "/admin/apps/"+superAppID+"/rotate-secret", "", hdrs)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rotate-secret: %d %s", resp.StatusCode, string(resp.Body))
		}
		var body struct {
			Code int `json:"code"`
			Data struct {
				Secret string `json:"secret"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if body.Data.Secret == "" || body.Data.Secret == oldSecret {
			t.Fatalf("rotate-secret returned %q (must be non-empty and different from old)", body.Data.Secret)
		}

		// New secret works.
		newHdrs := map[string]string{"X-App-ID": superAppID, "X-App-Secret": body.Data.Secret}
		resp = doRequest(t, engine, http.MethodGet, "/apps", "", newHdrs)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("after rotate, new secret: %d, want 200; body = %s", resp.StatusCode, string(resp.Body))
		}

		// Old secret rejected.
		oldHdrs := map[string]string{"X-App-ID": superAppID, "X-App-Secret": oldSecret}
		resp = doRequest(t, engine, http.MethodGet, "/apps", "", oldHdrs)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("after rotate, old secret: %d, want 401; body = %s", resp.StatusCode, string(resp.Body))
		}
	})
}

// TestE2E_CreateApp_ReturnsSecret asserts that POST /admin/apps returns a
// plaintext secret in the response, and that the bcrypt hash stored on disk
// verifies against it.
func TestE2E_CreateApp_ReturnsSecret(t *testing.T) {
	engine, _, db := setupE2EServer(t)
	hdrs := appAuthHeaders(superAppID)

	appID := "e2e-created-" + randomSuffix()
	body := `{"app_id":"` + appID + `","name":"Created E2E"}`
	resp := doRequest(t, engine, http.MethodPost, "/admin/apps", body, hdrs)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: %d %s", resp.StatusCode, string(resp.Body))
	}
	var parsed struct {
		Code int `json:"code"`
		Data struct {
			App    map[string]any `json:"app"`
			Secret string         `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Data.Secret == "" {
		t.Fatal("data.secret must be populated on create")
	}
	if _, ok := parsed.Data.App["secret_hash"]; ok {
		t.Errorf("app.secret_hash must NOT appear in the create response; got %v", parsed.Data.App)
	}

	// Hash stored in DB must verify against the returned plaintext.
	var hash string
	if err := db.QueryRow(`SELECT secret_hash FROM apps WHERE app_id = $1`, appID).Scan(&hash); err != nil {
		t.Fatalf("read secret_hash: %v", err)
	}
	if !util.CheckSecret(hash, parsed.Data.Secret) {
		t.Errorf("stored secret_hash does not verify against returned plaintext")
	}
	// Negative path: a wrong plaintext must not verify.
	if util.CheckSecret(hash, "wrong") {
		t.Errorf("stored secret_hash verifies against an unrelated plaintext — bcrypt mismatch suggests a bug")
	}
}