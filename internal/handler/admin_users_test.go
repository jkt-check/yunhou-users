package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/service"
)

// mockAdminUsersSvc implements the handler's local adminUsersService
// interface with canned returns + captured args.
type mockAdminUsersSvc struct {
	searchRes *service.AdminUserSearchResult
	searchErr error
	detailRes *service.AdminUserDetail
	detailErr error
	vipRes    *service.AdminVipResult
	vipErr    error

	gotQuery   string
	gotUserID  string
	gotAppID   string
	gotActor   string
	gotDays    int
	gotIdemKey string
}

func (m *mockAdminUsersSvc) SearchUsers(_ context.Context, q string) (*service.AdminUserSearchResult, error) {
	m.gotQuery = q
	return m.searchRes, m.searchErr
}

func (m *mockAdminUsersSvc) GetUserDetail(_ context.Context, userID string) (*service.AdminUserDetail, error) {
	m.gotUserID = userID
	return m.detailRes, m.detailErr
}

func (m *mockAdminUsersSvc) AddVipDays(_ context.Context, appID, actor, userID string, days int, idemKey string) (*service.AdminVipResult, error) {
	m.gotAppID, m.gotActor, m.gotUserID, m.gotDays, m.gotIdemKey = appID, actor, userID, days, idemKey
	return m.vipRes, m.vipErr
}

func adminUsersTestEngine(svc adminUsersService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewAdminUsersHandler(svc)
	engine.GET("/admin/users/search", h.SearchUsers)
	engine.GET("/admin/users/:id", h.GetUser)
	engine.POST("/admin/users/:id/vip", h.AddVip)
	return engine
}

const adminTestUUID = "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11"

func adminRequest(t *testing.T, engine *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestAdminUsersSearch(t *testing.T) {
	t.Run("empty q is 400", func(t *testing.T) {
		engine := adminUsersTestEngine(&mockAdminUsersSvc{})
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/search?q=%20%20", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400 (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"code":400`) {
			t.Fatalf("envelope missing code=400: %s", w.Body.String())
		}
	})

	t.Run("missing q is 400", func(t *testing.T) {
		engine := adminUsersTestEngine(&mockAdminUsersSvc{})
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/search", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", w.Code)
		}
	})

	t.Run("success envelope", func(t *testing.T) {
		svc := &mockAdminUsersSvc{searchRes: &service.AdminUserSearchResult{
			Users: []service.AdminUserSummary{{ID: adminTestUUID, Status: "active", CreatedAt: "2026-09-23T08:00:00Z", Identities: []service.AdminIdentity{}}},
		}}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/search?q=alice", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if svc.gotQuery != "alice" {
			t.Fatalf("query = %q, want alice", svc.gotQuery)
		}
		if !strings.Contains(w.Body.String(), `"code":0`) || !strings.Contains(w.Body.String(), adminTestUUID) {
			t.Fatalf("bad envelope: %s", w.Body.String())
		}
	})

	t.Run("internal error is a generic 500", func(t *testing.T) {
		svc := &mockAdminUsersSvc{searchErr: errors.New("db connection leaked detail")}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/search?q=x", "", nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "leaked detail") {
			t.Fatalf("internal error leaked: %s", w.Body.String())
		}
	})
}

func TestAdminUsersGetUser(t *testing.T) {
	t.Run("bad uuid is 400", func(t *testing.T) {
		engine := adminUsersTestEngine(&mockAdminUsersSvc{})
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/not-a-uuid", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", w.Code)
		}
	})

	t.Run("uppercase uuid passes validation", func(t *testing.T) {
		svc := &mockAdminUsersSvc{detailErr: service.ErrUserNotFound}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/"+strings.ToUpper(adminTestUUID), "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404 (uuid must pass validation): %s", w.Code, w.Body.String())
		}
	})

	t.Run("unknown user is 404", func(t *testing.T) {
		svc := &mockAdminUsersSvc{detailErr: service.ErrUserNotFound}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/"+adminTestUUID, "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"code":404`) {
			t.Fatalf("envelope missing code=404: %s", w.Body.String())
		}
	})

	t.Run("success envelope", func(t *testing.T) {
		svc := &mockAdminUsersSvc{detailRes: &service.AdminUserDetail{
			User:       service.AdminUserInfo{ID: adminTestUUID, Status: "active", CreatedAt: "2026-09-23T08:00:00Z"},
			Identities: []service.AdminIdentity{},
			History:    []service.AdminSubscriptionHistoryItem{},
		}}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodGet, "/admin/users/"+adminTestUUID, "", nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"code":0`) {
			t.Fatalf("got %d (%s)", w.Code, w.Body.String())
		}
		if svc.gotUserID != adminTestUUID {
			t.Fatalf("userID = %q", svc.gotUserID)
		}
	})
}

