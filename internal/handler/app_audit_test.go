package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/util"
)

// fakeAuditWriter captures audit_log entries the handler would insert.
type fakeAuditWriter struct {
	entries []*model.AuditLog
	err     error
}

func (f *fakeAuditWriter) Insert(_ context.Context, a *model.AuditLog) error {
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, a)
	return nil
}

// TestAppHandler_CreateApp_WritesAudit (audit I-3): a successful CreateApp
// must land an app.create audit row attributed to the calling app.
func TestAppHandler_CreateApp_WritesAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &mockAppRepo{}
	audit := &fakeAuditWriter{}
	h := NewAppHandler(appRepo, nil, audit)

	router := gin.New()
	router.POST("/apps", withCallerApp("creator"), h.CreateApp)

	body := `{"app_id":"newapp","name":"New App","description":"d"}`
	req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "app.create" {
		t.Errorf("action = %q, want app.create", e.Action)
	}
	if e.Actor != "admin:creator" {
		t.Errorf("actor = %q, want admin:creator", e.Actor)
	}
	if e.Target == nil || *e.Target != "app:newapp" {
		t.Errorf("target = %v, want app:newapp", e.Target)
	}
	var ctx map[string]any
	if err := json.Unmarshal(e.Context, &ctx); err != nil {
		t.Fatalf("audit context not JSON: %v", err)
	}
	if ctx["name"] != "New App" {
		t.Errorf("context name = %v, want New App", ctx["name"])
	}

	// A rejected create must not audit anything.
	audit.entries = nil
	req = httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(`invalid`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if len(audit.entries) != 0 {
		t.Errorf("failed create wrote %d audit rows, want 0", len(audit.entries))
	}
}

// TestAppHandler_UpdateApp_WritesAuditNoSecretMaterial (audit I-3): the
// audit row records WHICH fields changed but never the config values —
// apps.config holds live provider credentials.
func TestAppHandler_UpdateApp_WritesAuditNoSecretMaterial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	existing := []model.App{{AppID: "site", Name: "Site"}}
	appRepo := &mockAppRepo{apps: existing}
	audit := &fakeAuditWriter{}
	h := NewAppHandler(appRepo, nil, audit)

	router := gin.New()
	router.PATCH("/apps/:id", withCallerApp("site"), h.UpdateApp)

	const newSecret = "brand-new-provider-secret-123"
	body := `{"name":"Renamed","config":{"payment_providers":{"paypal":{"client_id":"c","client_secret":"` + newSecret + `","webhook_id":"w","mode":"live"}}}}`
	req := httptest.NewRequest(http.MethodPatch, "/apps/site", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "app.update" {
		t.Errorf("action = %q, want app.update", e.Action)
	}
	if e.Actor != "admin:site" {
		t.Errorf("actor = %q, want admin:site", e.Actor)
	}
	if e.Target == nil || *e.Target != "app:site" {
		t.Errorf("target = %v, want app:site", e.Target)
	}
	// The audit detail must not contain the new secret material (nor the
	// full config payload).
	if strings.Contains(string(e.Context), newSecret) {
		t.Errorf("audit context leaks the new client_secret: %s", string(e.Context))
	}
	var ctx map[string]any
	if err := json.Unmarshal(e.Context, &ctx); err != nil {
		t.Fatalf("audit context not JSON: %v", err)
	}
	fields, ok := ctx["fields_changed"].([]any)
	if !ok || len(fields) != 2 {
		t.Fatalf("fields_changed = %v, want [name config]", ctx["fields_changed"])
	}
	if ctx["config_changed"] != true {
		t.Errorf("config_changed = %v, want true", ctx["config_changed"])
	}
}

// TestAppHandler_RotateSecret_WritesAuditNoSecretMaterial (audit I-3,
// must-record): rotation lands an app.rotate_secret row naming the app and
// the actor — and the audit trail never carries the old or new plaintext.
func TestAppHandler_RotateSecret_WritesAuditNoSecretMaterial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldHash, err := util.HashSecret("old-plaintext-secret")
	if err != nil {
		t.Fatal(err)
	}
	appRepo := &mockAppRepo{apps: []model.App{{AppID: "test", Name: "Test", IsActive: true, SecretHash: oldHash}}}
	audit := &fakeAuditWriter{}
	h := NewAppHandler(appRepo, nil, audit)

	router := gin.New()
	router.POST("/apps/:id/rotate-secret", withCallerApp("test"), h.RotateSecret)

	req := httptest.NewRequest(http.MethodPost, "/apps/test/rotate-secret", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "app.rotate_secret" {
		t.Errorf("action = %q, want app.rotate_secret", e.Action)
	}
	if e.Actor != "admin:test" {
		t.Errorf("actor = %q, want admin:test", e.Actor)
	}
	if e.Target == nil || *e.Target != "app:test" {
		t.Errorf("target = %v, want app:test", e.Target)
	}
	// Neither the old plaintext nor the new one may appear anywhere in
	// the record (context or otherwise).
	blob, _ := json.Marshal(e)
	if strings.Contains(string(blob), "old-plaintext-secret") {
		t.Errorf("audit record leaks the OLD plaintext: %s", blob)
	}
	var resp struct {
		Data struct {
			Secret string `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(string(blob), resp.Data.Secret) {
		t.Errorf("audit record leaks the NEW plaintext")
	}

	// A failed rotation (unknown app) must not audit.
	audit.entries = nil
	req = httptest.NewRequest(http.MethodPost, "/apps/missing/rotate-secret", nil)
	router2 := gin.New()
	router2.POST("/apps/:id/rotate-secret", withCallerApp("missing"), h.RotateSecret)
	w = httptest.NewRecorder()
	router2.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if len(audit.entries) != 0 {
		t.Errorf("failed rotation wrote %d audit rows, want 0", len(audit.entries))
	}
}

// TestAppHandler_AuditInsertFailureDoesNotFailRequest: the mutation has
// already committed when the audit insert runs, so an audit failure is
// logged loudly but must not turn a successful mutation into a 500 (the
// client could not safely retry a rotation anyway).
func TestAppHandler_AuditInsertFailureDoesNotFailRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	appRepo := &mockAppRepo{}
	audit := &fakeAuditWriter{err: errors.New("db down")}
	h := NewAppHandler(appRepo, nil, audit)

	router := gin.New()
	router.POST("/apps", withCallerApp("creator"), h.CreateApp)

	req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(`{"app_id":"a","name":"A"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 despite audit failure; body = %s", w.Code, w.Body.String())
	}
}
