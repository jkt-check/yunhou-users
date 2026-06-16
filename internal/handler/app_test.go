package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

// ---------- app handler test setup ----------

// setupAppRouter creates a gin.Engine with app routes and a middleware that sets the "app" context value.
func setupAppRouter(h *AppHandler, authedApp *model.App) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if authedApp != nil {
			c.Set("app", authedApp)
		}
		c.Next()
	})
	r.POST("/apps", h.CreateApp)
	r.GET("/apps/:id", h.GetApp)
	r.PATCH("/apps/:id", h.UpdateApp)
	r.POST("/subscriptions", h.CreateSubscription)
	r.GET("/subscriptions/:id", h.GetSubscription)
	r.POST("/subscriptions/:id/cancel", h.CancelSubscription)
	return r
}

// ---------- CreateApp tests ----------

func TestCreateApp_MissingName(t *testing.T) {
	t.Parallel()

	h := NewAppHandler(&mockAppRepo{}, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	// Missing name (required field)
	w := performRequest(r, http.MethodPost, "/apps", `{"redirect_uris":["http://localhost/cb"]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid request body") {
		t.Errorf("body = %s, want containing 'invalid request body'", w.Body.String())
	}

	// Missing redirect_uris (required field)
	w = performRequest(r, http.MethodPost, "/apps", `{"name":"myapp"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestCreateApp_RepoError(t *testing.T) {
	t.Parallel()

	appRepo := &mockAppRepo{
		createFn: func(ctx context.Context, a *model.App) error {
			return errors.New("db error")
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"name":"myapp","redirect_uris":["http://localhost/cb"]}`
	w := performRequest(r, http.MethodPost, "/apps", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to create app") {
		t.Errorf("body = %s, want containing 'failed to create app'", w.Body.String())
	}
}

func TestCreateApp_Success(t *testing.T) {
	t.Parallel()

	var createdApp *model.App
	appRepo := &mockAppRepo{
		createFn: func(ctx context.Context, a *model.App) error {
			createdApp = a
			return nil
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"name":"myapp","redirect_uris":["http://localhost/cb"],"providers":["github"],"default_plan":"paid"}`
	w := performRequest(r, http.MethodPost, "/apps", body)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["app_id"] == nil || data["app_id"] == "" {
		t.Error("expected app_id in response")
	}
	if data["app_secret"] == nil || data["app_secret"] == "" {
		t.Error("expected app_secret in response")
	}
	if data["name"] != "myapp" {
		t.Errorf("name = %v, want myapp", data["name"])
	}
	if createdApp == nil {
		t.Fatal("app was not created")
	}
	if createdApp.ID == "" {
		t.Error("created app has no ID")
	}
	if createdApp.Secret == "" {
		t.Error("created app has no Secret")
	}
	if createdApp.Name != "myapp" {
		t.Errorf("created app Name = %q, want myapp", createdApp.Name)
	}
	if createdApp.DefaultPlan != "paid" {
		t.Errorf("created app DefaultPlan = %q, want paid", createdApp.DefaultPlan)
	}
}

func TestCreateApp_DefaultValues(t *testing.T) {
	t.Parallel()

	var createdApp *model.App
	appRepo := &mockAppRepo{
		createFn: func(ctx context.Context, a *model.App) error {
			createdApp = a
			return nil
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	// No providers or default_plan — should get defaults
	body := `{"name":"myapp","redirect_uris":["http://localhost/cb"]}`
	w := performRequest(r, http.MethodPost, "/apps", body)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	if createdApp == nil {
		t.Fatal("app was not created")
	}
	if len(createdApp.Providers) != 3 || createdApp.Providers[0] != "github" {
		t.Errorf("default Providers = %v, want [github, google, wechat]", createdApp.Providers)
	}
	if createdApp.DefaultPlan != "free" {
		t.Errorf("default DefaultPlan = %q, want free", createdApp.DefaultPlan)
	}
}

// ---------- GetApp tests ----------

func TestGetApp_Found(t *testing.T) {
	t.Parallel()

	appRepo := &mockAppRepo{
		findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: id, Name: "testapp"}, nil
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodGet, "/apps/app1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["id"] != "app1" {
		t.Errorf("data.id = %v, want app1", data["id"])
	}
}

func TestGetApp_NotFound(t *testing.T) {
	t.Parallel()

	appRepo := &mockAppRepo{
		findFn: func(ctx context.Context, id string) (*model.App, error) {
			return nil, errNotFound
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodGet, "/apps/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "app not found") {
		t.Errorf("body = %s, want containing 'app not found'", w.Body.String())
	}
}

// ---------- UpdateApp tests ----------

func TestUpdateApp_Found(t *testing.T) {
	t.Parallel()

	var updatedApp *model.App
	appRepo := &mockAppRepo{
		findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{
				ID:          id,
				Name:        "oldname",
				Providers:  []string{"github"},
				DefaultPlan: "free",
			}, nil
		},
		updateFn: func(ctx context.Context, a *model.App) error {
			updatedApp = a
			return nil
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"name":"newname","default_plan":"paid"}`
	w := performRequest(r, http.MethodPatch, "/apps/app1", body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if updatedApp == nil {
		t.Fatal("app was not updated")
	}
	if updatedApp.Name != "newname" {
		t.Errorf("updated Name = %q, want newname", updatedApp.Name)
	}
	if updatedApp.DefaultPlan != "paid" {
		t.Errorf("updated DefaultPlan = %q, want paid", updatedApp.DefaultPlan)
	}
}

func TestUpdateApp_PartialUpdate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		body             string
		wantName         string
		wantPlan         string
		wantProvidersLen int
		wantRedirectLen  int
	}{
		{
			name:             "update providers only",
			body:             `{"providers":["github","google"]}`,
			wantName:         "oldname",
			wantPlan:         "free",
			wantProvidersLen: 2,
			wantRedirectLen:  0,
		},
		{
			name:             "update redirect_uris only",
			body:             `{"redirect_uris":["http://localhost/cb","http://localhost/cb2"]}`,
			wantName:         "oldname",
			wantPlan:         "free",
			wantProvidersLen: 1,
			wantRedirectLen:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var updatedApp *model.App
			appRepo := &mockAppRepo{
				findFn: func(ctx context.Context, id string) (*model.App, error) {
					return &model.App{
						ID:          id,
						Name:        "oldname",
						Providers:   []string{"github"},
						DefaultPlan: "free",
					}, nil
				},
				updateFn: func(ctx context.Context, a *model.App) error {
					updatedApp = a
					return nil
				},
			}
			h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
			r := setupAppRouter(h, &model.App{ID: "app1"})

			w := performRequest(r, http.MethodPatch, "/apps/app1", tt.body)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
			}

			if updatedApp == nil {
				t.Fatal("app was not updated")
			}
			if updatedApp.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", updatedApp.Name, tt.wantName)
			}
			if updatedApp.DefaultPlan != tt.wantPlan {
				t.Errorf("DefaultPlan = %q, want %q", updatedApp.DefaultPlan, tt.wantPlan)
			}
			if len(updatedApp.Providers) != tt.wantProvidersLen {
				t.Errorf("Providers len = %d, want %d", len(updatedApp.Providers), tt.wantProvidersLen)
			}
			if len(updatedApp.RedirectURIs) != tt.wantRedirectLen {
				t.Errorf("RedirectURIs len = %d, want %d", len(updatedApp.RedirectURIs), tt.wantRedirectLen)
			}
		})
	}
}

func TestUpdateApp_NotFound(t *testing.T) {
	t.Parallel()

	appRepo := &mockAppRepo{
		findFn: func(ctx context.Context, id string) (*model.App, error) {
			return nil, errNotFound
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"name":"newname"}`
	w := performRequest(r, http.MethodPatch, "/apps/nonexistent", body)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestUpdateApp_InvalidBody(t *testing.T) {
	t.Parallel()

	appRepo := &mockAppRepo{
		findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: id, Name: "oldname"}, nil
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodPatch, "/apps/app1", "not-json")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid request body") {
		t.Errorf("body = %s, want containing 'invalid request body'", w.Body.String())
	}
}

func TestUpdateApp_UpdateError(t *testing.T) {
	appRepo := &mockAppRepo{
		findFn: func(ctx context.Context, id string) (*model.App, error) {
			return &model.App{ID: id, Name: "oldname"}, nil
		},
		updateFn: func(ctx context.Context, a *model.App) error {
			return errors.New("db error")
		},
	}
	h := NewAppHandler(appRepo, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"name":"newname"}`
	w := performRequest(r, http.MethodPatch, "/apps/app1", body)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to update app") {
		t.Errorf("body = %s, want containing 'failed to update app'", w.Body.String())
	}
}

// ---------- CreateSubscription tests ----------

func TestCreateSubscription_InvalidBody(t *testing.T) {
	t.Parallel()

	h := NewAppHandler(&mockAppRepo{}, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	// Empty body
	w := performRequest(r, http.MethodPost, "/subscriptions", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	// Missing required fields
	w = performRequest(r, http.MethodPost, "/subscriptions", `{"user_id":"u1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestCreateSubscription_InvalidExpiresAt(t *testing.T) {
	t.Parallel()

	h := NewAppHandler(&mockAppRepo{}, &mockSubscriptionRepo{}, service.NewSubscriptionService(&mockSubscriptionRepo{}))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"user_id":"u1","app_id":"app1","plan":"pro","expires_at":"not-a-date"}`
	w := performRequest(r, http.MethodPost, "/subscriptions", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid expires_at format") {
		t.Errorf("body = %s, want containing 'invalid expires_at format'", w.Body.String())
	}
}

func TestCreateSubscription_Duplicate(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		createFn: func(ctx context.Context, s *model.Subscription) error {
			return &duplicateKeyError{}
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"user_id":"u1","app_id":"app1","plan":"pro"}`
	w := performRequest(r, http.MethodPost, "/subscriptions", body)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "subscription already exists") {		t.Errorf("body = %s, want containing 'subscription already exists'", w.Body.String())
	}
}

func TestCreateSubscription_Success(t *testing.T) {
	t.Parallel()

	var createdSub *model.Subscription
	subRepo := &mockSubscriptionRepo{
		createFn: func(ctx context.Context, s *model.Subscription) error {
			createdSub = s
			return nil
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	body := `{"user_id":"u1","app_id":"app1","plan":"pro"}`
	w := performRequest(r, http.MethodPost, "/subscriptions", body)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["id"] == nil || data["id"] == "" {
		t.Error("expected id in response data")
	}
	if createdSub == nil {
		t.Fatal("subscription was not created")
	}
	if createdSub.Plan != "pro" {
		t.Errorf("Plan = %q, want paid", createdSub.Plan)
	}
	if createdSub.Status != "active" {
		t.Errorf("Status = %q, want active", createdSub.Status)
	}
}

func TestCreateSubscription_WithExpiresAt(t *testing.T) {
	t.Parallel()

	var createdSub *model.Subscription
	subRepo := &mockSubscriptionRepo{
		createFn: func(ctx context.Context, s *model.Subscription) error {
			createdSub = s
			return nil
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	expiresAt := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	body := `{"user_id":"u1","app_id":"app1","plan":"pro","expires_at":"` + expiresAt + `"}`
	w := performRequest(r, http.MethodPost, "/subscriptions", body)
	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	if createdSub == nil {
		t.Fatal("subscription was not created")
	}
	if createdSub.ExpiresAt == nil {
		t.Error("expected ExpiresAt to be set")
	}
}

// ---------- GetSubscription tests ----------

func TestGetSubscription_Found(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		findFn: func(ctx context.Context, id string) (*model.Subscription, error) {
			return &model.Subscription{ID: id, UserID: "u1", AppID: "app1", Plan: "pro", Status: "active"}, nil
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodGet, "/subscriptions/sub1", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["id"] != "sub1" {
		t.Errorf("data.id = %v, want sub1", data["id"])
	}
}

func TestGetSubscription_NotFound(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		findFn: func(ctx context.Context, id string) (*model.Subscription, error) {
			return nil, errNotFound
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodGet, "/subscriptions/nonexistent", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "subscription not found") {
		t.Errorf("body = %s, want containing 'subscription not found'", w.Body.String())
	}
}

// ---------- CancelSubscription tests ----------

func TestCancelSubscription_Found(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		findFn: func(ctx context.Context, id string) (*model.Subscription, error) {
			return &model.Subscription{ID: id, AppID: "app1", Status: "active"}, nil
		},
		updateStatusFn: func(ctx context.Context, id, status string) error {
			return nil
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodPost, "/subscriptions/sub1/cancel", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cancelled") {
		t.Errorf("body = %s, want containing 'cancelled'", w.Body.String())
	}
}

func TestCancelSubscription_AlreadyCancelled(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		findFn: func(ctx context.Context, id string) (*model.Subscription, error) {
			return &model.Subscription{ID: id, AppID: "app1", Status: "cancelled"}, nil
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodPost, "/subscriptions/sub1/cancel", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already cancelled") {
		t.Errorf("body = %s, want containing 'already cancelled'", w.Body.String())
	}
}

func TestCancelSubscription_NotFound(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		findFn: func(ctx context.Context, id string) (*model.Subscription, error) {
			return nil, errNotFound
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodPost, "/subscriptions/nonexistent/cancel", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "subscription not found") {
		t.Errorf("body = %s, want containing 'subscription not found'", w.Body.String())
	}
}

func TestCancelSubscription_UpdateStatusError(t *testing.T) {
	subRepo := &mockSubscriptionRepo{
		findFn: func(ctx context.Context, id string) (*model.Subscription, error) {
			return &model.Subscription{ID: id, AppID: "app1", Status: "active"}, nil
		},
		updateStatusFn: func(ctx context.Context, id, status string) error {
			return errors.New("db error")
		},
	}
	h := NewAppHandler(&mockAppRepo{}, subRepo, service.NewSubscriptionService(subRepo))
	r := setupAppRouter(h, &model.App{ID: "app1"})

	w := performRequest(r, http.MethodPost, "/subscriptions/sub1/cancel", "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

// ---------- compile-time check ----------

var _ repo.SubscriptionRepo = (*mockSubscriptionRepo)(nil)
