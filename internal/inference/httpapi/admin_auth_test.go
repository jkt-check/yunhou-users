package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
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

// stubRecorder captures audit events; RecordTx 供同事务审计路径使用，
// txErr 注入审计写失败（验证权限变更随审计一起回滚）。
type stubRecorder struct {
	events []management.AuditEvent
	txErr  error
}

func (r *stubRecorder) Record(_ context.Context, ev management.AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

func (r *stubRecorder) RecordTx(_ context.Context, _ domain.UnitOfWork, ev management.AuditEvent) error {
	if r.txErr != nil {
		return r.txErr
	}
	r.events = append(r.events, ev)
	return nil
}

// stubUow is a fake UnitOfWork tracking commit/rollback（评审轮1 I-2 测试）。
type stubUow struct{ committed, rolledBack bool }

func (u *stubUow) Commit(context.Context) error   { u.committed = true; return nil }
func (u *stubUow) Rollback(context.Context) error { u.rolledBack = true; return nil }

// stubOperatorTxStore implements httpapi.OperatorAdminTxStore in memory.
type stubOperatorTxStore struct {
	stubOperatorStore
	uow *stubUow
}

func (s *stubOperatorTxStore) Begin(context.Context) (domain.UnitOfWork, error) { return s.uow, nil }

func (s *stubOperatorTxStore) GrantRoleTx(_ context.Context, _ domain.UnitOfWork, userID, role string, grantedBy *string, reason string) (bool, error) {
	return s.GrantRole(context.Background(), userID, role, grantedBy, reason)
}

func (s *stubOperatorTxStore) RevokeRoleTx(_ context.Context, _ domain.UnitOfWork, userID, role string) (bool, error) {
	return s.RevokeRole(context.Background(), userID, role)
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

// newOperatorAdminEngine mounts the operator-admin endpoints with the two
// identity legs stubbed by headers (same pattern as newAuthzEngine).
func newOperatorAdminEngine(store httpapi.OperatorAdminStore, rec *stubRecorder) *gin.Engine {
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
	return engine
}

func grantRequest(engine *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/admin/operators", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", "adm-1")
	req.Header.Set("X-Test-App", "a")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestGrantRevokeHandlers(t *testing.T) {
	uow := &stubUow{}
	store := &stubOperatorTxStore{
		stubOperatorStore: stubOperatorStore{roles: map[string][]string{"adm-1": {"admin"}}},
		uow:               uow,
	}
	rec := &stubRecorder{}
	engine := newOperatorAdminEngine(store, rec)

	// Grant with a forged role field in the body → 400 (strict decoding).
	w := grantRequest(engine, `{"user_id":"u2","role":"operator","reason":"onboard","role_elevation":"superadmin"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("forged field must be rejected: %d %s", w.Code, w.Body.String())
	}

	// 单值后的尾部脏数据 → 400（评审轮1 m4）。
	w = grantRequest(engine, `{"user_id":"u2","role":"operator","reason":"onboard"} trailing-garbage`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("trailing data must be rejected: %d %s", w.Code, w.Body.String())
	}

	// Valid grant → audited with dual attribution, 同事务提交。
	w = grantRequest(engine, `{"user_id":"u2","role":"operator","reason":"onboard"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	if !uow.committed || uow.rolledBack {
		t.Fatalf("grant must commit the shared tx: %+v", uow)
	}
	if len(rec.events) != 1 || rec.events[0].Action != "permission.grant" {
		t.Fatalf("grant audit: %+v", rec.events)
	}
	if rec.events[0].ActorUser != "adm-1" || rec.events[0].ActorApp != "a" || rec.events[0].Reason != "onboard" {
		t.Fatalf("grant attribution: %+v", rec.events[0])
	}

	// Unknown role → 400.
	w = grantRequest(engine, `{"user_id":"u2","role":"superuser","reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown role: %d", w.Code)
	}

	// Revoke → audit permission.revoke.
	req := httptest.NewRequest(http.MethodDelete, "/admin/operators/u2/roles/operator", nil)
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

// 评审轮1 I-2：审计写失败 → 500 且权限变更随审计一起回滚（同生共死）；
// store 不支持事务时 fail-closed（绝不落下无审计的授权）。
func TestGrantRole_AuditAndEffectAtomic(t *testing.T) {
	// 审计失败 → 回滚，不落提交。
	uow := &stubUow{}
	store := &stubOperatorTxStore{
		stubOperatorStore: stubOperatorStore{roles: map[string][]string{"adm-1": {"admin"}}},
		uow:               uow,
	}
	rec := &stubRecorder{txErr: errors.New("audit sink down")}
	engine := newOperatorAdminEngine(store, rec)
	w := grantRequest(engine, `{"user_id":"u2","role":"operator","reason":"onboard"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("audit failure = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !uow.rolledBack || uow.committed {
		t.Fatalf("audit failure must roll the grant back: %+v", uow)
	}

	// store 无事务能力 → fail-closed 500（不是降级为非原子提交）。
	plain := &stubOperatorStore{roles: map[string][]string{"adm-1": {"admin"}}}
	engine2 := newOperatorAdminEngine(plain, &stubRecorder{})
	w = grantRequest(engine2, `{"user_id":"u2","role":"operator","reason":"onboard"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("non-transactional store = %d, want fail-closed 500: %s", w.Code, w.Body.String())
	}
	if len(plain.granted) != 0 {
		t.Fatalf("fail-closed must not apply the grant: %v", plain.granted)
	}
}
