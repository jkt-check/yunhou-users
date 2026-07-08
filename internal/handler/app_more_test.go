package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// --- additional subscription handler tests ---

func TestSubscriptionHandler_ListUserSubscriptions_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{subs: []model.Subscription{
		{ID: "s1", PlanID: "free", Status: "active"},
		{ID: "s2", PlanID: "monthly", Status: "cancelled"},
	}}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.GET("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.ListUserSubscriptions(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/user/subscriptions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"id":"s1"`) {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestSubscriptionHandler_ListUserSubscriptions_MissingAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.GET("/user/subscriptions", h.ListUserSubscriptions)

	req := httptest.NewRequest(http.MethodGet, "/user/subscriptions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := "invalid"
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_BadExpiresAt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := `{"plan_id":"free","expires_at":"not-rfc3339"}`
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_PlanNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{createErr: service.ErrPlanNotFound}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := `{"plan_id":"missing"}`
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_PlanInactive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{createErr: service.ErrPlanInactive}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := `{"plan_id":"inactive"}`
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_PaidPlanForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{createErr: service.ErrPaidPlanForbidden}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := `{"plan_id":"monthly"}`
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status=%d, want 403", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_Conflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{createErr: service.ErrUserHasActiveSub}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := `{"plan_id":"free"}`
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestSubscriptionHandler_CreateSubscription_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.POST("/user/subscriptions", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CreateSubscription(c)
	})
	body := `{"plan_id":"free"}`
	req := httptest.NewRequest(http.MethodPost, "/user/subscriptions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Errorf("status=%d, want 201", w.Code)
	}
}

func TestSubscriptionHandler_CancelSubscription_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{cancelErr: service.ErrSubscriptionNotFound}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.DELETE("/user/subscriptions/:id", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CancelSubscription(c)
	})
	req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/missing", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404", w.Code)
	}
}

func TestSubscriptionHandler_CancelSubscription_AlreadyCancelled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{cancelErr: service.ErrAlreadyCancelled}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.DELETE("/user/subscriptions/:id", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CancelSubscription(c)
	})
	req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/s1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestSubscriptionHandler_CancelSubscription_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.DELETE("/user/subscriptions/:id", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.CancelSubscription(c)
	})
	req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/s1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", w.Code)
	}
}

func TestSubscriptionHandler_CancelSubscription_MissingAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockSubSvc{}
	h := NewSubscriptionHandler(svc)
	r := gin.New()
	r.DELETE("/user/subscriptions/:id", h.CancelSubscription)
	req := httptest.NewRequest(http.MethodDelete, "/user/subscriptions/s1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
}

// --- additional plan handler tests ---

func TestPlanHandler_ListPlans_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{plans: []model.Plan{
		{ID: "free", Name: "Free", Price: 0, IsActive: true},
	}}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.GET("/admin/plans", h.ListPlans)
	req := httptest.NewRequest(http.MethodGet, "/admin/plans", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "free") {
		t.Errorf("body=%s", w.Body.String())
	}
}

