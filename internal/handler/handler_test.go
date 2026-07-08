package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

// --- Mocks implementing service interfaces ---

type mockAuthSvc struct {
	loginResp     *service.LoginResponse
	loginErr      error
	logoutErr     error
	refreshResp   *service.LoginResponse
	refreshErr    error
	testLoginResp *service.LoginResponse
	testLoginErr  error
}

func (m *mockAuthSvc) LoginWithProfile(ctx context.Context, req service.LoginWithProfileRequest) (*service.LoginResponse, error) {
	if m.loginErr != nil {
		return nil, m.loginErr
	}
	return m.loginResp, nil
}

func (m *mockAuthSvc) Logout(ctx context.Context, refreshToken string) error {
	return m.logoutErr
}

func (m *mockAuthSvc) RefreshToken(ctx context.Context, refreshToken, appID string) (*service.LoginResponse, error) {
	if m.refreshErr != nil {
		return nil, m.refreshErr
	}
	return m.refreshResp, nil
}

func (m *mockAuthSvc) TestLogin(ctx context.Context, req service.TestLoginRequest) (*service.LoginResponse, error) {
	if m.testLoginErr != nil {
		return nil, m.testLoginErr
	}
	return m.testLoginResp, nil
}

type mockTokenSvc struct {
	jwks map[string]interface{}
}

func (m *mockTokenSvc) JWKS() map[string]interface{} {
	return m.jwks
}

func (m *mockTokenSvc) SignAccessToken(userID, appID string, scope []string) (string, error) {
	return "test-access-token", nil
}

func (m *mockTokenSvc) VerifyAccessToken(token string) (*service.TokenClaims, error) {
	return &service.TokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "user-123"},
		AppID: "yundian",
		Scope: []string{"yundian"},
	}, nil
}

func (m *mockTokenSvc) Refresh(ctx context.Context, refreshToken, appID string) (string, string, error) {
	return "new-access", "new-refresh", nil
}

type mockSubSvc struct {
	subs       []model.Subscription
	createErr  error
	cancelErr  error
}

func (m *mockSubSvc) Create(ctx context.Context, userID, planID string, expiresAt *time.Time) (*model.Subscription, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &model.Subscription{ID: "sub-new", UserID: userID, PlanID: planID}, nil
}

func (m *mockSubSvc) Renew(ctx context.Context, id string, expiresAt *time.Time) (*model.Subscription, error) {
	return nil, nil
}

func (m *mockSubSvc) Cancel(ctx context.Context, id, userID string) error {
	return m.cancelErr
}

func (m *mockSubSvc) GetUserSubscription(ctx context.Context, userID string) (*model.Subscription, *model.Plan, error) {
	return nil, nil, nil
}

func (m *mockSubSvc) ListUserSubscriptions(ctx context.Context, userID string) ([]model.Subscription, error) {
	return m.subs, nil
}

type mockPlanSvc struct {
	plans        []model.Plan
	plan         *model.Plan
	getErr       error
	findByAppErr error
	createErr    error
	updateErr    error
	deleteErr    error
}

func (m *mockPlanSvc) ListPlans(ctx context.Context) ([]model.Plan, error) {
	return m.plans, nil
}

func (m *mockPlanSvc) FindByApp(ctx context.Context, appID string) ([]model.Plan, error) {
	if m.findByAppErr != nil {
		return nil, m.findByAppErr
	}
	var out []model.Plan
	for _, p := range m.plans {
		for _, a := range p.Apps {
			if a == appID {
				out = append(out, p)
				break
			}
		}
	}
	return out, nil
}

func (m *mockPlanSvc) GetPlan(ctx context.Context, id string) (*model.Plan, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	// Return a copy so callers writing back via UpdatePlan can't race with
	// other parallel subtests that share the same mock fixture.
	if m.plan == nil {
		return nil, nil
	}
	cp := *m.plan
	if m.plan.Apps != nil {
		cp.Apps = append([]string(nil), m.plan.Apps...)
	}
	return &cp, nil
}

func (m *mockPlanSvc) CreatePlan(ctx context.Context, p *model.Plan) error {
	return m.createErr
}

func (m *mockPlanSvc) UpdatePlan(ctx context.Context, p *model.Plan) error {
	return m.updateErr
}

func (m *mockPlanSvc) DeletePlan(ctx context.Context, id string) error {
	return m.deleteErr
}

// --- AuthHandler Tests ---

func TestAuthHandler_RefreshToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("refresh success", func(t *testing.T) {
		resp := &service.LoginResponse{
			AccessToken:  "new-access",
			RefreshToken: "new-refresh",
		}
		authSvc := &mockAuthSvc{refreshResp: resp}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/refresh", handler.RefreshToken)

		body := `{"refresh_token":"old-token"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("refresh missing token", func(t *testing.T) {
		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/refresh", handler.RefreshToken)

		body := `{}`
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})
}

func TestAuthHandler_Logout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("logout success", func(t *testing.T) {
		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/logout", handler.Logout)

		body := `{"refresh_token":"some-token"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("logout missing token", func(t *testing.T) {
		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/logout", handler.Logout)

		body := `{}`
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})
}

func TestAuthHandler_JWKS(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("jwks returns keys", func(t *testing.T) {
		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{
			jwks: map[string]interface{}{
				"keys": []map[string]interface{}{
					{"kty": "RSA", "kid": "yunhou-users-rsa"},
				},
			},
		}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.GET("/.well-known/jwks.json", handler.JWKS)

		req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}

		var resp map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &resp)
		keys, ok := resp["keys"].([]interface{})
		if !ok || len(keys) == 0 {
			t.Error("expected keys in response")
		}
	})
}

// --- PlanHandler Tests ---

