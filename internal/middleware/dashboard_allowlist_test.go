package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
)

// dashboardAllowlistEngine mounts a probe handler behind InternalAppAuth
// (faked by setting ContextApp directly — the allowlist middleware runs
// after InternalAppAuth and only reads ContextApp) plus
// DashboardAllowlist, and records whether the probe ran.
func dashboardAllowlistEngine(allowed []string, app *model.App) (*gin.Engine, *bool) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	ran := false
	engine.GET("/probe", func(c *gin.Context) {
		if app != nil {
			c.Set(ContextApp, app)
		}
	}, DashboardAllowlist(allowed), func(c *gin.Context) {
		ran = true
		c.Status(http.StatusOK)
	})
	return engine, &ran
}

func TestDashboardAllowlist_AllowedAppPasses(t *testing.T) {
	engine, ran := dashboardAllowlistEngine([]string{"yundash", "yundian"}, &model.App{AppID: "yundash"})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("allowlisted app: got %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !*ran {
		t.Error("allowlisted app: handler did not run")
	}
}

func TestDashboardAllowlist_ValidSecretButNotAllowlisted(t *testing.T) {
	// The app is fully authenticated (InternalAppAuth already ran upstream);
	// the allowlist is the only thing standing between it and the dashboard.
	engine, ran := dashboardAllowlistEngine([]string{"yundash"}, &model.App{AppID: "other-app"})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-allowlisted app: got %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if *ran {
		t.Error("non-allowlisted app: handler must not run")
	}
}

func TestDashboardAllowlist_EmptyListDeniesEverything(t *testing.T) {
	// Fail-closed default: an unconfigured allowlist (the case the
	// cmd/server startup refusal exists to catch) denies every app rather
	// than opening the surface to all callers.
	engine, ran := dashboardAllowlistEngine(nil, &model.App{AppID: "yundash"})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("empty allowlist: got %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	if *ran {
		t.Error("empty allowlist: handler must not run")
	}
}

func TestDashboardAllowlist_MissingContextAppFailsClosed(t *testing.T) {
	// ContextApp is guaranteed by the ancestor InternalAppAuth, but a
	// bare-mounted handler (unit test) must 403, not panic or pass.
	engine, ran := dashboardAllowlistEngine([]string{"yundash"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("missing ContextApp: got %d, want 403", w.Code)
	}
	if *ran {
		t.Error("missing ContextApp: handler must not run")
	}
}
