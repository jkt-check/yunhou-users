package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/model"
)

// chatTestFixture wires a ChatService with mock repos and an optional
// upstream stub. The stub, when non-nil, records the request it received.
func chatTestFixture(t *testing.T, upstream http.Handler) (*ChatService, *mockSubscriptionRepo, *mockPlanRepo) {
	t.Helper()
	subRepo := newMockSubscriptionRepo()
	planRepo := newMockPlanRepo()
	svc := NewChatService("test-key", "https://upstream.invalid", "deepseek-v4-flash", subRepo, planRepo)
	if upstream != nil {
		srv := httptest.NewServer(upstream)
		t.Cleanup(srv.Close)
		svc.SetBaseURL(srv.URL)
	}
	return svc, subRepo, planRepo
}

// seedChatActiveSub adds an active, non-expired subscription for userID pointing
// at planID.
func seedChatActiveSub(repo *mockSubscriptionRepo, userID, planID string) {
	now := time.Now()
	future := now.Add(30 * 24 * time.Hour)
	repo.byUserID[userID] = &model.Subscription{
		ID:        "sub-" + userID,
		UserID:    userID,
		PlanID:    planID,
		Status:    "active",
		StartedAt: now,
		ExpiresAt: &future,
	}
}

func TestChatService_NotEnabled(t *testing.T) {
	svc := NewChatService("", "https://upstream.invalid", "deepseek-v4-flash",
		newMockSubscriptionRepo(), newMockPlanRepo())
	_, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", []model.ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrChatNotEnabled) {
		t.Fatalf("err = %v, want ErrChatNotEnabled", err)
	}
}

func TestChatService_AccessGating(t *testing.T) {
	now := time.Now()
	activePlan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	inactivePlan := &model.Plan{ID: "retired", IsActive: false, Apps: pq.StringArray{"yunhou-website"}}
	wrongAppPlan := &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yundian"}}

	cases := []struct {
		name    string
		subRepo *mockSubscriptionRepo
		plan    *model.Plan
		wantErr error
	}{
		{"no subscription", newMockSubscriptionRepo(), activePlan, ErrChatNoAccess},
		{"expired subscription", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			expiredAt := now.Add(-1 * time.Hour)
			r.byUserID["u-1"] = &model.Subscription{ID: "s1", UserID: "u-1", PlanID: "monthly", Status: "active", StartedAt: now.Add(-60 * 24 * time.Hour), ExpiresAt: &expiredAt}
			return r
		}(), activePlan, ErrChatNoAccess},
		{"plan inactive", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			seedChatActiveSub(r, "u-1", "retired")
			return r
		}(), inactivePlan, ErrChatNoAccess},
		{"plan lacks app", func() *mockSubscriptionRepo {
			r := newMockSubscriptionRepo()
			seedChatActiveSub(r, "u-1", "monthly")
			return r
		}(), wrongAppPlan, ErrChatNoAccess},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planRepo := newMockPlanRepo()
			planRepo.plans[tc.plan.ID] = tc.plan
			svc := NewChatService("test-key", "https://upstream.invalid", "deepseek-v4-flash", tc.subRepo, planRepo)
			_, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", []model.ChatMessage{{Role: "user", Content: "hi"}})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestChatService_RepoError(t *testing.T) {
	// FindActiveByUserID repo failure must surface as a wrapped error (500),
	// not as a false "no access".
	subRepo := newMockSubscriptionRepo()
	subRepo.findErr = errors.New("db down")
	svc := NewChatService("test-key", "https://upstream.invalid", "deepseek-v4-flash", subRepo, newMockPlanRepo())
	_, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", []model.ChatMessage{{Role: "user", Content: "hi"}})
	if err == nil || errors.Is(err, ErrChatNoAccess) || errors.Is(err, ErrChatNotEnabled) {
		t.Fatalf("err = %v, want a wrapped repo error", err)
	}
}

func TestChatService_StreamSuccess(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: [DONE]\n\n"
	var gotMethod, gotAuth, gotContentType string
	var gotBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	})

	svc, subRepo, planRepo := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website",
		[]model.ChatMessage{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream content-type = %q, want application/json", gotContentType)
	}
	body := string(gotBody)
	for _, want := range []string{`"model":"deepseek-v4-flash"`, `"stream":true`, `"role":"user"`, `"content":"hi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream body missing %s: %s", want, body)
		}
	}
	// The SSE body must pass through verbatim.
	streamed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(streamed) != sse {
		t.Errorf("stream = %q, want %q", streamed, sse)
	}
}

// TestChatService_StreamChunkedDelayed guards the cancel-on-close binding:
// the transport's readLoop tears down the upstream connection when reqCtx is
// cancelled, so cancelling at StreamChat return (instead of at body Close)
// would cut a real, slowly-dripping SSE stream before the caller read it.
func TestChatService_StreamChunkedDelayed(t *testing.T) {
	chunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"一\"}}]}\n\n"
	chunk2 := "data: {\"choices\":[{\"delta\":{\"content\":\"二\"}}]}\n\n"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(chunk1))
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond) // second chunk arrives after StreamChat has returned
		w.Write([]byte(chunk2))
		w.(http.Flusher).Flush()
	})

	svc, subRepo, planRepo := chatTestFixture(t, upstream)
	seedChatActiveSub(subRepo, "u-1", "monthly")
	planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

	resp, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", []model.ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	defer resp.Body.Close()

	streamed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if got := string(streamed); got != chunk1+chunk2 {
		t.Fatalf("stream = %q, want both delayed chunks %q", got, chunk1+chunk2)
	}
}

func TestChatService_UpstreamErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"rate limited", http.StatusTooManyRequests, ErrChatRateLimited},
		{"upstream 500", http.StatusInternalServerError, ErrChatUpstreamError},
		{"upstream 401", http.StatusUnauthorized, ErrChatUpstreamError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error":"boom"}`))
			})
			svc, subRepo, planRepo := chatTestFixture(t, upstream)
			seedChatActiveSub(subRepo, "u-1", "monthly")
			planRepo.plans["monthly"] = &model.Plan{ID: "monthly", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}

			_, err := svc.StreamChat(context.Background(), "u-1", "yunhou-website", []model.ChatMessage{{Role: "user", Content: "hi"}})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
