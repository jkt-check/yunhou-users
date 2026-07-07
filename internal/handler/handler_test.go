package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
	"github.com/yunhou/users/internal/util"
)

// --- Mocks implementing service interfaces ---

type mockAuthSvc struct {
	loginResp    *service.LoginResponse
	loginErr     error
	logoutErr    error
	refreshResp  *service.LoginResponse
	refreshErr   error
}

func (m *mockAuthSvc) Login(ctx context.Context, req service.LoginRequest) (*service.LoginResponse, error) {
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
	plans     []model.Plan
	plan      *model.Plan
	getErr    error
	createErr error
	updateErr error
	deleteErr error
}

func (m *mockPlanSvc) ListPlans(ctx context.Context) ([]model.Plan, error) {
	return m.plans, nil
}

func (m *mockPlanSvc) FindByApp(ctx context.Context, appID string) ([]model.Plan, error) {
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
	return m.plan, nil
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

func TestAuthHandler_Login(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("login success", func(t *testing.T) {
		nickname := "testuser"
		resp := &service.LoginResponse{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			User:         service.UserInfo{ID: "user-123", Nickname: &nickname},
			Subscription: &service.SubscriptionInfo{PlanID: "free", HasAccess: true},
		}
		authSvc := &mockAuthSvc{loginResp: resp}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/login", handler.Login)

		body := `{"provider":"github","provider_token":"tok","app_id":"yundian"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("login invalid body", func(t *testing.T) {
		authSvc := &mockAuthSvc{}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/login", handler.Login)

		body := `invalid json`
		req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("login unauthorized", func(t *testing.T) {
		authSvc := &mockAuthSvc{loginErr: service.ErrInvalidProviderToken}
		tokenSvc := &mockTokenSvc{}
		handler := NewAuthHandler(authSvc, tokenSvc)

		router := gin.New()
		router.POST("/auth/login", handler.Login)

		body := `{"provider":"github","provider_token":"bad","app_id":"yundian"}`
		req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})
}

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

		body := `{"config":{"payment_providers":{"lemonsqueezy":{"api_key":"k","store_id":"s"}}}}`
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
		if !bytes.Contains(updated.Config, []byte(`"lemonsqueezy"`)) {
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

	t.Run("happy path with paypal + ls configured", func(t *testing.T) {
		plans := []model.Plan{
			{ID: "free", Name: "Free", Price: 0, IntervalDays: 0, Apps: pq.StringArray{"yundian"}, IsActive: true, IsDefault: true},
			{ID: "monthly", Name: "Monthly", Price: 29.9, IntervalDays: 30, Apps: pq.StringArray{"yundian"}, IsActive: true},
		}
		app := model.App{
			AppID:    "yundian",
			Name:     "Yundian",
			IsActive: true,
			Config: json.RawMessage(`{
				"brand": {"name": "Yundian Brand"},
				"payment_providers": {
					"paypal": {"plans": {"monthly": {"plan_id": "P-1", "trial_days": 7, "billing_cycle_days": 30}}},
					"lemonsqueezy": {"plans": {"monthly": {"variant_id": "var-1", "trial_days": 0, "billing_cycle_days": 30}}}
				}
			}`),
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
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int                `json:"code"`
			Data []model.PublicPlan `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Data) != 2 {
			t.Fatalf("got %d plans, want 2", len(resp.Data))
		}
		// free plan: no provider entries, no cycle
		if resp.Data[0].ID != "free" {
			t.Errorf("first plan id = %q, want free", resp.Data[0].ID)
		}
		if len(resp.Data[0].ProviderIDs) != 0 {
			t.Errorf("free plan provider_ids = %+v, want empty", resp.Data[0].ProviderIDs)
		}
		// monthly plan: both channels populated, cycle from paypal (precedence)
		if resp.Data[1].ID != "monthly" {
			t.Errorf("second plan id = %q, want monthly", resp.Data[1].ID)
		}
		ppID := resp.Data[1].ProviderIDs["paypal"]
		if ppID != "P-1" {
			t.Errorf("monthly.paypal = %q, want P-1", ppID)
		}
		lsID := resp.Data[1].ProviderIDs["lemonsqueezy"]
		if lsID != "var-1" {
			t.Errorf("monthly.lemonsqueezy = %q, want var-1", lsID)
		}
		if resp.Data[1].Cycle == nil || resp.Data[1].Cycle.TrialDays != 7 || resp.Data[1].Cycle.BillingCycleDays != 30 {
			t.Errorf("monthly.cycle = %+v, want {7, 30}", resp.Data[1].Cycle)
		}
	})

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
		router.POST("/apps/:id/quote", handler.PostQuote)
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
		router.POST("/apps/:id/quote", handler.PostQuote)
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
		router.POST("/apps/:id/quote", handler.PostQuote)
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
		router.POST("/apps/:id/quote", handler.PostQuote)
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
		router.POST("/apps/:id/quote", handler.PostQuote)
		req := httptest.NewRequest(http.MethodPost, "/apps/yundian/quote", bytes.NewBufferString(`{"plan_id":"monthly"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}
