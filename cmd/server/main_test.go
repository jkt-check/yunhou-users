package main

import (
	"bytes"
	"io"
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

// TestMaxRequestBodyBytes_SkipList locks in the /chat + /v1 exemption (R6
// hotfix): without it the engine-level 1 MiB reader truncates bodies before
// the handler-level 8 MiB caps ever fire — clients were 400'd at exactly
// 1 MiB. Every skipped route receives the body untouched (its own handler
// cap decides); a normal route is still capped at exactly n. Registering
// stubs for all four skip paths pins each entry string against typos.
func TestMaxRequestBodyBytes_SkipList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	skipPaths := []string{"/chat", "/v1/chat/completions", "/v1/messages", "/v1/responses"}
	r := gin.New()
	r.Use(maxRequestBodyBytes(1<<20, skipPaths...))

	// 2 MiB: above the engine cap, below the handler-level 8 MiB caps.
	body := bytes.Repeat([]byte("a"), 2<<20)

	readN := map[string]int{}
	readErr := map[string]error{}
	readAll := func(path string) gin.HandlerFunc {
		return func(c *gin.Context) {
			b, err := io.ReadAll(c.Request.Body)
			readN[path] = len(b)
			readErr[path] = err
			c.Status(http.StatusOK)
		}
	}
	for _, p := range skipPaths {
		r.POST(p, readAll(p))
	}
	r.POST("/other", readAll("/other"))

	post := func(path string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, w.Code)
		}
	}

	for _, p := range skipPaths {
		post(p)
		if readErr[p] != nil || readN[p] != len(body) {
			t.Errorf("%s: read %d bytes, err = %v; want full %d-byte body (skip list)", p, readN[p], readErr[p], len(body))
		}
	}

	post("/other")
	if readErr["/other"] == nil || readErr["/other"].Error() != "http: request body too large" {
		t.Errorf("/other: err = %v, want \"http: request body too large\"", readErr["/other"])
	}
	if readN["/other"] != 1<<20 {
		t.Errorf("/other: read %d bytes, want exactly %d (the 1 MiB cap)", readN["/other"], 1<<20)
	}
}
