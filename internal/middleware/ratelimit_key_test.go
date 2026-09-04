package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// RateLimitWithKey backs the per-user heartbeat bucket: two distinct keys
// (user IDs) must get independent allowances even from the same source IP.
func TestRateLimitWithKey(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	newEngine := func(handler gin.HandlerFunc, keyVal string) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
		engine := gin.New()
		engine.Use(func(c *gin.Context) {
			if keyVal != "" {
				c.Set(ContextUserID, keyVal)
			}
		}, handler)
		engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

		w1 := httptest.NewRecorder()
		engine.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/test", nil))
		w2 := httptest.NewRecorder()
		engine.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/test", nil))
		return w1, w2
	}

	t.Run("distinct keys get independent buckets", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		handler := RateLimitWithKey(ctx, 1, 1, func(c *gin.Context) string {
			return c.GetString(ContextUserID)
		})

		// user-a: first request allowed, second blocked (burst 1)
		w1, w2 := newEngine(handler, "user-a")
		if w1.Code != http.StatusOK {
			t.Errorf("user-a first request: expected 200, got %d", w1.Code)
		}
		if w2.Code != http.StatusTooManyRequests {
			t.Errorf("user-a second request: expected 429, got %d", w2.Code)
		}

		// user-b (same IP): unaffected by user-a's exhaustion
		w3, _ := newEngine(handler, "user-b")
		if w3.Code != http.StatusOK {
			t.Errorf("user-b first request: expected 200, got %d", w3.Code)
		}
	})

	t.Run("empty key falls back to client IP", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		handler := RateLimitWithKey(ctx, 1, 1, func(c *gin.Context) string {
			return c.GetString(ContextUserID) // unset → ""
		})

		w1, w2 := newEngine(handler, "")
		if w1.Code != http.StatusOK {
			t.Errorf("first request: expected 200, got %d", w1.Code)
		}
		if w2.Code != http.StatusTooManyRequests {
			t.Errorf("second request: expected 429, got %d", w2.Code)
		}
	})
}
