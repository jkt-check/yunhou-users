package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type envelope struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func do(t *testing.T, engine http.Handler, method, path string, body any, wantStatus int) envelope {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d (body %s)", method, path, w.Code, wantStatus, w.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: envelope decode: %v", method, path, err)
	}
	return env
}

func dataField(t *testing.T, env envelope, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(env.Data, &m); err != nil {
		t.Fatalf("data decode: %v (%s)", err, env.Data)
	}
	raw, ok := m[key]
	if !ok {
		t.Fatalf("data missing key %q: %s", key, env.Data)
	}
	return raw
}

// TestAdminCatalogFlow drives the whole Task 3 acceptance over HTTP: add a
// second same-protocol model, give it multiple deployments, publish, query
// — no restart anywhere in sight.
func TestAdminCatalogFlow(t *testing.T) {
	engine := newTestServer(t)

	// Provider.
	env := do(t, engine, http.MethodPost, "/admin/providers", map[string]any{
		"code": "openai", "display_name": "OpenAI", "access_type": "official_api",
	}, http.StatusOK)
	var providerID string
	if err := json.Unmarshal(dataField(t, env, "id"), &providerID); err != nil {
		t.Fatal(err)
	}

	// First model.
	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "glm-4.6", "display_name": "GLM 4.6",
		"context_tokens": 200000, "max_output_tokens": 8192,
		"protocols": []string{"openai_chat"},
	}, http.StatusOK)

	// Second model, SAME protocol — the acceptance scenario.
	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "deepseek-v4", "display_name": "DeepSeek V4",
		"context_tokens": 128000, "max_output_tokens": 4096,
		"protocols": []string{"openai_chat"},
	}, http.StatusOK)

	// Invalid model id → 400 envelope.
	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "BAD ID", "display_name": "x",
		"context_tokens": 1, "max_output_tokens": 1, "protocols": []string{"openai_chat"},
	}, http.StatusBadRequest)

	// Two deployments for the second model (模型可有多个部署).
	var depIDs []string
	for _, base := range []string{"https://primary.example.com/v1", "https://backup.example.com/v1"} {
		env := do(t, engine, http.MethodPost, "/admin/deployments", map[string]any{
			"provider_id": providerID, "upstream_model": "deepseek-chat",
			"base_url": base, "protocol": "openai_chat", "status": "active",
		}, http.StatusOK)
		var id string
		if err := json.Unmarshal(dataField(t, env, "id"), &id); err != nil {
			t.Fatal(err)
		}
		depIDs = append(depIDs, id)
	}
	for _, depID := range depIDs {
		do(t, engine, http.MethodPost, "/admin/models/deepseek-v4/routes", map[string]any{
			"deployment_id": depID, "weight": 1, "enabled": true,
		}, http.StatusOK)
	}

	// Activate the second model, publish, then query everything back.
	do(t, engine, http.MethodPost, "/admin/models/deepseek-v4/lifecycle", map[string]any{
		"lifecycle": "active",
	}, http.StatusOK)
	env = do(t, engine, http.MethodPost, "/admin/catalog/publish", nil, http.StatusOK)
	var rev int
	if err := json.Unmarshal(dataField(t, env, "revision"), &rev); err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Errorf("published revision = %d, want 1", rev)
	}

	// Read-only queries.
	env = do(t, engine, http.MethodGet, "/admin/models?limit=50", nil, http.StatusOK)
	var models []struct {
		ID        string `json:"id"`
		Lifecycle string `json:"lifecycle"`
	}
	if err := json.Unmarshal(dataField(t, env, "models"), &models); err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Errorf("models = %+v, want 2", models)
	}

	env = do(t, engine, http.MethodGet, "/admin/models/deepseek-v4/routes", nil, http.StatusOK)
	var routes []map[string]any
	if err := json.Unmarshal(dataField(t, env, "routes"), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Errorf("routes = %d, want 2 (multiple deployments)", len(routes))
	}

	env = do(t, engine, http.MethodGet, "/admin/deployments?provider_id="+providerID, nil, http.StatusOK)
	var deployments []map[string]any
	if err := json.Unmarshal(dataField(t, env, "deployments"), &deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 2 {
		t.Errorf("deployments = %d, want 2", len(deployments))
	}

	do(t, engine, http.MethodGet, "/admin/catalog/active", nil, http.StatusOK)
	do(t, engine, http.MethodGet, "/admin/catalog/revisions", nil, http.StatusOK)

	// Unknown model → 404.
	do(t, engine, http.MethodGet, "/admin/models/nope", nil, http.StatusNotFound)
}