func TestPlanHandler_ListPlans(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("list plans success", func(t *testing.T) {
		plans := []model.Plan{
			{ID: "free", Name: "Free", Apps: []string{"yundian"}},
			{ID: "monthly", Name: "Monthly", Apps: []string{"yundian", "yundash"}},
		}
		planSvc := &mockPlanSvc{plans: plans}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.GET("/admin/plans", handler.ListPlans)

		req := httptest.NewRequest(http.MethodGet, "/admin/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("empty plan list → 200 with empty array", func(t *testing.T) {
		planSvc := &mockPlanSvc{plans: nil}
		handler := NewPlanHandler(planSvc, nil, nil)
		router := gin.New()
		router.GET("/admin/plans", handler.ListPlans)
		req := httptest.NewRequest(http.MethodGet, "/admin/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

func TestPlanHandler_CreatePlan(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("create plan success", func(t *testing.T) {
		planSvc := &mockPlanSvc{}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.POST("/admin/plans", handler.CreatePlan)

		body := `{"id":"test","name":"Test Plan","price":9.99,"interval_days":30,"apps":["yundian"]}`
		req := httptest.NewRequest(http.MethodPost, "/admin/plans", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Errorf("expected 201, got %d", w.Code)
		}
	})

	t.Run("create plan invalid body", func(t *testing.T) {
		planSvc := &mockPlanSvc{}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.POST("/admin/plans", handler.CreatePlan)

		body := `invalid`
		req := httptest.NewRequest(http.MethodPost, "/admin/plans", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})
}

func TestPlanHandler_DeletePlan(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("delete plan success", func(t *testing.T) {
		planSvc := &mockPlanSvc{}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.DELETE("/admin/plans/:id", handler.DeletePlan)

		req := httptest.NewRequest(http.MethodDelete, "/admin/plans/test-plan", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

func TestPlanHandler_GetPlan(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("get plan success", func(t *testing.T) {
		plan := &model.Plan{ID: "monthly", Name: "Monthly Plan"}
		planSvc := &mockPlanSvc{plan: plan}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.GET("/admin/plans/:id", handler.GetPlan)

		req := httptest.NewRequest(http.MethodGet, "/admin/plans/monthly", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("get plan not found", func(t *testing.T) {
		planSvc := &mockPlanSvc{getErr: sql.ErrNoRows}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.GET("/admin/plans/:id", handler.GetPlan)

		req := httptest.NewRequest(http.MethodGet, "/admin/plans/nonexistent", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestPlanHandler_UpdatePlan(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("update plan success", func(t *testing.T) {
		plan := &model.Plan{ID: "monthly", Name: "Monthly", Price: 9.99}
		planSvc := &mockPlanSvc{plan: plan}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.PATCH("/admin/plans/:id", handler.UpdatePlan)

		body := `{"name":"Monthly Updated","price":19.99}`
		req := httptest.NewRequest(http.MethodPatch, "/admin/plans/monthly", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("update plan not found", func(t *testing.T) {
		planSvc := &mockPlanSvc{getErr: sql.ErrNoRows}
		handler := NewPlanHandler(planSvc, nil, nil)

		router := gin.New()
		router.PATCH("/admin/plans/:id", handler.UpdatePlan)

		body := `{"name":"Updated"}`
		req := httptest.NewRequest(http.MethodPatch, "/admin/plans/nonexistent", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

// --- SubscriptionHandler Tests ---

func TestSubscriptionHandler_ListUserSubscriptions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("list subscriptions success", func(t *testing.T) {
		subs := []model.Subscription{
			{ID: "sub-1", UserID: "user-1", PlanID: "free"},
		}
		subSvc := &mockSubSvc{subs: subs}
		handler := NewSubscriptionHandler(subSvc)

		router := gin.New()
		router.GET("/user/subscriptions", func(c *gin.Context) { c.Set("user_id", "user-123") }, handler.ListUserSubscriptions)

		req := httptest.NewRequest(http.MethodGet, "/user/subscriptions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("missing auth → 401", func(t *testing.T) {
		subSvc := &mockSubSvc{}
		handler := NewSubscriptionHandler(subSvc)
		router := gin.New()
		router.GET("/user/subscriptions", handler.ListUserSubscriptions)
		req := httptest.NewRequest(http.MethodGet, "/user/subscriptions", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})
}

func TestSubscriptionHandler_CreateSubscription(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("create subscription success", func(t *testing.T) {
		subSvc := &mockSubSvc{}
		handler := NewSubscriptionHandler(subSvc)

		router := gin.New()
		router.POST("/user/subscriptions", func(c *gin.Context) { c.Set("user_id", "user-123") }, handler.CreateSubscription)

		body := `{"plan_id":"monthly"}`
		req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Errorf("expected 201, got %d", w.Code)
		}
	})
}

func TestSubscriptionHandler_CancelSubscription(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("cancel subscription success", func(t *testing.T) {
		subSvc := &mockSubSvc{}
		handler := NewSubscriptionHandler(subSvc)

		router := gin.New()
		router.DELETE("/user/subscriptions/:id", func(c *gin.Context) { c.Set("user_id", "user-123") }, handler.CancelSubscription)

		req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/sub-1", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

// --- AppHandler Tests ---

type mockAppRepo struct {
	apps      []model.App
	findErr   error
	createErr error
	updateErr error
}

func (m *mockAppRepo) List(ctx context.Context) ([]model.App, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	return m.apps, nil
}

func (m *mockAppRepo) ListUnhashed(ctx context.Context) ([]model.App, error) {
	var out []model.App
	for _, a := range m.apps {
		if a.SecretHash == "" {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *mockAppRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	for _, a := range m.apps {
		if a.AppID == id {
			return &a, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *mockAppRepo) Create(ctx context.Context, a *model.App) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.apps = append(m.apps, *a)
	return nil
}

func (m *mockAppRepo) Update(ctx context.Context, a *model.App) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	for i, app := range m.apps {
		if app.AppID == a.AppID {
			m.apps[i] = *a
			break
		}
	}
	return nil
}

func (m *mockAppRepo) RotateSecretHash(ctx context.Context, appID, newHash string) error {
	for i, app := range m.apps {
		if app.AppID == appID {
			m.apps[i].SecretHash = newHash
			return nil
		}
	}
	return sql.ErrNoRows
}

func (m *mockAppRepo) BackfillSecretHash(ctx context.Context, appID, newHash string) (bool, error) {
	for i, app := range m.apps {
		if app.AppID == appID {
			if m.apps[i].SecretHash != "" {
				return true, nil // mimic production guard
			}
			m.apps[i].SecretHash = newHash
			return false, nil
		}
	}
	return false, sql.ErrNoRows
}

func TestAppHandler_ListApps(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("list apps success", func(t *testing.T) {
		apps := []model.App{
			{AppID: "yundian", Name: "Yundian"},
			{AppID: "yundash", Name: "Yundash"},
		}
		appRepo := &mockAppRepo{apps: apps}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.GET("/apps", handler.ListApps)

		req := httptest.NewRequest(http.MethodGet, "/apps", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("list apps error", func(t *testing.T) {
		appRepo := &mockAppRepo{findErr: errors.New("db error")}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.GET("/apps", handler.ListApps)

		req := httptest.NewRequest(http.MethodGet, "/apps", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestAppHandler_GetApp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("get existing app", func(t *testing.T) {
		apps := []model.App{{AppID: "yundian", Name: "Yundian"}}
		appRepo := &mockAppRepo{apps: apps}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.GET("/apps/:id", handler.GetApp)

		req := httptest.NewRequest(http.MethodGet, "/apps/yundian", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("get nonexistent app", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.GET("/apps/:id", handler.GetApp)

		req := httptest.NewRequest(http.MethodGet, "/apps/nonexistent", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestAppHandler_CreateApp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("create app success", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.POST("/apps", handler.CreateApp)

		body := `{"app_id":"test","name":"Test App"}`
		req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
		}
		// Plaintext secret must be in the response (one-time) and the row's
		// SecretHash must verify against that plaintext via bcrypt.
		var resp struct {
			Code int `json:"code"`
			Data struct {
				App    *model.App `json:"app"`
				Secret string     `json:"secret"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Data.Secret == "" {
			t.Fatal("data.secret must be populated on create")
		}
		if len(appRepo.apps) != 1 || !util.CheckSecret(appRepo.apps[0].SecretHash, resp.Data.Secret) {
			t.Errorf("stored hash does not verify against returned plaintext; stored hash = %q", appRepo.apps[0].SecretHash)
		}
		// Hash field must never leak in JSON responses (json:"-").
		if resp.Data.App.SecretHash != "" {
			t.Errorf("app.secret_hash leaked in response: %q", resp.Data.App.SecretHash)
		}
	})

	t.Run("create app invalid body", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.POST("/apps", handler.CreateApp)

		body := `invalid`
		req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("create app with payment_providers config", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.POST("/apps", handler.CreateApp)

		body := `{"app_id":"site","name":"Site","config":{"payment_providers":{"paypal":{"client_id":"cid","client_secret":"cs","webhook_id":"W","mode":"live","plans":{"m":{"plan_id":"P","trial_days":0,"billing_cycle_days":30}}}}}}`
		req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
		}
		if len(appRepo.apps) != 1 {
			t.Fatalf("expected 1 app stored, got %d", len(appRepo.apps))
		}
		if !bytes.Contains(appRepo.apps[0].Config, []byte(`"paypal"`)) {
			t.Errorf("config not persisted; got %s", string(appRepo.apps[0].Config))
		}
	})

	t.Run("create app rejects invalid paypal config", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.POST("/apps", handler.CreateApp)

		// missing secret + webhook_id + mode
		body := `{"app_id":"site","name":"Site","config":{"payment_providers":{"paypal":{"client_id":"cid"}}}}`
		req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
	})
}

func TestAppHandler_RotateSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("rotate secret success", func(t *testing.T) {
		// Pre-seed an app with a known old hash. After rotation, the old
		// plaintext must no longer verify against the stored hash and the
		// new plaintext returned in the response must verify.
		oldHash, err := util.HashSecret("old-secret")
		if err != nil {
			t.Fatal(err)
		}
		appRepo := &mockAppRepo{apps: []model.App{{AppID: "test", Name: "Test", IsActive: true, SecretHash: oldHash}}}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.POST("/apps/:id/rotate-secret", handler.RotateSecret)

		req := httptest.NewRequest(http.MethodPost, "/apps/test/rotate-secret", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
			Data struct {
				Secret string `json:"secret"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		newPlain := resp.Data.Secret
		if newPlain == "" {
			t.Fatal("data.secret must be populated on rotate")
		}
		if newPlain == "old-secret" {
			t.Error("rotated secret reused the old plaintext")
		}
		if !util.CheckSecret(appRepo.apps[0].SecretHash, newPlain) {
			t.Errorf("stored hash does not verify against the new plaintext; hash = %q", appRepo.apps[0].SecretHash)
		}
		if util.CheckSecret(appRepo.apps[0].SecretHash, "old-secret") {
			t.Error("old plaintext still verifies — rotation didn't actually replace the hash")
		}
	})

	t.Run("rotate secret for unknown app", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.POST("/apps/:id/rotate-secret", handler.RotateSecret)

		req := httptest.NewRequest(http.MethodPost, "/apps/missing/rotate-secret", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestAppHandler_UpdateApp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("update app success", func(t *testing.T) {
		apps := []model.App{{AppID: "test", Name: "Old Name"}}
		appRepo := &mockAppRepo{apps: apps}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.PATCH("/apps/:id", handler.UpdateApp)

		body := `{"name":"New Name"}`
		req := httptest.NewRequest(http.MethodPatch, "/apps/test", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("update nonexistent app", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo, nil)

		router := gin.New()
		router.PATCH("/apps/:id", handler.UpdateApp)

		body := `{"name":"New Name"}`
		req := httptest.NewRequest(http.MethodPatch, "/apps/nonexistent", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("update app replaces config", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		existing := []model.App{
			{AppID: "site", Name: "x", Config: json.RawMessage(`{"old":"value"}`)},
		}
		appRepo := &mockAppRepo{apps: existing}
		handler := NewAppHandler(appRepo, nil)
		router := gin.New()
		router.PATCH("/apps/:id", handler.UpdateApp)

		body := `{"config":{"payment_providers":{"paypal":{"client_id":"c","client_secret":"s","webhook_id":"w","mode":"live"}}}}`
		req := httptest.NewRequest(http.MethodPatch, "/apps/site", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		updated, err := appRepo.FindByID(context.Background(), "site")
		if err != nil {
			t.Fatalf("find after update: %v", err)
		}
		if !bytes.Contains(updated.Config, []byte(`"paypal"`)) {
			t.Errorf("config not replaced; got %s", string(updated.Config))
		}
		if bytes.Contains(updated.Config, []byte(`"old"`)) {
			t.Errorf("old config not fully replaced; got %s", string(updated.Config))
		}
	})

	t.Run("update app with invalid config returns 400", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		existing := []model.App{{AppID: "site", Name: "x"}}
		appRepo := &mockAppRepo{apps: existing}
		handler := NewAppHandler(appRepo, nil)
		router := gin.New()
		router.PATCH("/apps/:id", handler.UpdateApp)

		body := `{"config":{"payment_providers":{"paypal":{"client_id":"only"}}}}`
		req := httptest.NewRequest(http.MethodPatch, "/apps/site", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

// --- UserHandler tests ---

func TestUserHandler_GetProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("get profile success", func(t *testing.T) {
		userID := "user-123"
		nickname := "testuser"
		user := &model.User{ID: userID, Nickname: &nickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.GET("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.GetProfile)

		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		userRepo := &mockUserRepo{findErr: sql.ErrNoRows}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.GET("/profile", func(c *gin.Context) { c.Set("user_id", "nonexistent") }, handler.GetProfile)

		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestUserHandler_UpdateProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("update nickname success", func(t *testing.T) {
		userID := "user-123"
		oldNickname := "oldname"
		user := &model.User{ID: userID, Nickname: &oldNickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UpdateProfile)

		body := `{"nickname":"newname"}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("update avatar_url success", func(t *testing.T) {
		userID := "user-123"
		nickname := "testuser"
		user := &model.User{ID: userID, Nickname: &nickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UpdateProfile)

		body := `{"avatar_url":"https://example.com/avatar.png"}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("invalid avatar_url - not https", func(t *testing.T) {
		userID := "user-123"
		nickname := "testuser"
		user := &model.User{ID: userID, Nickname: &nickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UpdateProfile)

		body := `{"avatar_url":"http://example.com/avatar.png"}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("invalid avatar_url - userinfo phishing", func(t *testing.T) {
		// https://x:y@evil.com is parsed as having Host=evil.com and would
		// display as a link to evil.com while looking like "x" in the URL.
		// The handler must reject this.
		userID := "user-123"
		nickname := "testuser"
		user := &model.User{ID: userID, Nickname: &nickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UpdateProfile)

		body := `{"avatar_url":"https://victim.example.com:pw@evil.com/x.png"}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for userinfo URL, got %d", w.Code)
		}
	})

	t.Run("invalid avatar_url - has fragment", func(t *testing.T) {
		userID := "user-123"
		nickname := "testuser"
		user := &model.User{ID: userID, Nickname: &nickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UpdateProfile)

		body := `{"avatar_url":"https://example.com/x.png#frag"}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for fragment URL, got %d", w.Code)
		}
	})

	t.Run("invalid nickname - empty after trim", func(t *testing.T) {
		userID := "user-123"
		nickname := "testuser"
		user := &model.User{ID: userID, Nickname: &nickname}
		userRepo := &mockUserRepo{user: user}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UpdateProfile)

		body := `{"nickname":"   "}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for whitespace-only nickname, got %d", w.Code)
		}
	})

	t.Run("user not found", func(t *testing.T) {
		userRepo := &mockUserRepo{findErr: sql.ErrNoRows}
		handler := NewUserHandler(userRepo, &mockIdentityRepo{})

		router := gin.New()
		router.PATCH("/profile", func(c *gin.Context) { c.Set("user_id", "nonexistent") }, handler.UpdateProfile)

		body := `{"nickname":"newname"}`
		req := httptest.NewRequest(http.MethodPatch, "/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestUserHandler_ListIdentities(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("list identities success", func(t *testing.T) {
		userID := "user-123"
		identities := []model.SocialIdentity{
			{ID: "id1", UserID: userID, Provider: "github"},
			{ID: "id2", UserID: userID, Provider: "google"},
		}
		identityRepo := &mockIdentityRepo{identities: identities}
		handler := NewUserHandler(&mockUserRepo{}, identityRepo)

		router := gin.New()
		router.GET("/identities", func(c *gin.Context) { c.Set("user_id", userID) }, handler.ListIdentities)

		req := httptest.NewRequest(http.MethodGet, "/identities", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("list identities error", func(t *testing.T) {
		userID := "user-123"
		identityRepo := &mockIdentityRepo{listErr: errors.New("db error")}
		handler := NewUserHandler(&mockUserRepo{}, identityRepo)

		router := gin.New()
		router.GET("/identities", func(c *gin.Context) { c.Set("user_id", userID) }, handler.ListIdentities)

		req := httptest.NewRequest(http.MethodGet, "/identities", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestUserHandler_UnbindIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("unbind success", func(t *testing.T) {
		userID := "user-123"
		identityID := "id1"
		identityRepo := &mockIdentityRepo{deleteResult: true}
		handler := NewUserHandler(&mockUserRepo{}, identityRepo)

		router := gin.New()
		router.DELETE("/identities/:id", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UnbindIdentity)

		req := httptest.NewRequest(http.MethodDelete, "/identities/"+identityID, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("unbind - last identity", func(t *testing.T) {
		userID := "user-123"
		identityRepo := &mockIdentityRepo{deleteResult: false}
		handler := NewUserHandler(&mockUserRepo{}, identityRepo)

		router := gin.New()
		router.DELETE("/identities/:id", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UnbindIdentity)

		req := httptest.NewRequest(http.MethodDelete, "/identities/id1", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("unbind error", func(t *testing.T) {
		userID := "user-123"
		identityRepo := &mockIdentityRepo{deleteErr: errors.New("db error")}
		handler := NewUserHandler(&mockUserRepo{}, identityRepo)

		router := gin.New()
		router.DELETE("/identities/:id", func(c *gin.Context) { c.Set("user_id", userID) }, handler.UnbindIdentity)

		req := httptest.NewRequest(http.MethodDelete, "/identities/id1", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestUserHandler_ListAppsRemoved(t *testing.T) {
	// UserHandler.ListApps was removed: subscriptions live under the
	// SubscriptionHandler (`GET /user/subscriptions`), so this test exists
	// only as a placeholder documenting the deletion.
	_ = (*UserHandler)(nil)
}

// ============================================================================
// Error-path coverage for the handlers most under-tested above.
// Each section targets a handler whose function coverage is below ~75%.
// ============================================================================

func newUserEngine(userRepo *mockUserRepo, identityRepo *mockIdentityRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewUserHandler(userRepo, identityRepo)
	engine.GET("/user/profile", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "user-1")
		h.GetProfile(c)
	})
	engine.PATCH("/user/profile", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "user-1")
		h.UpdateProfile(c)
	})
	return engine
}

func TestUserHandler_GetProfile_DBError_500(t *testing.T) {
	t.Parallel()
	userRepo := &mockUserRepo{findErr: errors.New("db down")}
	engine := newUserEngine(userRepo, &mockIdentityRepo{})
	req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("DB error: got %d, want 500", rec.Code)
	}
}

func TestUserHandler_UpdateProfile_ErrorPaths(t *testing.T) {
	t.Parallel()
	user := &model.User{ID: "user-1", Nickname: ptr("Old")}

	cases := []struct {
		name        string
		userRepo    *mockUserRepo
		body        string
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "user not found → 404",
			userRepo:    &mockUserRepo{findErr: sql.ErrNoRows},
			body:        `{"nickname":"new"}`,
			wantStatus:  http.StatusNotFound,
			wantMessage: "user not found",
		},
		{
			name:        "DB error on lookup → 500",
			userRepo:    &mockUserRepo{findErr: errors.New("db down")},
			body:        `{"nickname":"new"}`,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "failed to load profile",
		},
		{
			name:        "bad JSON → 400",
			userRepo:    &mockUserRepo{user: user},
			body:        `{`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "invalid request body",
		},
		{
			name:        "empty nickname → 400",
			userRepo:    &mockUserRepo{user: user},
			body:        `{"nickname":"   "}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "nickname must be 1-100 characters",
		},
		{
			name:        "too-long nickname → 400",
			userRepo:    &mockUserRepo{user: user},
			body:        `{"nickname":"` + strings.Repeat("x", 101) + `"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "nickname must be 1-100 characters",
		},
		{
			name:        "non-https avatar_url → 400",
			userRepo:    &mockUserRepo{user: user},
			body:        `{"avatar_url":"http://example.com/x.png"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "avatar_url must be a valid HTTPS URL",
		},
		{
			name:        "avatar_url with fragment → 400",
			userRepo:    &mockUserRepo{user: user},
			body:        `{"avatar_url":"https://example.com/x.png#y"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "avatar_url must be a valid HTTPS URL",
		},
		{
			name:        "avatar_url with userinfo → 400",
			userRepo:    &mockUserRepo{user: user},
			body:        `{"avatar_url":"https://u:p@example.com/x.png"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "avatar_url must be a valid HTTPS URL",
		},
		{
			name:        "DB error on update → 500",
			userRepo:    &mockUserRepo{user: user, updateErr: errors.New("db down")},
			body:        `{"nickname":"new-name"}`,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "failed to update profile",
		},
		{
			name:       "valid update → 200",
			userRepo:   &mockUserRepo{user: user},
			body:       `{"nickname":"  new-name  ","avatar_url":"https://example.com/x.png"}`,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newUserEngine(tc.userRepo, &mockIdentityRepo{})
			req := httptest.NewRequest(http.MethodPatch, "/user/profile", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}

// ==== SubscriptionHandler.CreateSubscription — error paths ====

func newSubEngine(subSvc *mockSubSvc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	sh := NewSubscriptionHandler(subSvc)
	engine.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "user-1")
		sh.CreateSubscription(c)
	})
	engine.DELETE("/user/subscriptions/:id", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "user-1")
		sh.CancelSubscription(c)
	})
	return engine
}

func TestSubscriptionHandler_CreateSubscription_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		subSvc      *mockSubSvc
		body        string
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "bad JSON → 400",
			subSvc:      &mockSubSvc{},
			body:        `{`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "invalid request body",
		},
		{
			name:        "missing plan_id → 400",
			subSvc:      &mockSubSvc{},
			body:        `{}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "invalid request body",
		},
		{
			name:        "invalid expires_at → 400",
			subSvc:      &mockSubSvc{},
			body:        `{"plan_id":"monthly","expires_at":"not-rfc3339"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "invalid expires_at format",
		},
		{
			name:        "ErrPlanNotFound → 400",
			subSvc:      &mockSubSvc{createErr: service.ErrPlanNotFound},
			body:        `{"plan_id":"unknown"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "plan not found",
		},
		{
			name:        "ErrPlanInactive → 400",
			subSvc:      &mockSubSvc{createErr: service.ErrPlanInactive},
			body:        `{"plan_id":"disabled"}`,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "plan is inactive",
		},
		{
			name:        "ErrPaidPlanForbidden → 403",
			subSvc:      &mockSubSvc{createErr: service.ErrPaidPlanForbidden},
			body:        `{"plan_id":"paid"}`,
			wantStatus:  http.StatusForbidden,
			wantMessage: "paid plans require payment",
		},
		{
			name:        "ErrUserHasActiveSub → 409",
			subSvc:      &mockSubSvc{createErr: service.ErrUserHasActiveSub},
			body:        `{"plan_id":"monthly"}`,
			wantStatus:  http.StatusConflict,
			wantMessage: "user already has an active subscription",
		},
		{
			name:        "unknown error → 500",
			subSvc:      &mockSubSvc{createErr: errors.New("db exploded")},
			body:        `{"plan_id":"monthly"}`,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "failed to create",
		},
		{
			name:       "valid subscription success → 201",
			subSvc:     &mockSubSvc{},
			body:       `{"plan_id":"monthly","expires_at":"2026-12-31T23:59:59Z"}`,
			wantStatus: http.StatusCreated,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newSubEngine(tc.subSvc)
			req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}

func TestSubscriptionHandler_CreateSubscription_NoAuth_401(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	sh := NewSubscriptionHandler(&mockSubSvc{})
	engine := gin.New()
	// No middleware that sets ContextUserID → handler sees empty userID.
	engine.POST("/user/subscriptions", sh.CreateSubscription)
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions",
		strings.NewReader(`{"plan_id":"monthly"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d, want 401", rec.Code)
	}
}

// ==== SubscriptionHandler.CancelSubscription — error paths ====

func TestSubscriptionHandler_CancelSubscription_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		subSvc      *mockSubSvc
		wantStatus  int
		wantMessage string
	}{
		{"ErrSubscriptionNotFound → 404",
			&mockSubSvc{cancelErr: service.ErrSubscriptionNotFound},
			http.StatusNotFound, "subscription not found"},
		{"ErrAlreadyCancelled → 400",
			&mockSubSvc{cancelErr: service.ErrAlreadyCancelled},
			http.StatusBadRequest, "already cancelled"},
		{"unknown error → 500",
			&mockSubSvc{cancelErr: errors.New("db exploded")},
			http.StatusInternalServerError, "failed to cancel"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newSubEngine(tc.subSvc)
			req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/sub-1", nil)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}

func TestSubscriptionHandler_CancelSubscription_NoAuth_401(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	sh := NewSubscriptionHandler(&mockSubSvc{})
	engine := gin.New()
	engine.DELETE("/user/subscriptions/:id", sh.CancelSubscription)
	req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/sub-1", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing auth: got %d, want 401", rec.Code)
	}
}

// ==== PlanHandler.DeletePlan / UpdatePlan error paths ====

func TestPlanHandler_DeletePlan_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		svc        *mockPlanSvc
		wantStatus int
		wantMessage string
	}{
		{"PlanNotFound → 500",
			&mockPlanSvc{deleteErr: sql.ErrNoRows},
			http.StatusInternalServerError, "failed to delete plan"},
		{"unknown error → 500",
			&mockPlanSvc{deleteErr: errors.New("db exploded")},
			http.StatusInternalServerError, "failed to delete plan"},
		{"success → 200",
			&mockPlanSvc{},
			http.StatusOK, "deleted"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gin.SetMode(gin.TestMode)
			h := NewPlanHandler(tc.svc, nil, nil)
			engine := gin.New()
			engine.DELETE("/admin/plans/:id", h.DeletePlan)
			req := httptest.NewRequest(http.MethodDelete, "/admin/plans/x", nil)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}

func TestPlanHandler_UpdatePlan_ErrorPaths(t *testing.T) {
	t.Parallel()
	existingPlan := &model.Plan{ID: "monthly", Name: "Monthly", Price: 9.99}
	cases := []struct {
		name        string
		svc         *mockPlanSvc
		body        string
		wantStatus  int
		wantMessage string
	}{
		{"bad JSON → 400", &mockPlanSvc{plan: existingPlan}, `{`,
			http.StatusBadRequest, "invalid request body"},
		{"plan not found → 404",
			&mockPlanSvc{getErr: sql.ErrNoRows},
			`{"name":"x"}`, http.StatusNotFound, "plan not found"},
		{"DB lookup error → 500",
			&mockPlanSvc{getErr: errors.New("db exploded")},
			`{"name":"x"}`, http.StatusInternalServerError, "failed to load plan"},
		{"negative price → 400",
			&mockPlanSvc{plan: existingPlan},
			`{"price":-1}`, http.StatusBadRequest, "price must be"},
		{"negative interval_days → 400",
			&mockPlanSvc{plan: existingPlan},
			`{"interval_days":-1}`, http.StatusBadRequest, "interval_days must be"},
		{"update repo error → 500",
			&mockPlanSvc{plan: existingPlan, updateErr: errors.New("db exploded")},
			`{"name":"updated"}`, http.StatusInternalServerError, "failed to update plan"},
		{"success → 200",
			&mockPlanSvc{plan: existingPlan},
			`{"name":"updated"}`, http.StatusOK, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gin.SetMode(gin.TestMode)
			h := NewPlanHandler(tc.svc, nil, nil)
			engine := gin.New()
			engine.PATCH("/admin/plans/:id", h.UpdatePlan)
			req := httptest.NewRequest(http.MethodPatch, "/admin/plans/x",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}

func TestPlanHandler_CreatePlan_ErrorPaths(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		svc        *mockPlanSvc
		body       string
		wantStatus int
	}{
		{"bad JSON → 400", &mockPlanSvc{}, `{`, http.StatusBadRequest},
		{"unknown error → 500",
			&mockPlanSvc{createErr: errors.New("db exploded")},
			`{"id":"monthly","name":"m"}`, http.StatusInternalServerError},
		{"success → 201", &mockPlanSvc{},
			`{"id":"monthly","name":"m"}`, http.StatusCreated},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewPlanHandler(tc.svc, nil, nil)
			engine := gin.New()
			engine.POST("/admin/plans", h.CreatePlan)
			req := httptest.NewRequest(http.MethodPost, "/admin/plans",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// ==== Auth handler error paths ====

func TestAuthHandler_RefreshToken_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		svc         *mockAuthSvc
		body        string
		wantStatus  int
		wantMessage string
	}{
		{"bad JSON → 400", &mockAuthSvc{}, `{`,
			http.StatusBadRequest, "invalid request body"},
		{"ErrInvalidRefreshToken → 401",
			&mockAuthSvc{refreshErr: service.ErrInvalidRefreshToken},
			`{"refresh_token":"bad","app_id":"a"}`,
			http.StatusUnauthorized, "invalid refresh token"},
		{"unknown error → 500",
			&mockAuthSvc{refreshErr: errors.New("db exploded")},
			`{"refresh_token":"x","app_id":"a"}`,
			http.StatusInternalServerError, "refresh failed"},
		{"success → 200",
			&mockAuthSvc{refreshResp: &service.LoginResponse{}},
			`{"refresh_token":"x","app_id":"a"}`,
			http.StatusOK, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gin.SetMode(gin.TestMode)
			h := NewAuthHandler(tc.svc, &mockTokenSvc{})
			engine := gin.New()
			engine.POST("/auth/refresh", h.RefreshToken)
			req := httptest.NewRequest(http.MethodPost, "/auth/refresh",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantMessage != "" && !strings.Contains(rec.Body.String(), tc.wantMessage) {
				t.Errorf("body missing %q: %s", tc.wantMessage, rec.Body.String())
			}
		})
	}
}

func TestAuthHandler_Logout_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		svc        *mockAuthSvc
		wantStatus int
	}{
		{"bad JSON → 400",
			&mockAuthSvc{},
			http.StatusBadRequest},
		{"unknown error → 500",
			&mockAuthSvc{logoutErr: errors.New("db exploded")},
			http.StatusInternalServerError},
		{"success → 200",
			&mockAuthSvc{},
			http.StatusOK},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gin.SetMode(gin.TestMode)
			h := NewAuthHandler(tc.svc, &mockTokenSvc{})
			engine := gin.New()
			engine.POST("/auth/logout", h.Logout)
			body := `{"refresh_token":"x"}`
			if tc.name == "bad JSON → 400" {
				body = `{`
			}
			req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// ==== App handler error paths ====

func newAppEngine(repo *mockAppRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewAppHandler(repo, nil)
	engine.GET("/apps/:id", h.GetApp)
	engine.POST("/admin/apps", h.CreateApp)
	engine.PATCH("/admin/apps/:id", h.UpdateApp)
	return engine
}

func TestAppHandler_GetApp_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		repo       *mockAppRepo
		wantStatus int
	}{
		{"ErrAppNotFound → 404",
			&mockAppRepo{findErr: sql.ErrNoRows}, http.StatusNotFound},
		{"DB error → 500",
			&mockAppRepo{findErr: errors.New("db exploded")},
			http.StatusInternalServerError},
		{"success → 200",
			&mockAppRepo{apps: []model.App{{AppID: "yundian", Name: "云店"}}},
			http.StatusOK},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newAppEngine(tc.repo)
			req := httptest.NewRequest(http.MethodGet, "/apps/yundian", nil)
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestAppHandler_CreateApp_ErrorPaths(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		repo       *mockAppRepo
		body       string
		wantStatus int
	}{
		{"bad JSON → 400", &mockAppRepo{}, `{`, http.StatusBadRequest},
		{"unknown error → 500",
			&mockAppRepo{createErr: errors.New("db exploded")},
			`{"app_id":"yundian","name":"云店"}`, http.StatusInternalServerError},
		{"success → 201",
			&mockAppRepo{},
			`{"app_id":"yundian","name":"云店"}`, http.StatusCreated},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newAppEngine(tc.repo)
			req := httptest.NewRequest(http.MethodPost, "/admin/apps",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestAppHandler_UpdateApp_ErrorPaths(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		repo       *mockAppRepo
		body       string
		wantStatus int
	}{
		{"bad JSON → 400", &mockAppRepo{apps: []model.App{{AppID: "yundian"}}}, `{`, http.StatusBadRequest},
		{"ErrAppNotFound → 404",
			&mockAppRepo{updateErr: service.ErrAppNotFound},
			`{"name":"new"}`, http.StatusNotFound},
		{"unknown error → 500",
			&mockAppRepo{apps: []model.App{{AppID: "yundian"}}, updateErr: errors.New("db exploded")},
			`{"name":"new"}`, http.StatusInternalServerError},
		{"success → 200",
			&mockAppRepo{apps: []model.App{{AppID: "yundian"}}},
			`{"name":"new"}`, http.StatusOK},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := newAppEngine(tc.repo)
			req := httptest.NewRequest(http.MethodPatch, "/admin/apps/yundian",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// ptr is a tiny helper so test case declarations stay one-liners.
func ptr(s string) *string { return &s }

// --- Mock implementations for UserHandler tests ---

type mockUserRepo struct {
	user     *model.User
	findErr  error
	updateErr error
}

func (m *mockUserRepo) FindByID(ctx context.Context, id string) (*model.User, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	return m.user, nil
}

func (m *mockUserRepo) Update(ctx context.Context, u *model.User) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.user = u
	return nil
}

type mockIdentityRepo struct {
	identities  []model.SocialIdentity
	listErr     error
	deleteResult bool
	deleteErr   error
}

func (m *mockIdentityRepo) ListByUserID(ctx context.Context, userID string) ([]model.SocialIdentity, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.identities, nil
}

func (m *mockIdentityRepo) DeleteIfNotLast(ctx context.Context, id, userID string) (bool, error) {
	if m.deleteErr != nil {
		return false, m.deleteErr
	}
	return m.deleteResult, nil
}

// --- GetProviderToken tests ---

type fakeProviderToken struct {
	result *model.ProviderToken
	err    error
	calls  int
}

func (f *fakeProviderToken) Get(ctx context.Context, appID, channel string) (*model.ProviderToken, error) {
	f.calls++
	return f.result, f.err
}

func TestAppHandler_GetProviderToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("paypal success", func(t *testing.T) {
		appRepo := &mockAppRepo{apps: []model.App{{AppID: "site", IsActive: true}}}
		pt := &fakeProviderToken{result: &model.ProviderToken{Channel: "paypal", AccessToken: "AT", ExpiresIn: 3600}}
		handler := NewAppHandler(appRepo, pt)
		router := gin.New()
		router.GET("/apps/:id/provider-token/:channel", handler.GetProviderToken)

		req := httptest.NewRequest(http.MethodGet, "/apps/site/provider-token/paypal", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int                  `json:"code"`
			Data *model.ProviderToken `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Data == nil || resp.Data.AccessToken != "AT" {
			t.Errorf("data = %+v", resp.Data)
		}
		if pt.calls != 1 {
			t.Errorf("service calls = %d, want 1", pt.calls)
		}
	})

	t.Run("unsupported channel returns 400", func(t *testing.T) {
		pt := &fakeProviderToken{err: service.ErrUnsupportedChannel}
		handler := NewAppHandler(&mockAppRepo{}, pt)
		router := gin.New()
		router.GET("/apps/:id/provider-token/:channel", handler.GetProviderToken)

		req := httptest.NewRequest(http.MethodGet, "/apps/site/provider-token/stripe", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("provider not configured returns 400", func(t *testing.T) {
		pt := &fakeProviderToken{err: service.ErrProviderNotConfigured}
		handler := NewAppHandler(&mockAppRepo{}, pt)
		router := gin.New()
		router.GET("/apps/:id/provider-token/:channel", handler.GetProviderToken)

		req := httptest.NewRequest(http.MethodGet, "/apps/site/provider-token/paypal", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("app inactive returns 403", func(t *testing.T) {
		pt := &fakeProviderToken{err: service.ErrAppInactive}
		handler := NewAppHandler(&mockAppRepo{}, pt)
		router := gin.New()
		router.GET("/apps/:id/provider-token/:channel", handler.GetProviderToken)

		req := httptest.NewRequest(http.MethodGet, "/apps/site/provider-token/paypal", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})

	t.Run("app not found returns 404", func(t *testing.T) {
		pt := &fakeProviderToken{err: service.ErrAppNotFound}
		handler := NewAppHandler(&mockAppRepo{}, pt)
		router := gin.New()
		router.GET("/apps/:id/provider-token/:channel", handler.GetProviderToken)

		req := httptest.NewRequest(http.MethodGet, "/apps/missing/provider-token/paypal", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("upstream error returns 502", func(t *testing.T) {
		pt := &fakeProviderToken{err: errors.New("upstream failed")}
		handler := NewAppHandler(&mockAppRepo{}, pt)
		router := gin.New()
		router.GET("/apps/:id/provider-token/:channel", handler.GetProviderToken)

		req := httptest.NewRequest(http.MethodGet, "/apps/site/provider-token/paypal", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", w.Code)
		}
	})
}

// --- PlanHandler.GetAppPlans tests ---

func TestPlanHandler_GetAppPlans(t *testing.T) {
	gin.SetMode(gin.TestMode)


	t.Run("app not found returns 404", func(t *testing.T) {
		appRepo := &mockAppRepo{findErr: sql.ErrNoRows}
		handler := NewPlanHandler(&mockPlanSvc{}, appRepo, nil)
		router := gin.New()
		router.GET("/apps/:id/plans", handler.GetAppPlans)

		req := httptest.NewRequest(http.MethodGet, "/apps/missing/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("app with no payment_providers returns plans with empty provider_ids", func(t *testing.T) {
		plans := []model.Plan{{ID: "free", Name: "Free", IsActive: true, Apps: pq.StringArray{"yundian"}}}
		appRepo := &mockAppRepo{apps: []model.App{{AppID: "yundian", IsActive: true}}}
		planSvc := &mockPlanSvc{plans: plans}
		handler := NewPlanHandler(planSvc, appRepo, nil)
		router := gin.New()
		router.GET("/apps/:id/plans", handler.GetAppPlans)

		req := httptest.NewRequest(http.MethodGet, "/apps/yundian/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var resp struct {
			Code int                `json:"code"`
			Data []model.PublicPlan `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Data) != 1 || len(resp.Data[0].ProviderIDs) != 0 {
			t.Errorf("expected 1 plan with empty provider_ids, got %+v", resp.Data)
		}
	})
}

// --- PostQuote handler tests ---

type fakeQuoteSvc struct {
	result *model.Quote
	err    error
}

func (f *fakeQuoteSvc) Get(ctx context.Context, appID, planID, userID string) (*model.Quote, error) {
	return f.result, f.err
}

func TestPlanHandler_PostQuote(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("happy path", func(t *testing.T) {
		appRepo := &mockAppRepo{apps: []model.App{{AppID: "yundian", IsActive: true}}}
		quote := &model.Quote{
			PlanID: "monthly", Amount: 29.9, Currency: "USD",
			SubExpiresAt: time.Now().Add(30 * 24 * time.Hour),
			CycleConfig:  model.CycleConfig{TrialDays: 0, BillingCycleDays: 30, Base: "now + trial + cycle"},
			ProviderData: map[string]any{"paypal": map[string]any{"plan_id": "P-1"}},
		}
		planSvc := &mockPlanSvc{}
		handler := NewPlanHandler(planSvc, appRepo, &fakeQuoteSvc{result: quote})
		router := gin.New()
		// Single registration with a middleware that simulates the JWT-auth
		// setting user_id in context (the real router mounts JWTAuth here).
		router.POST("/apps/:id/quote", func(c *gin.Context) {
			c.Set("user_id", "user-123")
			c.Next()
		}, handler.PostQuote)

		body := `{"plan_id":"monthly"}`
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int          `json:"code"`
			Data *model.Quote `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Data == nil || resp.Data.PlanID != "monthly" || resp.Data.Amount != 29.9 {
			t.Errorf("data = %+v", resp.Data)
		}
	})

	t.Run("invalid body returns 400", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{})
		router := gin.New()
		router.POST("/apps/:id/quote", handler.PostQuote)

		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString("not json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("missing plan_id returns 400", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{})
		router := gin.New()
		router.POST("/apps/:id/quote", handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("plan not found returns 404", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{err: service.ErrPlanNotFound})
		router := gin.New()
		// PostQuote defends on a non-empty user_id; simulate the JWT-auth
		// middleware that production mounts on this route.
		router.POST("/apps/:id/quote", func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "user-1")
			c.Next()
		}, handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{"plan_id":"missing"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("app inactive returns 403", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{err: service.ErrAppInactive})
		router := gin.New()
		router.POST("/apps/:id/quote", func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "user-1")
			c.Next()
		}, handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{"plan_id":"monthly"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})

	t.Run("plan inactive returns 400", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{err: service.ErrPlanInactive})
		router := gin.New()
		router.POST("/apps/:id/quote", func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "user-1")
			c.Next()
		}, handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{"plan_id":"monthly"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("app not found returns 404", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{err: service.ErrAppNotFound})
		router := gin.New()
		router.POST("/apps/:id/quote", func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "user-1")
			c.Next()
		}, handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{"plan_id":"monthly"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("plan/app mismatch returns 400", func(t *testing.T) {
		handler := NewPlanHandler(&mockPlanSvc{}, &mockAppRepo{}, &fakeQuoteSvc{err: service.ErrPlanAppMismatch})
		router := gin.New()
		router.POST("/apps/:id/quote", func(c *gin.Context) {
			c.Set(middleware.ContextUserID, "user-1")
			c.Next()
		}, handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{"plan_id":"monthly"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

// =========================================================================
// authErrReason — maps service-layer auth sentinels to URL-fragment-safe
// tokens. Used by the GitHub OAuth callback to encode failure reason in
// the redirect.
// =========================================================================

func TestAuthErrReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{service.ErrAppNotFound, "app_not_found"},
		{service.ErrAppInactive, "app_disabled"},
		{service.ErrUserNotFound, "user_not_found"},
		{service.ErrUserDeleted, "user_not_found"},
		{service.ErrUserSuspended, "user_suspended"},
		{service.ErrSubscriptionExpired, "subscription_expired"},
		{service.ErrSubscriptionNotActive, "subscription_expired"},
		{errors.New("some other error"), "auth_failed"},
		{nil, "auth_failed"},
	}
	for _, tc := range cases {
		if got := authErrReason(tc.err); got != tc.want {
			t.Errorf("authErrReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestAuthHandler_TestLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("mode not set → 404", func(t *testing.T) {
		prev := os.Getenv("PAYPAL_L3_E2E_MODE")
		os.Unsetenv("PAYPAL_L3_E2E_MODE")
		t.Cleanup(func() { os.Setenv("PAYPAL_L3_E2E_MODE", prev) })

		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)
		router := gin.New()
		router.POST("/test/login", handler.TestLogin)

		req := httptest.NewRequest(http.MethodPost, "/test/login",
			bytes.NewBufferString(`{"email":"x@y.com","app_id":"yundian"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("mode enabled + success → 200", func(t *testing.T) {
		prev := os.Getenv("PAYPAL_L3_E2E_MODE")
		os.Setenv("PAYPAL_L3_E2E_MODE", "1")
		t.Cleanup(func() { os.Setenv("PAYPAL_L3_E2E_MODE", prev) })

		authSvc := &mockAuthSvc{
			testLoginResp: &service.LoginResponse{
				AccessToken:  "at",
				RefreshToken: "rt",
			},
		}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)
		router := gin.New()
		router.POST("/test/login", handler.TestLogin)

		req := httptest.NewRequest(http.MethodPost, "/test/login",
			bytes.NewBufferString(`{"email":"x@y.com","app_id":"yundian"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("malformed body → 400", func(t *testing.T) {
		prev := os.Getenv("PAYPAL_L3_E2E_MODE")
		os.Setenv("PAYPAL_L3_E2E_MODE", "1")
		t.Cleanup(func() { os.Setenv("PAYPAL_L3_E2E_MODE", prev) })

		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)
		router := gin.New()
		router.POST("/test/login", handler.TestLogin)

		req := httptest.NewRequest(http.MethodPost, "/test/login",
			bytes.NewBufferString(`not-json`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("service error → 500", func(t *testing.T) {
		prev := os.Getenv("PAYPAL_L3_E2E_MODE")
		os.Setenv("PAYPAL_L3_E2E_MODE", "1")
		t.Cleanup(func() { os.Setenv("PAYPAL_L3_E2E_MODE", prev) })

		authSvc := &mockAuthSvc{testLoginErr: errors.New("service down")}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)
		router := gin.New()
		router.POST("/test/login", handler.TestLogin)

		req := httptest.NewRequest(http.MethodPost, "/test/login",
			bytes.NewBufferString(`{"email":"x@y.com","app_id":"yundian"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})

	t.Run("mode not 1 → 404", func(t *testing.T) {
		prev := os.Getenv("PAYPAL_L3_E2E_MODE")
		os.Setenv("PAYPAL_L3_E2E_MODE", "0")
		t.Cleanup(func() { os.Setenv("PAYPAL_L3_E2E_MODE", prev) })

		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)
		router := gin.New()
		router.POST("/test/login", handler.TestLogin)

		req := httptest.NewRequest(http.MethodPost, "/test/login",
			bytes.NewBufferString(`{"email":"x@y.com","app_id":"yundian"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})
}

func TestPlanHandler_GetAppPlans_FullCoverage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("app with paypal config returns cycle + provider_id", func(t *testing.T) {
		plans := []model.Plan{
			{ID: "free", Name: "Free", IsActive: true, Apps: pq.StringArray{"yundian"}, IsDefault: true},
			{ID: "monthly", Name: "Monthly", IsActive: true, Apps: pq.StringArray{"yundian"}, IntervalDays: 30, Price: 29.9},
		}
		app := model.App{
			AppID: "yundian", IsActive: true,
			Config: json.RawMessage(`{"payment_providers":{"paypal":{"client_id":"cid","plans":{"monthly":{"plan_id":"P-1","trial_days":7,"billing_cycle_days":31}}}}}`),
		}
		appRepo := &mockAppRepo{apps: []model.App{app}}
		planSvc := &mockPlanSvc{plans: plans}
		handler := NewPlanHandler(planSvc, appRepo, nil)
		router := gin.New()
		router.GET("/apps/:id/plans", handler.GetAppPlans)

		req := httptest.NewRequest(http.MethodGet, "/apps/yundian/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int                `json:"code"`
			Data []model.PublicPlan `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Data) != 2 {
			t.Fatalf("expected 2 plans, got %d", len(resp.Data))
		}
		// monthly plan should have paypal provider_id + cycle
		var monthly *model.PublicPlan
		for i := range resp.Data {
			if resp.Data[i].ID == "monthly" {
				monthly = &resp.Data[i]
				break
			}
		}
		if monthly == nil {
			t.Fatal("monthly plan not in response")
		}
		if monthly.ProviderIDs["paypal"] != "P-1" {
			t.Errorf("ProviderIDs[paypal] = %q, want P-1", monthly.ProviderIDs["paypal"])
		}
		if monthly.Cycle == nil {
			t.Fatal("expected cycle to be non-nil")
		}
		if monthly.Cycle.BillingCycleDays != 31 {
			t.Errorf("Cycle.BillingCycleDays = %d, want 31", monthly.Cycle.BillingCycleDays)
		}
	})

	t.Run("planSvc error returns 500", func(t *testing.T) {
		appRepo := &mockAppRepo{apps: []model.App{{AppID: "yundian", IsActive: true}}}
		// planSvc errors on FindByApp — handler must surface 500.
		planSvc := &mockPlanSvc{findByAppErr: errors.New("db down")}
		handler := NewPlanHandler(planSvc, appRepo, nil)
		router := gin.New()
		router.GET("/apps/:id/plans", handler.GetAppPlans)

		req := httptest.NewRequest(http.MethodGet, "/apps/yundian/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
	})

	t.Run("malformed app config returns 500", func(t *testing.T) {
		appRepo := &mockAppRepo{apps: []model.App{{AppID: "yundian", IsActive: true, Config: json.RawMessage(`not-json`)}}}
		planSvc := &mockPlanSvc{plans: nil}
		handler := NewPlanHandler(planSvc, appRepo, nil)
		router := gin.New()
		router.GET("/apps/:id/plans", handler.GetAppPlans)

		req := httptest.NewRequest(http.MethodGet, "/apps/yundian/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
	})

	t.Run("FindByID db error returns 500", func(t *testing.T) {
		appRepo := &mockAppRepo{findErr: errors.New("db down")}
		handler := NewPlanHandler(&mockPlanSvc{}, appRepo, nil)
		router := gin.New()
		router.GET("/apps/:id/plans", handler.GetAppPlans)

		req := httptest.NewRequest(http.MethodGet, "/apps/yundian/plans", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
	})
}
