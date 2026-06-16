package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

// ---------- user handler mock repos (reuse the ones from auth_test.go) ----------

// setupUserRouter creates a gin.Engine with user routes and middleware to set user_id.
func setupUserRouter(h *UserHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Set user_id from query parameter for testing
		if uid := c.Query("user_id"); uid != "" {
			c.Set(middleware.ContextUserID, uid)
		}
		c.Next()
	})
	r.GET("/profile", h.GetProfile)
	r.PATCH("/profile", h.UpdateProfile)
	r.GET("/identities", h.ListIdentities)
	r.DELETE("/identities/:id", h.UnbindIdentity)
	r.GET("/apps", h.ListApps)
	return r
}

// ---------- GetProfile tests ----------

func TestGetProfile_Found(t *testing.T) {
	t.Parallel()

	nickname := "testuser"
	avatar := "https://example.com/avatar.png"
	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return &model.User{
				ID:        id,
				Nickname:  &nickname,
				AvatarURL: &avatar,
				Status:    "active",
			}, nil
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/profile?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["code"] != float64(0) {
		t.Errorf("code = %v, want 0", resp["code"])
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("response missing data object")
	}
	if data["id"] != "user1" {
		t.Errorf("data.id = %v, want user1", data["id"])
	}
	if data["nickname"] != "testuser" {
		t.Errorf("data.nickname = %v, want testuser", data["nickname"])
	}
}

func TestGetProfile_NotFound(t *testing.T) {
	t.Parallel()

	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return nil, errNotFound
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/profile?user_id=nonexistent", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "user not found") {
		t.Errorf("body = %s, want containing 'user not found'", w.Body.String())
	}
}

// ---------- UpdateProfile tests ----------

func TestUpdateProfile_Found(t *testing.T) {
	t.Parallel()

	origNickname := "original"
	origAvatar := "https://example.com/orig.png"
	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return &model.User{
				ID:        id,
				Nickname:  &origNickname,
				AvatarURL: &origAvatar,
				Status:    "active",
			}, nil
		},
		updateFn: func(ctx context.Context, u *model.User) error {
			return nil
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	body := `{"nickname":"newname"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/profile?user_id=user1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

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
	if data["nickname"] != "newname" {
		t.Errorf("data.nickname = %v, want newname", data["nickname"])
	}
}

func TestUpdateProfile_NotFound(t *testing.T) {
	t.Parallel()

	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return nil, errNotFound
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	body := `{"nickname":"newname"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/profile?user_id=nonexistent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "user not found") {
		t.Errorf("body = %s, want containing 'user not found'", w.Body.String())
	}
}

func TestUpdateProfile_PartialUpdate(t *testing.T) {
	t.Parallel()

	origNickname := "original"
	origAvatar := "https://example.com/orig.png"
	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return &model.User{
				ID:        id,
				Nickname:  &origNickname,
				AvatarURL: &origAvatar,
				Status:    "active",
			}, nil
		},
		updateFn: func(ctx context.Context, u *model.User) error {
			return nil
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})

	tests := []struct {
		name        string
		body        string
		wantNick    string
		wantAvatar  string
	}{
		{
			name:       "update nickname only",
			body:        `{"nickname":"newnick"}`,
			wantNick:   "newnick",
			wantAvatar: "https://example.com/orig.png",
		},
		{
			name:       "update avatar only",
			body:        `{"avatar_url":"https://example.com/new.png"}`,
			wantNick:   "original",
			wantAvatar: "https://example.com/new.png",
		},
		{
			name:       "update both",
			body:        `{"nickname":"bothnick","avatar_url":"https://example.com/both.png"}`,
			wantNick:   "bothnick",
			wantAvatar: "https://example.com/both.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rCopy := setupUserRouter(h)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/profile?user_id=user1", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rCopy.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
			}

			var resp map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}
			data := resp["data"].(map[string]interface{})
			if data["nickname"] != tt.wantNick {
				t.Errorf("nickname = %v, want %v", data["nickname"], tt.wantNick)
			}
			if data["avatar_url"] != tt.wantAvatar {
				t.Errorf("avatar_url = %v, want %v", data["avatar_url"], tt.wantAvatar)
			}
		})
	}
}

func TestUpdateProfile_InvalidBody(t *testing.T) {
	t.Parallel()

	origNickname := "original"
	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return &model.User{ID: id, Nickname: &origNickname, Status: "active"}, nil
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/profile?user_id=user1", strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid request body") {
		t.Errorf("body = %s, want containing 'invalid request body'", w.Body.String())
	}
}

