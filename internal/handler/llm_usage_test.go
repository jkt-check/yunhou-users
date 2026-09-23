package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// stubLLMUsageRepo implements repo.LLMUsageRepo so the handler test can drive
// the concrete *service.LLMUsageService (mirrors usageHandler tests, which
// wire the real service over a mock).
type stubLLMUsageRepo struct {
	rows []model.LLMUsageRow
	err  error
}

func (s *stubLLMUsageRepo) InsertEvent(_ context.Context, _ model.LLMUsageEvent) error {
	return nil
}

func (s *stubLLMUsageRepo) SumByModel(_ context.Context, _, _ string) ([]model.LLMUsageRow, error) {
	return s.rows, s.err
}

func llmUsageTestEngine(r *stubLLMUsageRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewLLMUsageHandler(service.NewLLMUsageService(r))
	engine.GET("/admin/stats/llm-usage", h.GetByModel)
	return engine
}

func TestLLMUsageGetByModel(t *testing.T) {
	t.Parallel()

	t.Run("missing or invalid params map to 400", func(t *testing.T) {
		engine := llmUsageTestEngine(&stubLLMUsageRepo{})
		for _, url := range []string{
			"/admin/stats/llm-usage",
			"/admin/stats/llm-usage?from=2026-09-01",
			"/admin/stats/llm-usage?from=2026/09/01&to=2026-09-07",
			"/admin/stats/llm-usage?from=2026-09-07&to=2026-09-01",
		} {
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d (%s)", url, w.Code, w.Body.String())
			}
		}
	})

	t.Run("success returns rows and empty slice is []", func(t *testing.T) {
		engine := llmUsageTestEngine(&stubLLMUsageRepo{rows: []model.LLMUsageRow{
			{Model: "deepseek-flash", Requests: 3, InputTokens: 1000, OutputTokens: 200, CostMicros: 3600},
		}})
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/llm-usage?from=2026-09-01&to=2026-09-07", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"deepseek-flash"`) {
			t.Errorf("response missing model row: %s", w.Body.String())
		}

		empty := llmUsageTestEngine(&stubLLMUsageRepo{})
		w = httptest.NewRecorder()
		empty.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/llm-usage?from=2026-09-01&to=2026-09-07", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"data":[]`) {
			t.Errorf("empty result must serialize as [], got %s", w.Body.String())
		}
	})

	t.Run("repo error maps to 500 without detail leak", func(t *testing.T) {
		engine := llmUsageTestEngine(&stubLLMUsageRepo{err: errors.New("db down")})
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/llm-usage?from=2026-09-01&to=2026-09-07", nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "db down") {
			t.Error("internal error detail must not leak to the client")
		}
	})

	t.Run("nil service maps to 503, not a panic", func(t *testing.T) {
		// router.Setup registers the route unconditionally; a nil service
		// (e.g. tests wiring the router without the repo) must degrade to a
		// clean error, mirroring how a disabled chat returns an error rather
		// than crashing the request.
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		engine.GET("/admin/stats/llm-usage", NewLLMUsageHandler(nil).GetByModel)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
			"/admin/stats/llm-usage?from=2026-09-01&to=2026-09-07", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d (%s)", w.Code, w.Body.String())
		}
	})
}
