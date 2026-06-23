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
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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
		handler := NewPlanHandler(planSvc)

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

func TestAppHandler_ListApps(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("list apps success", func(t *testing.T) {
		apps := []model.App{
			{AppID: "yundian", Name: "Yundian"},
			{AppID: "yundash", Name: "Yundash"},
		}
		appRepo := &mockAppRepo{apps: apps}
		handler := NewAppHandler(appRepo)

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
		handler := NewAppHandler(appRepo)

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
		handler := NewAppHandler(appRepo)

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
		handler := NewAppHandler(appRepo)

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
		handler := NewAppHandler(appRepo)

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
	})

	t.Run("create app invalid body", func(t *testing.T) {
		appRepo := &mockAppRepo{}
		handler := NewAppHandler(appRepo)

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
}

func TestAppHandler_UpdateApp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("update app success", func(t *testing.T) {
		apps := []model.App{{AppID: "test", Name: "Old Name"}}
		appRepo := &mockAppRepo{apps: apps}
		handler := NewAppHandler(appRepo)

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
		handler := NewAppHandler(appRepo)

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
