package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/util"
)

// ---------- mock AppRepo ----------

type mockAppRepo struct {
	findFn func(ctx context.Context, id string) (*model.App, error)
}

func (m *mockAppRepo) Create(_ context.Context, _ *model.App) error { return nil }
func (m *mockAppRepo) Update(_ context.Context, _ *model.App) error { return nil }
func (m *mockAppRepo) FindByID(ctx context.Context, id string) (*model.App, error) {
	if m.findFn != nil {
		return m.findFn(ctx, id)
	}
	return nil, errors.New("not found")
}

// compile-time check
var _ repo.AppRepo = (*mockAppRepo)(nil)

// ---------- tests ----------

func TestAppAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Pre-hash a known secret so we can use it in test cases.
	secret := "test-secret-123"
	hashed, err := util.HashSecret(secret)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}

	tests := []struct {
		name       string
		method     string
		headers    map[string]string
		form       map[string]string
		findFn     func(ctx context.Context, id string) (*model.App, error)
		wantStatus int
		wantCode   float64
		wantMsg    string
	}{
		{
			name:       "missing app_id and app_secret in headers and form",
			method:     http.MethodGet,
			headers:    map[string]string{},
			findFn:     nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "missing app_id or app_secret",
		},
		{
			name:   "missing app_id only in headers",
			method: http.MethodGet,
			headers: map[string]string{
				"X-App-Secret": secret,
			},
			findFn:     nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "missing app_id or app_secret",
		},
		{
			name:   "missing app_secret only in headers",
			method: http.MethodGet,
			headers: map[string]string{
				"X-App-ID": "app-1",
			},
			findFn:     nil,
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "missing app_id or app_secret",
		},
		{
			name:       "valid headers but invalid app_id",
			method:     http.MethodGet,
			headers:    map[string]string{"X-App-ID": "bad-app", "X-App-Secret": secret},
			findFn:     func(_ context.Context, id string) (*model.App, error) { return nil, errors.New("not found") },
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "invalid app_id",
		},
		{
			name:       "valid app_id but invalid app_secret",
			method:     http.MethodGet,
			headers:    map[string]string{"X-App-ID": "app-1", "X-App-Secret": "wrong-secret"},
			findFn:     func(_ context.Context, id string) (*model.App, error) { return &model.App{ID: id, Secret: hashed}, nil },
			wantStatus: http.StatusUnauthorized,
			wantCode:   401,
			wantMsg:    "invalid app_secret",
		},
		{
			name:       "valid headers with correct app_id and app_secret",
			method:     http.MethodGet,
			headers:    map[string]string{"X-App-ID": "app-1", "X-App-Secret": secret},
			findFn:     func(_ context.Context, id string) (*model.App, error) { return &model.App{ID: id, Secret: hashed}, nil },
			wantStatus: http.StatusOK,
			wantCode:   0,
			wantMsg:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &mockAppRepo{findFn: tt.findFn}

			r := gin.New()
			r.Use(AppAuth(repo))
			r.Any("/", func(c *gin.Context) {
				app, exists := c.Get("app")
				if !exists {
					c.Status(http.StatusInternalServerError)
					return
				}
				c.JSON(http.StatusOK, gin.H{"app_id": app.(*model.App).ID})
			})

			var req *http.Request
			if tt.method == http.MethodPost && len(tt.form) > 0 {
				form := url.Values{}
				for k, v := range tt.form {
					form.Set(k, v)
				}
				req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			} else {
				req = httptest.NewRequest(tt.method, "/", nil)
			}

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}

			if tt.wantMsg != "" {
				var body map[string]interface{}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if body["code"] != tt.wantCode {
					t.Errorf("code: got %v, want %v", body["code"], tt.wantCode)
				}
				if body["message"] != tt.wantMsg {
					t.Errorf("message: got %v, want %v", body["message"], tt.wantMsg)
				}
			}
		})
	}
}

func TestAppAuth_FormCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()

	secret := "form-secret"
	hashed, err := util.HashSecret(secret)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}

	repo := &mockAppRepo{
		findFn: func(_ context.Context, id string) (*model.App, error) {
			return &model.App{ID: id, Secret: hashed}, nil
		},
	}

	var gotAppID string
	r := gin.New()
	r.Use(AppAuth(repo))
	r.POST("/", func(c *gin.Context) {
		app, _ := c.Get("app")
		gotAppID = app.(*model.App).ID
		c.Status(http.StatusOK)
	})

	form := url.Values{}
	form.Set("app_id", "app-form")
	form.Set("app_secret", secret)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gotAppID != "app-form" {
		t.Errorf("app_id from context: got %q, want %q", gotAppID, "app-form")
	}
}

func TestAppAuth_FormFallbackWhenHeadersEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()

	secret := "fallback-secret"
	hashed, err := util.HashSecret(secret)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}

	repo := &mockAppRepo{
		findFn: func(_ context.Context, id string) (*model.App, error) {
			if id == "app-fb" {
				return &model.App{ID: id, Secret: hashed}, nil
			}
			return nil, errors.New("not found")
		},
	}

	r := gin.New()
	r.Use(AppAuth(repo))
	r.POST("/", func(c *gin.Context) {
		app, exists := c.Get("app")
		if !exists {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.JSON(http.StatusOK, gin.H{"app_id": app.(*model.App).ID})
	})

	form := url.Values{}
	form.Set("app_id", "app-fb")
	form.Set("app_secret", secret)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestAppAuth_ContextSetsApp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()

	secret := "ctx-secret"
	hashed, err := util.HashSecret(secret)
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}

	repo := &mockAppRepo{
		findFn: func(_ context.Context, id string) (*model.App, error) {
			return &model.App{ID: id, Secret: hashed, Name: "TestApp"}, nil
		},
	}

	var gotApp *model.App
	r := gin.New()
	r.Use(AppAuth(repo))
	r.GET("/", func(c *gin.Context) {
		val, _ := c.Get("app")
		gotApp = val.(*model.App)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-App-ID", "app-ctx")
	req.Header.Set("X-App-Secret", secret)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	if gotApp == nil {
		t.Fatal("app in context is nil")
	}
	if gotApp.ID != "app-ctx" {
		t.Errorf("app.ID: got %q, want %q", gotApp.ID, "app-ctx")
	}
	if gotApp.Name != "TestApp" {
		t.Errorf("app.Name: got %q, want %q", gotApp.Name, "TestApp")
	}
}
