package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

// stubOperatorStore implements httpapi.OperatorAdminStore in memory.
type stubOperatorStore struct {
	roles   map[string][]string
	granted []string
	revoked []string
}

func (s *stubOperatorStore) RolesForUser(_ context.Context, userID string) ([]string, error) {
	return s.roles[userID], nil
}

func (s *stubOperatorStore) GrantRole(_ context.Context, userID, role string, _ *string, _ string) (bool, error) {
	s.granted = append(s.granted, userID+":"+role)
	return true, nil
}

func (s *stubOperatorStore) RevokeRole(_ context.Context, userID, role string) (bool, error) {
	s.revoked = append(s.revoked, userID+":"+role)
	return true, nil
}

func (s *stubOperatorStore) ListOperators(_ context.Context) ([]management.OperatorGrant, error) {
	return nil, nil
}

// stubRecorder captures audit events.
type stubRecorder struct {
	events []management.AuditEvent
}

func (r *stubRecorder) Record(_ context.Context, ev management.AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

func newAuthzEngine(store httpapi.OperatorStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// Fake the two upstream auth legs: set ContextUserID/ContextApp the way
	// JWTAuth / InternalAppAuth would after verifying real credentials.
	engine.Use(func(c *gin.Context) {
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set(middleware.ContextUserID, u)
		}
		if a := c.GetHeader("X-Test-App"); a != "" {
			c.Set(middleware.ContextApp, &model.App{AppID: a})
			c.Set(middleware.ContextAppID, a) // JWTAuth leg, mirrored
		}
		c.Next()
	})
	group := engine.Group("/admin")
	group.Use(httpapi.OperatorAuthz(store, management.PermCredentialsManage))
	group.POST("/credentials", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"actor": httpapi.OperatorOf(c)})
	})
	return engine
}

