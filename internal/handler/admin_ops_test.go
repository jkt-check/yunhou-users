package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// mockAdminOpsSvc implements the handler's local adminOpsService interface.
type mockAdminOpsSvc struct {
	res *service.AdminOpsMetrics
	err error

	gotTZ string
}

func (m *mockAdminOpsSvc) Metrics(_ context.Context, tz string) (*service.AdminOpsMetrics, error) {
	m.gotTZ = tz
	return m.res, m.err
}

func TestAdminOpsMetrics(t *testing.T) {
	newEngine := func(svc adminOpsService) *gin.Engine {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		engine.GET("/admin/ops/metrics", NewAdminOpsHandler(svc).GetMetrics)
		return engine
	}

	t.Run("success envelope and tz passthrough", func(t *testing.T) {
		svc := &mockAdminOpsSvc{res: &service.AdminOpsMetrics{}}
		w := adminRequest(t, newEngine(svc), http.MethodGet, "/admin/ops/metrics?tz=UTC", "", nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":0`) {
			t.Fatalf("got %d (%s)", w.Code, w.Body.String())
		}
		if svc.gotTZ != "UTC" {
			t.Fatalf("tz = %q, want UTC", svc.gotTZ)
		}
	})

	t.Run("missing tz passes empty (service defaults)", func(t *testing.T) {
		svc := &mockAdminOpsSvc{res: &service.AdminOpsMetrics{}}
		w := adminRequest(t, newEngine(svc), http.MethodGet, "/admin/ops/metrics", "", nil)
		if w.Code != http.StatusOK || svc.gotTZ != "" {
			t.Fatalf("code=%d tz=%q", w.Code, svc.gotTZ)
		}
	})

	t.Run("invalid tz is 400", func(t *testing.T) {
		svc := &mockAdminOpsSvc{err: &service.AdminParamError{Reason: `invalid tz: "Mars/Olympus"`}}
		w := adminRequest(t, newEngine(svc), http.MethodGet, "/admin/ops/metrics?tz=Mars/Olympus", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), "Mars/Olympus") {
			t.Fatalf("message missing detail: %s", w.Body.String())
		}
	})

	t.Run("internal error is a generic 500", func(t *testing.T) {
		svc := &mockAdminOpsSvc{err: errors.New("pq: relation detail")}
		w := adminRequest(t, newEngine(svc), http.MethodGet, "/admin/ops/metrics", "", nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "relation detail") {
			t.Fatalf("internal error leaked: %s", w.Body.String())
		}
	})
}
