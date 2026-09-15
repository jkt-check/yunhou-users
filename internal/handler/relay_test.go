package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

const testRelaySecretForHandler = "handler-test-relay-secret-0123456789"

// relayStubSubRepo / relayStubPlanRepo 只实现 RelayService 用到的方法;
// 其余接口方法经内嵌 nil interface 满足编译(本测试不会触达)。
type relayStubSubRepo struct {
	repo.SubscriptionRepo
	sub *model.Subscription
	err error
}

func (r *relayStubSubRepo) FindActiveByUserID(context.Context, string) (*model.Subscription, error) {
	return r.sub, r.err
}

type relayStubPlanRepo struct {
	repo.PlanRepo
	plan *model.Plan
	err  error
}

func (r *relayStubPlanRepo) FindByID(context.Context, string) (*model.Plan, error) {
	return r.plan, r.err
}

// newRelayServiceForHandlerTest 构造放行(accessErr == nil)或拒绝两种
// RelayService。
func newRelayServiceForHandlerTest(t *testing.T, accessErr error) *service.RelayService {
	t.Helper()
	subRepo := &relayStubSubRepo{}
	planRepo := &relayStubPlanRepo{}
	if accessErr == nil {
		future := time.Now().Add(24 * time.Hour)
		subRepo.sub = &model.Subscription{ID: "s1", UserID: "u-1", PlanID: "p1", Status: "active", ExpiresAt: &future}
		planRepo.plan = &model.Plan{ID: "p1", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	} else {
		subRepo.err = sql.ErrNoRows
	}
	return service.NewRelayService(subRepo, planRepo,
		service.NewRelayTicketService(testRelaySecretForHandler, "", 300*time.Second))
}

func relayTicketTestRouter(svc *service.RelayService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewRelayHandler(svc)
	r := gin.New()
	r.POST("/relay/ticket", func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "u-1")
		c.Set(middleware.ContextAppID, "yunhou-website")
		h.IssueTicket(c)
	})
	return r
}

func TestRelayTicketIssueOK(t *testing.T) {
	svc := newRelayServiceForHandlerTest(t, nil)
	r := relayTicketTestRouter(svc)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/relay/ticket", nil)
	req.Host = "api.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Ticket    string `json:"ticket"`
			ExpiresIn int    `json:"expires_in"`
			WSURL     string `json:"ws_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.Ticket == "" || resp.Data.ExpiresIn != 300 {
		t.Fatalf("bad data: %+v", resp.Data)
	}
	if resp.Data.WSURL != "wss://api.example.com/relay/ws" {
		t.Fatalf("ws_url = %q", resp.Data.WSURL)
	}
	// 签出的 ticket 必须能被同一 secret 校验回 user
	userID, _, err := service.NewRelayTicketService(testRelaySecretForHandler, "", 300*time.Second).Verify(resp.Data.Ticket)
	if err != nil || userID != "u-1" {
		t.Fatalf("round trip: userID=%q err=%v", userID, err)
	}
}

func TestRelayTicketIssueForbidden(t *testing.T) {
	svc := newRelayServiceForHandlerTest(t, service.ErrRelayNoAccess)
	r := relayTicketTestRouter(svc)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/relay/ticket", nil))
	if rec.Code != 403 {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "remote access requires paid plan") {
		t.Fatalf("message: %s", rec.Body.String())
	}
}