func TestAdminModelOptimisticConflictOverHTTP(t *testing.T) {
	engine := newTestServer(t)
	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "m-1", "display_name": "M",
		"context_tokens": 10, "max_output_tokens": 10, "protocols": []string{"openai_chat"},
	}, http.StatusOK)

	env := do(t, engine, http.MethodGet, "/admin/models/m-1", nil, http.StatusOK)
	var fetched struct {
		DisplayName string `json:"display_name"`
		UpdatedAt   string `json:"updated_at"`
	}
	if err := json.Unmarshal(env.Data, &fetched); err != nil {
		t.Fatal(err)
	}

	// Edit A wins.
	do(t, engine, http.MethodPatch, "/admin/models/m-1", map[string]any{
		"display_name": "A", "context_tokens": 10, "max_output_tokens": 10,
		"protocols": []string{"openai_chat"}, "updated_at": fetched.UpdatedAt,
	}, http.StatusOK)

	// Edit B reuses the stale token → 409 conflict, not silent overwrite.
	env = do(t, engine, http.MethodPatch, "/admin/models/m-1", map[string]any{
		"display_name": "B", "context_tokens": 10, "max_output_tokens": 10,
		"protocols": []string{"openai_chat"}, "updated_at": fetched.UpdatedAt,
	}, http.StatusConflict)
	if env.Message == "" {
		t.Error("conflict response must carry a message")
	}

	// Missing version token → 400.
	do(t, engine, http.MethodPatch, "/admin/models/m-1", map[string]any{
		"display_name": "C",
	}, http.StatusBadRequest)
}

func TestAdminPublishValidateAndRollbackOverHTTP(t *testing.T) {
	engine := newTestServer(t)

	// Publish on an empty catalog is fine (empty snapshot).
	do(t, engine, http.MethodPost, "/admin/catalog/publish", nil, http.StatusOK)

	// A structurally broken catalog is refused at publish time: route
	// pointing at a deleted... simpler: unknown reference via route create
	// is already 400; here verify the rollback endpoint shape.
	env := do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "r-1", "display_name": "R",
		"context_tokens": 10, "max_output_tokens": 10, "protocols": []string{"openai_chat"},
	}, http.StatusOK)
	var updatedAt string
	if err := json.Unmarshal(mustField(env.Data, "updated_at"), &updatedAt); err != nil {
		t.Fatal(err)
	}
	do(t, engine, http.MethodPatch, "/admin/models/r-1", map[string]any{
		"display_name": "R2", "context_tokens": 10, "max_output_tokens": 10,
		"protocols": []string{"openai_chat"}, "updated_at": updatedAt,
	}, http.StatusOK)
	do(t, engine, http.MethodPost, "/admin/catalog/publish", nil, http.StatusOK)

	// Roll back to revision 1 → new revision 3 with old content.
	env = do(t, engine, http.MethodPost, "/admin/catalog/rollback", map[string]any{
		"to_revision": 1,
	}, http.StatusOK)
	var rev int
	if err := json.Unmarshal(dataField(t, env, "revision"), &rev); err != nil {
		t.Fatal(err)
	}
	if rev != 3 {
		t.Errorf("rollback revision = %d, want 3", rev)
	}

	// Revision 2 was published, so rolling back to it is allowed (content
	// equals revision 2 under a new revision number).
	do(t, engine, http.MethodPost, "/admin/catalog/rollback", map[string]any{
		"to_revision": 2,
	}, http.StatusOK)
}

func mustField(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	return m[key]
}

// TestAdminDeploymentConfigHeadersRejected: the extension config may carry
// operator-supplied request headers (Task 8 introduces the convention), but
// they must never override the gateway's authentication/tracing boundary —
// deployment writes (operator API path; env import funnels through the same
// ValidateDeployment) reject blacklisted headers and malformed payloads.
func TestAdminDeploymentConfigHeadersRejected(t *testing.T) {
	engine := newTestServer(t)

	env := do(t, engine, http.MethodPost, "/admin/providers", map[string]any{
		"code": "hdr-check", "display_name": "HDR", "access_type": "official_api",
	}, http.StatusOK)
	var providerID string
	if err := json.Unmarshal(dataField(t, env, "id"), &providerID); err != nil {
		t.Fatal(err)
	}

	base := map[string]any{
		"provider_id": providerID, "upstream_model": "m", "base_url": "https://api.example.com/v1",
		"protocol": "openai_chat",
	}

	// Gateway-managed headers are refused however they are cased.
	for _, h := range []string{"Authorization", "authorization", "X-Api-Key", "Host", "X-Request-Id"} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		body["config"] = map[string]any{"schema_version": 1, "headers": map[string]string{h: "evil"}}
		do(t, engine, http.MethodPost, "/admin/deployments", body, http.StatusBadRequest)
	}

	// Malformed headers payload (not a string→string object) fails closed.
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	body["config"] = map[string]any{"schema_version": 1, "headers": []string{"Authorization"}}
	do(t, engine, http.MethodPost, "/admin/deployments", body, http.StatusBadRequest)

	// Benign custom headers are accepted — the forward-compatible path for
	// Task 8 protocol headers (blacklist only, not absence).
	body = map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	body["config"] = map[string]any{"schema_version": 1, "headers": map[string]string{"X-Tenant": "acme"}}
	do(t, engine, http.MethodPost, "/admin/deployments", body, http.StatusOK)
}