func TestPlanHandler_GetPlan_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{plan: &model.Plan{ID: "free", Name: "Free"}}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.GET("/admin/plans/:id", h.GetPlan)
	req := httptest.NewRequest(http.MethodGet, "/admin/plans/free", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_CreatePlan_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.POST("/admin/plans", h.CreatePlan)
	req := httptest.NewRequest(http.MethodPost, "/admin/plans", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_CreatePlan_NegativePrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.POST("/admin/plans", h.CreatePlan)
	body := `{"id":"bad","name":"Bad","price":-1}`
	req := httptest.NewRequest(http.MethodPost, "/admin/plans", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_CreatePlan_NegativeInterval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.POST("/admin/plans", h.CreatePlan)
	body := `{"id":"bad","name":"Bad","interval_days":-1}`
	req := httptest.NewRequest(http.MethodPost, "/admin/plans", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_CreatePlan_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.POST("/admin/plans", h.CreatePlan)
	body := `{"id":"new","name":"New","price":1.0,"interval_days":30}`
	req := httptest.NewRequest(http.MethodPost, "/admin/plans", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_UpdatePlan_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.PATCH("/admin/plans/:id", h.UpdatePlan)
	req := httptest.NewRequest(http.MethodPatch, "/admin/plans/free", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_UpdatePlan_NegativePrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{plan: &model.Plan{ID: "free", Name: "Free", IsActive: true}}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.PATCH("/admin/plans/:id", h.UpdatePlan)
	body := `{"price":-1}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/plans/free", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestPlanHandler_DeletePlan_FKViolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{deleteErr: newFKViolation()}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.DELETE("/admin/plans/:id", h.DeletePlan)
	req := httptest.NewRequest(http.MethodDelete, "/admin/plans/in-use", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
}

func TestPlanHandler_DeletePlan_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockPlanSvc{}
	h := NewPlanHandler(svc, nil, nil)
	r := gin.New()
	r.DELETE("/admin/plans/:id", h.DeletePlan)
	req := httptest.NewRequest(http.MethodDelete, "/admin/plans/free", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d", w.Code)
	}
}

// newFKViolation returns a real *pq.Error whose Code() == "23503".
func newFKViolation() error {
	return &pq.Error{Code: "23503"}
}

// --- additional app handler tests ---

func TestAppHandler_UpdateApp_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewMockAppRepo()
	r.apps["yundian"] = &model.App{AppID: "yundian", Name: "Yundian", IsActive: true}
	h := NewAppHandler(r, nil)
	g := gin.New()
	g.PATCH("/admin/apps/:id", h.UpdateApp)
	req := httptest.NewRequest(http.MethodPatch, "/admin/apps/yundian", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestAppHandler_UpdateApp_EmptyName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := NewMockAppRepo()
	r.apps["yundian"] = &model.App{AppID: "yundian", Name: "Yundian", IsActive: true}
	h := NewAppHandler(r, nil)
	g := gin.New()
	g.PATCH("/admin/apps/:id", h.UpdateApp)
	body := `{"name":"   "}`
	req := httptest.NewRequest(http.MethodPatch, "/admin/apps/yundian", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

// MockAppRepo is a simple hand-rolled mock for AppRepo tests.
type MockAppRepo struct {
	apps map[string]*model.App
}

func NewMockAppRepo() *MockAppRepo {
	return &MockAppRepo{apps: make(map[string]*model.App)}
}

func (m *MockAppRepo) List(_ context.Context) ([]model.App, error) {
	out := make([]model.App, 0, len(m.apps))
	for _, a := range m.apps {
		out = append(out, *a)
	}
	return out, nil
}

func (m *MockAppRepo) ListUnhashed(_ context.Context) ([]model.App, error) {
	out := make([]model.App, 0, len(m.apps))
	for _, a := range m.apps {
		if a.SecretHash == "" {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (m *MockAppRepo) FindByID(_ context.Context, id string) (*model.App, error) {
	a, ok := m.apps[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return a, nil
}

func (m *MockAppRepo) Create(_ context.Context, a *model.App) error {
	m.apps[a.AppID] = a
	return nil
}

func (m *MockAppRepo) Update(_ context.Context, a *model.App) error {
	m.apps[a.AppID] = a
	return nil
}

func (m *MockAppRepo) RotateSecretHash(_ context.Context, appID, newHash string) error {
	a, ok := m.apps[appID]
	if !ok {
		return errors.New("not found")
	}
	a.SecretHash = newHash
	return nil
}

func (m *MockAppRepo) BackfillSecretHash(_ context.Context, appID, newHash string) (bool, error) {
	a, ok := m.apps[appID]
	if !ok {
		return false, errors.New("not found")
	}
	if a.SecretHash != "" {
		return true, nil // mimic production guard
	}
	a.SecretHash = newHash
	return false, nil
}

// --- additional auth handler tests ---

func TestAuthHandler_Login_UnsupportedProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockAuthSvc{loginErr: service.ErrUnsupportedProvider}
	h := NewAuthHandler(svc, &mockTokenSvc{})
	r := gin.New()
	r.POST("/auth/login", h.Login)
	body := `{"provider":"facebook","provider_token":"x","app_id":"yundian"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestAuthHandler_Login_SuspendedUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockAuthSvc{loginErr: service.ErrUserSuspended}
	h := NewAuthHandler(svc, &mockTokenSvc{})
	r := gin.New()
	r.POST("/auth/login", h.Login)
	body := `{"provider":"google","provider_token":"x","app_id":"yundian"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
}

func TestAuthHandler_Login_InternalServerError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockAuthSvc{loginErr: errors.New("db boom")}
	h := NewAuthHandler(svc, &mockTokenSvc{})
	r := gin.New()
	r.POST("/auth/login", h.Login)
	body := `{"provider":"google","provider_token":"x","app_id":"yundian"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestAuthHandler_Refresh_InvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockAuthSvc{refreshErr: service.ErrInvalidRefreshToken}
	h := NewAuthHandler(svc, &mockTokenSvc{})
	r := gin.New()
	r.POST("/auth/refresh", h.RefreshToken)
	body := `{"refresh_token":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
}

func TestAuthHandler_Refresh_InternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockAuthSvc{refreshErr: errors.New("db boom")}
	h := NewAuthHandler(svc, &mockTokenSvc{})
	r := gin.New()
	r.POST("/auth/refresh", h.RefreshToken)
	body := `{"refresh_token":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestAuthHandler_Logout_InternalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &mockAuthSvc{logoutErr: errors.New("db boom")}
	h := NewAuthHandler(svc, &mockTokenSvc{})
	r := gin.New()
	r.POST("/auth/logout", h.Logout)
	body := `{"refresh_token":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

// --- additional user handler tests ---

func TestUserHandler_GetProfile_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{findErr: sql.ErrNoRows}
	r := &mockIdentityRepo{}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.GET("/user/profile", func(c *gin.Context) {
		c.Set("user_id", "u-missing")
		h.GetProfile(c)
	})
	req := httptest.NewRequest(http.MethodGet, "/user/profile", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d", w.Code)
	}
}

func TestUserHandler_UpdateProfile_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{user: &model.User{ID: "u1", Status: "active"}}
	r := &mockIdentityRepo{}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.PATCH("/user/profile", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.UpdateProfile(c)
	})
	req := httptest.NewRequest(http.MethodPatch, "/user/profile", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d", w.Code)
	}
}

func TestUserHandler_UpdateProfile_BadNickname(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{user: &model.User{ID: "u1", Status: "active"}}
	r := &mockIdentityRepo{}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.PATCH("/user/profile", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.UpdateProfile(c)
	})
	cases := []string{
		`{"nickname":""}`,
		`{"nickname":"` + strings.Repeat("x", 101) + `"}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPatch, "/user/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status=%d, want 400", body, w.Code)
		}
	}
}

func TestUserHandler_UpdateProfile_BadAvatar(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{user: &model.User{ID: "u1", Status: "active"}}
	r := &mockIdentityRepo{}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.PATCH("/user/profile", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.UpdateProfile(c)
	})
	cases := []string{
		`{"avatar_url":"http://example.com/x.png"}`,   // not https
		`{"avatar_url":"https://x:y@example.com"}`,     // userinfo
		`{"avatar_url":"https://example.com/x#frag"}`,  // fragment
		`{"avatar_url":"not a url at all"}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPatch, "/user/profile", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body=%s: status=%d, want 400", body, w.Code)
		}
	}
}

func TestUserHandler_UpdateProfile_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{user: &model.User{ID: "u1", Status: "active"}}
	r := &mockIdentityRepo{}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.PATCH("/user/profile", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.UpdateProfile(c)
	})
	body := `{"nickname":"alice","avatar_url":"https://example.com/a.png"}`
	req := httptest.NewRequest(http.MethodPatch, "/user/profile", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, body=%s", w.Code, w.Body.String())
	}
}

func TestUserHandler_ListIdentities_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{}
	r := &mockIdentityRepo{identities: []model.SocialIdentity{
		{ID: "id-1", Provider: "github"},
	}}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.GET("/user/identities", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.ListIdentities(c)
	})
	req := httptest.NewRequest(http.MethodGet, "/user/identities", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d", w.Code)
	}
}

func TestUserHandler_UnbindIdentity_TooFew(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{}
	r := &mockIdentityRepo{deleteResult: false}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.DELETE("/user/identities/:id", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.UnbindIdentity(c)
	})
	req := httptest.NewRequest(http.MethodDelete, "/user/identities/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", w.Code)
	}
}

func TestUserHandler_UnbindIdentity_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &mockUserRepo{}
	r := &mockIdentityRepo{deleteResult: true}
	h := NewUserHandler(u, r)
	g := gin.New()
	g.DELETE("/user/identities/:id", func(c *gin.Context) {
		c.Set("user_id", "u1")
		h.UnbindIdentity(c)
	})
	req := httptest.NewRequest(http.MethodDelete, "/user/identities/x", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d", w.Code)
	}
}

// --- mocks needed for user handler tests ---
// (mockUserRepo and mockIdentityRepo are defined in handler_test.go; we
// only consume them here.)