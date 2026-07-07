package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// stubPinger implements the Pinger interface without touching a real DB.
type stubPinger struct {
	err error
}

func (s *stubPinger) PingContext(ctx context.Context) error { return s.err }

func TestHealth_HealthyReturns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &HealthHandler{Pinger: &stubPinger{err: nil}}
	r.GET("/healthz", h.Handle)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["code"].(float64) != 0 {
		t.Fatalf("expected code=0, got %v", body["code"])
	}
	data, ok := body["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected data object, got %T", body["data"])
	}
	if data["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", data["status"])
	}
}

func TestHealth_DBDownReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &HealthHandler{Pinger: &stubPinger{err: errors.New("db down")}}
	r.GET("/healthz", h.Handle)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHealth_NewHealthHandlerConstructor covers the wrapper the router
// calls into — without it the constructor is 0% covered.
func TestHealth_NewHealthHandlerConstructor(t *testing.T) {
	t.Parallel()
	h := NewHealthHandler(&stubPinger{err: nil})
	if h == nil {
		t.Fatal("NewHealthHandler returned nil")
	}
	if h.Pinger == nil {
		t.Fatal("Pinger not set")
	}
}
