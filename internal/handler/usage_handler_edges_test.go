package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/model"
)

// usage_handler_edges_test.go — Task 16 覆盖率补强：运营统计 handler 的
// 参数校验分支（from/to 必传）。

func TestUsageHandler_RequiredFromTo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewUsageHandler(&stubUsageSvc{})

	for _, path := range []string{"/admin/stats/usage-duration", "/admin/stats/new-users"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		switch path {
		case "/admin/stats/usage-duration":
			h.GetUsageDuration(c)
		default:
			h.GetNewUsers(c)
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s without from/to = %d, want 400", path, w.Code)
		}
	}
}

type stubUsageSvc struct{}

func (stubUsageSvc) RecordBeats(ctx context.Context, userID, appID string, beats []model.UsageBeat) (int, error) {
	return len(beats), nil
}
func (stubUsageSvc) ActiveStats(ctx context.Context, date, granularity, groupBy string) (*model.ActiveStats, error) {
	return &model.ActiveStats{}, nil
}
func (stubUsageSvc) DurationStats(ctx context.Context, from, to, granularity, groupBy string) ([]model.UsageDurationRow, error) {
	return nil, nil
}
func (stubUsageSvc) NewUsersStats(ctx context.Context, from, to string) ([]model.NewUsersRow, error) {
	return nil, nil
}