func TestUpdateProfile_UpdateError(t *testing.T) {
	origNickname := "original"
	userRepo := &mockUserRepo{
		findFn: func(ctx context.Context, id string) (*model.User, error) {
			return &model.User{ID: id, Nickname: &origNickname, Status: "active"}, nil
		},
		updateFn: func(ctx context.Context, u *model.User) error {
			return errors.New("db error")
		},
	}

	h := NewUserHandler(userRepo, &mockSocialIdentityRepo{}, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	body := `{"nickname":"newname"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/profile?user_id=user1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to update profile") {
		t.Errorf("body = %s, want containing 'failed to update profile'", w.Body.String())
	}
}

// ---------- ListIdentities tests ----------

func TestListIdentities_Success(t *testing.T) {
	t.Parallel()

	identityRepo := &mockSocialIdentityRepo{
		listByUserIDFn: func(ctx context.Context, userID string) ([]model.SocialIdentity, error) {
			return []model.SocialIdentity{
				{ID: "si1", UserID: userID, Provider: "github", ProviderUID: "12345"},
				{ID: "si2", UserID: userID, Provider: "google", ProviderUID: "67890"},
			}, nil
		},
	}

	h := NewUserHandler(&mockUserRepo{}, identityRepo, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/identities?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].([]interface{})
	if !ok {
		t.Fatal("response missing data array")
	}
	if len(data) != 2 {
		t.Errorf("data length = %d, want 2", len(data))
	}
}

func TestListIdentities_Error(t *testing.T) {
	t.Parallel()

	identityRepo := &mockSocialIdentityRepo{
		listByUserIDFn: func(ctx context.Context, userID string) ([]model.SocialIdentity, error) {
			return nil, errors.New("db error")
		},
	}

	h := NewUserHandler(&mockUserRepo{}, identityRepo, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/identities?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to list identities") {
		t.Errorf("body = %s, want containing 'failed to list identities'", w.Body.String())
	}
}

// ---------- UnbindIdentity tests ----------

func TestUnbindIdentity_LastIdentityProtection(t *testing.T) {
	t.Parallel()

	identityRepo := &mockSocialIdentityRepo{
		deleteIfNotLastFn: func(ctx context.Context, id, userID string) (bool, error) {
			return false, nil // Not deleted — last identity
		},
	}

	h := NewUserHandler(&mockUserRepo{}, identityRepo, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/identities/si1?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "must keep at least one social account") {
		t.Errorf("body = %s, want containing 'must keep at least one social account'", w.Body.String())
	}
}

func TestUnbindIdentity_DeleteIfNotLastError(t *testing.T) {
	t.Parallel()

	identityRepo := &mockSocialIdentityRepo{
		deleteIfNotLastFn: func(ctx context.Context, id, userID string) (bool, error) {
			return false, errors.New("db error")
		},
	}

	h := NewUserHandler(&mockUserRepo{}, identityRepo, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/identities/si1?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to unbind identity") {
		t.Errorf("body = %s, want containing 'failed to unbind identity'", w.Body.String())
	}
}

func TestUnbindIdentity_DeleteError(t *testing.T) {
	t.Parallel()

	identityRepo := &mockSocialIdentityRepo{
		deleteIfNotLastFn: func(ctx context.Context, id, userID string) (bool, error) {
			return false, errors.New("db error")
		},
	}

	h := NewUserHandler(&mockUserRepo{}, identityRepo, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/identities/si1?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to unbind identity") {
		t.Errorf("body = %s, want containing 'failed to unbind identity'", w.Body.String())
	}
}

func TestUnbindIdentity_Success(t *testing.T) {
	t.Parallel()

	identityRepo := &mockSocialIdentityRepo{
		deleteIfNotLastFn: func(ctx context.Context, id, userID string) (bool, error) {
			return true, nil // Successfully deleted
		},
	}

	h := NewUserHandler(&mockUserRepo{}, identityRepo, &mockSubscriptionRepo{})
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/identities/si1?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unbound") {
		t.Errorf("body = %s, want containing 'unbound'", w.Body.String())
	}
}

// ---------- ListApps tests ----------

func TestListApps_Success(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		listFn: func(ctx context.Context, userID string) ([]model.Subscription, error) {
			return []model.Subscription{
				{ID: "sub1", UserID: userID, AppID: "app1", Plan: "free", Status: "active"},
				{ID: "sub2", UserID: userID, AppID: "app2", Plan: "pro", Status: "active"},
			}, nil
		},
	}

	h := NewUserHandler(&mockUserRepo{}, &mockSocialIdentityRepo{}, subRepo)
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	data, ok := resp["data"].([]interface{})
	if !ok {
		t.Fatal("response missing data array")
	}
	if len(data) != 2 {
		t.Errorf("data length = %d, want 2", len(data))
	}
}

func TestListApps_Error(t *testing.T) {
	t.Parallel()

	subRepo := &mockSubscriptionRepo{
		listFn: func(ctx context.Context, userID string) ([]model.Subscription, error) {
			return nil, errors.New("db error")
		},
	}

	h := NewUserHandler(&mockUserRepo{}, &mockSocialIdentityRepo{}, subRepo)
	r := setupUserRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/apps?user_id=user1", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to list apps") {
		t.Errorf("body = %s, want containing 'failed to list apps'", w.Body.String())
	}
}