func doAuthed(engine *gin.Engine, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestOperatorAuthzDualIdentity(t *testing.T) {
	store := &stubOperatorStore{roles: map[string][]string{
		"op-1": {"operator"},
		"plain-user": nil,
	}}
	engine := newAuthzEngine(store)

	// Both legs + role → 200 and the handler sees the verified attribution.
	w := doAuthed(engine, http.MethodPost, "/admin/credentials", map[string]string{
		"X-Test-User": "op-1", "X-Test-App": "yunhou-website",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("authorized operator: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Actor struct {
			UserID string `json:"user_id"`
			AppID  string `json:"app_id"`
			Roles  []string `json:"roles"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Actor.UserID != "op-1" || body.Actor.AppID != "yunhou-website" || len(body.Actor.Roles) != 1 {
		t.Fatalf("attribution not propagated: %+v", body.Actor)
	}

	// Missing JWT leg → 401.
	w = doAuthed(engine, http.MethodPost, "/admin/credentials", map[string]string{"X-Test-App": "yunhou-website"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing user identity: %d", w.Code)
	}
	// Missing service identity → 403.
	w = doAuthed(engine, http.MethodPost, "/admin/credentials", map[string]string{"X-Test-User": "op-1"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing service identity: %d", w.Code)
	}
	// Authenticated user without any operator role → 403.
	w = doAuthed(engine, http.MethodPost, "/admin/credentials", map[string]string{
		"X-Test-User": "plain-user", "X-Test-App": "yunhou-website",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-operator user: %d", w.Code)
	}
	// Neither leg → 401 (user identity is the first gate).
	w = doAuthed(engine, http.MethodPost, "/admin/credentials", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no identity at all: %d", w.Code)
	}
}

func TestOperatorAuthzPermissionSeparation(t *testing.T) {
	// auditor 只有 usage:read：可读统计、不可写凭据。
	store := &stubOperatorStore{roles: map[string][]string{"aud-1": {"auditor"}}}
	engine := newAuthzEngine(store)
	w := doAuthed(engine, http.MethodPost, "/admin/credentials", map[string]string{
		"X-Test-User": "aud-1", "X-Test-App": "yunhou-website",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("auditor must not write credentials: %d", w.Code)
	}

	// Role in a request header/body is meaningless: the middleware reads
	// only the server-verified context. Sending a forged role header does
	// not change the outcome for a non-operator.
	store2 := &stubOperatorStore{roles: map[string][]string{"plain-user": nil}}
	engine2 := newAuthzEngine(store2)
	w = doAuthed(engine2, http.MethodPost, "/admin/credentials", map[string]string{
		"X-Test-User": "plain-user", "X-Test-App": "yunhou-website", "X-Role": "admin",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("forged role must not elevate: %d", w.Code)
	}
}

func TestOperatorRequireRole(t *testing.T) {
	store := &stubOperatorStore{roles: map[string][]string{
		"adm-1":  {"admin"},
		"op-1":   {"operator"},
	}}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserID, c.GetHeader("X-Test-User"))
		c.Set(middleware.ContextApp, &model.App{AppID: c.GetHeader("X-Test-App")})
		c.Set(middleware.ContextAppID, c.GetHeader("X-Test-App")) // JWTAuth leg, mirrored
		c.Next()
	})
	g := engine.Group("/admin")
	g.Use(httpapi.OperatorRequireRole(store, management.RoleAdmin))
	g.POST("/operators", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	if w := doAuthed(engine, http.MethodPost, "/admin/operators", map[string]string{"X-Test-User": "adm-1", "X-Test-App": "a"}); w.Code != http.StatusOK {
		t.Fatalf("admin: %d", w.Code)
	}
	if w := doAuthed(engine, http.MethodPost, "/admin/operators", map[string]string{"X-Test-User": "op-1", "X-Test-App": "a"}); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin operator: %d", w.Code)
	}
}

func TestGrantRevokeHandlers(t *testing.T) {
	store := &stubOperatorStore{roles: map[string][]string{"adm-1": {"admin"}}}
	rec := &stubRecorder{}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserID, c.GetHeader("X-Test-User"))
		c.Set(middleware.ContextApp, &model.App{AppID: c.GetHeader("X-Test-App")})
		c.Set(middleware.ContextAppID, c.GetHeader("X-Test-App")) // JWTAuth leg, mirrored
		c.Next()
	})
	g := engine.Group("/admin")
	g.Use(httpapi.OperatorRequireRole(store, management.RoleAdmin))
	httpapi.NewAdminAuthHandler(store, rec).RegisterOperators(g)

	// Grant with a forged role field in the body → 400 (strict decoding).
	body := `{"user_id":"u2","role":"operator","reason":"onboard","role_elevation":"superadmin"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/operators", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", "adm-1")
	req.Header.Set("X-Test-App", "a")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("forged field must be rejected: %d %s", w.Code, w.Body.String())
	}

	// Valid grant → audited with dual attribution.
	body = `{"user_id":"u2","role":"operator","reason":"onboard"}`
	req = httptest.NewRequest(http.MethodPost, "/admin/operators", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", "adm-1")
	req.Header.Set("X-Test-App", "a")
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	if len(rec.events) != 1 || rec.events[0].Action != "permission.grant" {
		t.Fatalf("grant audit: %+v", rec.events)
	}
	if rec.events[0].ActorUser != "adm-1" || rec.events[0].ActorApp != "a" || rec.events[0].Reason != "onboard" {
		t.Fatalf("grant attribution: %+v", rec.events[0])
	}

	// Unknown role → 400.
	body = `{"user_id":"u2","role":"superuser","reason":"x"}`
	req = httptest.NewRequest(http.MethodPost, "/admin/operators", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", "adm-1")
	req.Header.Set("X-Test-App", "a")
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown role: %d", w.Code)
	}

	// Revoke → audit permission.revoke.
	req = httptest.NewRequest(http.MethodDelete, "/admin/operators/u2/roles/operator", nil)
	req.Header.Set("X-Test-User", "adm-1")
	req.Header.Set("X-Test-App", "a")
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d", w.Code)
	}
	if rec.events[len(rec.events)-1].Action != "permission.revoke" {
		t.Fatalf("revoke audit: %+v", rec.events[len(rec.events)-1])
	}
}

