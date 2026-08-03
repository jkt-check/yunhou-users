package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestTimeoutMiddleware_SkipList locks in the /chat exemption: a typo in the
// skip path would silently re-subject the SSE stream to the 20s cap and
// nothing else would fail. A skipped route keeps the request's original
// context (no deadline); a normal route gets the middleware's deadline.
func TestTimeoutMiddleware_SkipList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(timeoutMiddleware(50*time.Millisecond, "/chat"))

	var skipHasDeadline, normalHasDeadline bool
	var normalRemaining time.Duration
	r.POST("/chat", func(c *gin.Context) {
		_, skipHasDeadline = c.Request.Context().Deadline()
		c.Status(http.StatusOK)
	})
	r.POST("/other", func(c *gin.Context) {
		deadline, ok := c.Request.Context().Deadline()
		normalHasDeadline = ok
		if ok {
			normalRemaining = time.Until(deadline)
		}
		c.Status(http.StatusOK)
	})

	for _, path := range []string{"/chat", "/other"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, w.Code)
		}
	}

	if skipHasDeadline {
		t.Error("/chat: request context has a deadline, want none (skip list)")
	}
	if !normalHasDeadline {
		t.Fatal("/other: request context has no deadline, want the middleware's 50ms cap")
	}
	if normalRemaining <= 0 || normalRemaining > 50*time.Millisecond {
		t.Errorf("/other: deadline in %v, want within (0, 50ms]", normalRemaining)
	}
}
