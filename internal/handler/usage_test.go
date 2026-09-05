package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// mockUsageSvc implements the handler's local usageService interface with
// canned returns + captured args (hand-rolled, same style as mockAuthSvc).
type mockUsageSvc struct {
	inserted   int
	recordErr  error
	activeResp *model.ActiveStats
	activeErr  error
	duration   []model.UsageDurationRow
	newUsers   []model.NewUsersRow

	gotUserID, gotAppID string
	gotBeats            []model.UsageBeat
	gotArgs             []string
}

func (m *mockUsageSvc) RecordBeats(_ context.Context, userID, appID string, beats []model.UsageBeat) (int, error) {
	m.gotUserID, m.gotAppID, m.gotBeats = userID, appID, beats
	return m.inserted, m.recordErr
}

func (m *mockUsageSvc) ActiveStats(_ context.Context, date, granularity, groupBy string) (*model.ActiveStats, error) {
	m.gotArgs = []string{date, granularity, groupBy}
	return m.activeResp, m.activeErr
}

func (m *mockUsageSvc) DurationStats(_ context.Context, from, to, granularity, groupBy string) ([]model.UsageDurationRow, error) {
	m.gotArgs = []string{from, to, granularity, groupBy}
	return m.duration, nil
}

func (m *mockUsageSvc) NewUsersStats(_ context.Context, from, to string) ([]model.NewUsersRow, error) {
	m.gotArgs = []string{from, to}
	return m.newUsers, nil
}

// usageTestEngine wires the heartbeat route the same way router.Setup does:
// a stand-in for JWTAuth that stamps the identity into the context, then
// the handler. The stats routes need no identity stub (admin auth is
// middleware-level and not under test here).
func usageTestEngine(svc usageService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/user/usage/heartbeat", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "user-abc")
		c.Set(middleware.ContextAppID, "yunhou-website")
	}, NewUsageHandler(svc).PostHeartbeat)
	h := NewUsageHandler(svc)
	engine.GET("/admin/stats/active", h.GetActiveStats)
	engine.GET("/admin/stats/usage-duration", h.GetUsageDuration)
	engine.GET("/admin/stats/new-users", h.GetNewUsers)
	return engine
}

func validUsageBeat() model.UsageBeat {
	return model.UsageBeat{
		ClientEventID: "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11",
		OccurredAt:    time.Date(2026, 9, 4, 10, 0, 0, 0, time.FixedZone("CST", 8*3600)),
		LocalDate:     "2026-09-04",
		ActiveSeconds: 300,
		Platform:      "macos",
		AppVersion:    "2.1.14",
	}
}

func postHeartbeat(t *testing.T, engine *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/user/usage/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)
	return w
}