func TestAdminUsersAddVip(t *testing.T) {
	t.Run("bad uuid is 400", func(t *testing.T) {
		engine := adminUsersTestEngine(&mockAdminUsersSvc{})
		w := adminRequest(t, engine, http.MethodPost, "/admin/users/nope/vip", `{"days":30}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", w.Code)
		}
	})

	t.Run("days bounds", func(t *testing.T) {
		for _, body := range []string{
			`{"days":0}`,
			`{"days":3651}`,
			`{"days":-1}`,
			`{"days":1.5}`,
			`{"days":"30"}`,
			`{}`,
			`{"days":30,"extra":true}`,
			`not-json`,
		} {
			engine := adminUsersTestEngine(&mockAdminUsersSvc{})
			w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %s: got %d, want 400 (%s)", body, w.Code, w.Body.String())
			}
		}
	})

	t.Run("days 1 and 3650 accepted", func(t *testing.T) {
		for _, days := range []string{"1", "3650"} {
			svc := &mockAdminUsersSvc{vipRes: &service.AdminVipResult{Action: "granted", PlanID: "monthly"}}
			engine := adminUsersTestEngine(svc)
			w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":`+days+`}`, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("days=%s: got %d (%s)", days, w.Code, w.Body.String())
			}
		}
	})

	t.Run("idempotency key over 128 chars is 400", func(t *testing.T) {
		engine := adminUsersTestEngine(&mockAdminUsersSvc{})
		w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`,
			map[string]string{"Idempotency-Key": strings.Repeat("k", 129)})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", w.Code)
		}
		// 上限按字符而非字节:128 个多字节字符(384 字节)合法,129 个非法。
		svc := &mockAdminUsersSvc{vipRes: &service.AdminVipResult{Action: "granted", PlanID: "monthly"}}
		engine = adminUsersTestEngine(svc)
		w = adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`,
			map[string]string{"Idempotency-Key": strings.Repeat("密", 128)})
		if w.Code != http.StatusOK {
			t.Fatalf("128 runes: got %d, want 200 (%s)", w.Code, w.Body.String())
		}
		w = adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`,
			map[string]string{"Idempotency-Key": strings.Repeat("密", 129)})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("129 runes: got %d, want 400", w.Code)
		}
	})

	t.Run("args threading and success envelope", func(t *testing.T) {
		svc := &mockAdminUsersSvc{vipRes: &service.AdminVipResult{
			Action: "extended",
			PlanID: "monthly",
			Before: &service.AdminVipSub{PlanID: "monthly"},
		}}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`,
			map[string]string{"Idempotency-Key": "dash-123"})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":"extended"`) {
			t.Fatalf("got %d (%s)", w.Code, w.Body.String())
		}
		// No InternalAppAuth middleware in the test engine, so the app
		// attribution degrades to the defensive defaults.
		if svc.gotActor != "admin:unknown" || svc.gotAppID != "" {
			t.Fatalf("actor=%q appID=%q", svc.gotActor, svc.gotAppID)
		}
		if svc.gotUserID != adminTestUUID || svc.gotDays != 30 || svc.gotIdemKey != "dash-123" {
			t.Fatalf("userID=%q days=%d idemKey=%q", svc.gotUserID, svc.gotDays, svc.gotIdemKey)
		}
	})

	t.Run("rejection is 409 with the operator message", func(t *testing.T) {
		svc := &mockAdminUsersSvc{vipErr: &service.AdminVipRejection{Reason: "该用户是终身 VIP（expires_at 为空），不支持加时长"}}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("got %d, want 409", w.Code)
		}
		if !strings.Contains(w.Body.String(), "终身 VIP") {
			t.Fatalf("message not passed through: %s", w.Body.String())
		}
	})

	t.Run("unknown user is 404", func(t *testing.T) {
		svc := &mockAdminUsersSvc{vipErr: service.ErrUserNotFound}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404", w.Code)
		}
	})

	t.Run("internal error is a generic 500", func(t *testing.T) {
		svc := &mockAdminUsersSvc{vipErr: errors.New("trigger raised: plan monthly missing")}
		engine := adminUsersTestEngine(svc)
		w := adminRequest(t, engine, http.MethodPost, "/admin/users/"+adminTestUUID+"/vip", `{"days":30}`, nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "trigger raised") {
			t.Fatalf("internal error leaked: %s", w.Body.String())
		}
	})
}