func TestPostHeartbeat(t *testing.T) {
	t.Parallel()

	t.Run("valid batch reaches service with JWT identity", func(t *testing.T) {
		svc := &mockUsageSvc{inserted: 2}
		engine := usageTestEngine(svc)

		beat := validUsageBeat()
		payload, err := json.Marshal(model.UsageHeartbeatRequest{Beats: []model.UsageBeat{beat, beat}})
		if err != nil {
			t.Fatal(err)
		}
		w := postHeartbeat(t, engine, string(payload))

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
		if svc.gotUserID != "user-abc" || svc.gotAppID != "yunhou-website" {
			t.Errorf("identity = (%q, %q), want JWT context values", svc.gotUserID, svc.gotAppID)
		}
		if len(svc.gotBeats) != 2 {
			t.Errorf("beats = %d, want 2", len(svc.gotBeats))
		}
		var resp struct {
			Data struct {
				Accepted int `json:"accepted"`
				Inserted int `json:"inserted"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Data.Accepted != 2 || resp.Data.Inserted != 2 {
			t.Errorf("accepted/inserted = %d/%d, want 2/2", resp.Data.Accepted, resp.Data.Inserted)
		}
	})

	// Field-validation matrix: each case mutates one field of a valid beat
	// into an invalid one and expects a 400 without the service being hit.
	invalidBodies := map[string]string{
		"empty beats":          `{"beats": []}`,
		"missing beats":        `{}`,
		"bad client_event_id":  `{"beats": [{"client_event_id": "not-a-uuid", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "2026-09-04", "active_seconds": 300, "platform": "macos", "app_version": "2.1.14"}]}`,
		"missing occurred_at":  `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "local_date": "2026-09-04", "active_seconds": 300, "platform": "macos", "app_version": "2.1.14"}]}`,
		"bad local_date":       `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "04/09/2026", "active_seconds": 300, "platform": "macos", "app_version": "2.1.14"}]}`,
		"active_seconds > max": `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "2026-09-04", "active_seconds": 3601, "platform": "macos", "app_version": "2.1.14"}]}`,
		"active_seconds < 0":   `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "2026-09-04", "active_seconds": -1, "platform": "macos", "app_version": "2.1.14"}]}`,
		"bad platform":         `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "2026-09-04", "active_seconds": 300, "platform": "dos", "app_version": "2.1.14"}]}`,
		"empty app_version":    `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "2026-09-04", "active_seconds": 300, "platform": "macos", "app_version": ""}]}`,
		"long app_version":     `{"beats": [{"client_event_id": "3f6b0d4e-7c2a-4c1a-9a4b-2f2c0d5e8a11", "occurred_at": "2026-09-04T10:00:00+08:00", "local_date": "2026-09-04", "active_seconds": 300, "platform": "macos", "app_version": "` + strings.Repeat("x", 65) + `"}]}`,
		"malformed json":       `{"beats": [`,
	}
	for name, body := range invalidBodies {
		t.Run("reject: "+name, func(t *testing.T) {
			svc := &mockUsageSvc{}
			engine := usageTestEngine(svc)
			w := postHeartbeat(t, engine, body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d (%s)", w.Code, w.Body.String())
			}
			if svc.gotBeats != nil {
				t.Error("service must not be called for invalid input")
			}
		})
	}

	t.Run("reject: batch over 100 beats", func(t *testing.T) {
		svc := &mockUsageSvc{}
		engine := usageTestEngine(svc)
		beats := make([]model.UsageBeat, model.UsageMaxBeatsPerRequest+1)
		for i := range beats {
			beats[i] = validUsageBeat()
		}
		payload, _ := json.Marshal(model.UsageHeartbeatRequest{Beats: beats})
		w := postHeartbeat(t, engine, string(payload))
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("service error maps to 500", func(t *testing.T) {
		svc := &mockUsageSvc{recordErr: errors.New("db down")}
		engine := usageTestEngine(svc)
		payload, _ := json.Marshal(model.UsageHeartbeatRequest{Beats: []model.UsageBeat{validUsageBeat()}})
		w := postHeartbeat(t, engine, string(payload))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "db down") {
			t.Error("internal error detail must not leak to the client")
		}
	})
}

func TestGetActiveStats(t *testing.T) {
	t.Parallel()

	t.Run("default params and success shape", func(t *testing.T) {
		svc := &mockUsageSvc{activeResp: &model.ActiveStats{
			Date: "2026-09-04", From: "2026-09-04", To: "2026-09-04",
			Granularity: "day", Total: 42,
		}}
		engine := usageTestEngine(svc)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/stats/active", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
		// date defaults to today (server tz), granularity to day, no group_by
		if svc.gotArgs[1] != "day" || svc.gotArgs[2] != "" {
			t.Errorf("granularity/group_by = %q/%q, want day/empty", svc.gotArgs[1], svc.gotArgs[2])
		}
		if _, err := time.Parse("2006-01-02", svc.gotArgs[0]); err != nil {
			t.Errorf("default date %q is not YYYY-MM-DD", svc.gotArgs[0])
		}
		if !strings.Contains(w.Body.String(), `"total":42`) {
			t.Errorf("response missing total: %s", w.Body.String())
		}
	})

	t.Run("query params forwarded", func(t *testing.T) {
		svc := &mockUsageSvc{activeResp: &model.ActiveStats{}}
		engine := usageTestEngine(svc)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/active?date=2026-09-01&granularity=week&group_by=platform", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		want := []string{"2026-09-01", "week", "platform"}
		for i, v := range want {
			if svc.gotArgs[i] != v {
				t.Errorf("arg %d = %q, want %q", i, svc.gotArgs[i], v)
			}
		}
	})

	t.Run("invalid param maps to 400 with detail", func(t *testing.T) {
		svc := &mockUsageSvc{activeErr: service.ErrUsageInvalidParam}
		engine := usageTestEngine(svc)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/stats/active?granularity=year", nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("internal error maps to 500", func(t *testing.T) {
		svc := &mockUsageSvc{activeErr: errors.New("db down")}
		engine := usageTestEngine(svc)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/stats/active", nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

func TestGetUsageDurationAndNewUsers(t *testing.T) {
	t.Parallel()

	t.Run("usage-duration requires from/to", func(t *testing.T) {
		engine := usageTestEngine(&mockUsageSvc{})
		for _, url := range []string{
			"/admin/stats/usage-duration",
			"/admin/stats/usage-duration?from=2026-09-01",
			"/admin/stats/new-users",
			"/admin/stats/new-users?to=2026-09-04",
		} {
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d", url, w.Code)
			}
		}
	})

	t.Run("usage-duration success", func(t *testing.T) {
		svc := &mockUsageSvc{duration: []model.UsageDurationRow{
			{Period: "2026-09-04", TotalSeconds: 1200, ActiveUsers: 3, PerUserSeconds: 400},
		}}
		engine := usageTestEngine(svc)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/usage-duration?from=2026-09-01&to=2026-09-04&granularity=month", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if svc.gotArgs[2] != "month" {
			t.Errorf("granularity = %q, want month", svc.gotArgs[2])
		}
		if !strings.Contains(w.Body.String(), `"per_user_seconds":400`) {
			t.Errorf("response missing per_user_seconds: %s", w.Body.String())
		}
	})

	t.Run("new-users success", func(t *testing.T) {
		svc := &mockUsageSvc{newUsers: []model.NewUsersRow{{Date: "2026-09-04", Users: 5}}}
		engine := usageTestEngine(svc)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/new-users?from=2026-09-01&to=2026-09-04", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if svc.gotArgs[0] != "2026-09-01" || svc.gotArgs[1] != "2026-09-04" {
			t.Errorf("args = %v", svc.gotArgs)
		}
		if !strings.Contains(w.Body.String(), `"users":5`) {
			t.Errorf("response missing users: %s", w.Body.String())
		}
	})
}
