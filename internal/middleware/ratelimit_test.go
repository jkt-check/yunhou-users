package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRateLimit(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	t.Run("allows requests under limit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := RateLimit(ctx, 10, 20) // returns gin.HandlerFunc

		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/test", nil)

			engine := gin.New()
			engine.Use(handler)
			engine.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})
			engine.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Errorf("request %d: expected 200, got %d", i+1, w.Code)
			}
		}
	})

	t.Run("blocks requests over limit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := RateLimit(ctx, 1, 1) // 1 req, burst 1

		// First request should succeed
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		engine := gin.New()
		engine.Use(handler)
		engine.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		engine.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("first request: expected 200, got %d", w.Code)
		}

		// Second request should be rate limited
		w2 := httptest.NewRecorder()
		r2 := httptest.NewRequest(http.MethodGet, "/test", nil)
		engine.ServeHTTP(w2, r2)

		if w2.Code != http.StatusTooManyRequests {
			t.Errorf("second request: expected 429, got %d", w2.Code)
		}
	})

	t.Run("allows burst up to limit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := RateLimit(ctx, 10, 5) // 5 burst

		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/test", nil)
			engine := gin.New()
			engine.Use(handler)
			engine.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})
			engine.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Errorf("burst request %d: expected 200, got %d", i+1, w.Code)
			}
		}

		// 6th request should be blocked
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		engine := gin.New()
		engine.Use(handler)
		engine.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		engine.ServeHTTP(w, r)

		if w.Code != http.StatusTooManyRequests {
			t.Errorf("6th request: expected 429, got %d", w.Code)
		}
	})

	t.Run("refills tokens over time", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := RateLimit(ctx, 2, 2) // 2 per second

		// Exhaust tokens
		for i := 0; i < 2; i++ {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/test", nil)
			engine := gin.New()
			engine.Use(handler)
			engine.GET("/test", func(c *gin.Context) {
				c.Status(http.StatusOK)
			})
			engine.ServeHTTP(w, r)
		}

		// Should be blocked
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		engine := gin.New()
		engine.Use(handler)
		engine.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		engine.ServeHTTP(w, r)

		if w.Code != http.StatusTooManyRequests {
			t.Errorf("should be rate limited, got %d", w.Code)
		}

		// Wait for refill (more than 1 second)
		time.Sleep(1100 * time.Millisecond)

		w2 := httptest.NewRecorder()
		r2 := httptest.NewRequest(http.MethodGet, "/test", nil)
		engine.ServeHTTP(w2, r2)

		if w2.Code != http.StatusOK {
			t.Errorf("after refill: expected 200, got %d", w2.Code)
		}
	})

	t.Run("thread safe", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := RateLimit(ctx, 100, 100)

		var wg sync.WaitGroup
		success := 0
		var mu sync.Mutex

		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/test", nil)
				engine := gin.New()
				engine.Use(handler)
				engine.GET("/test", func(c *gin.Context) {
					c.Status(http.StatusOK)
				})
				engine.ServeHTTP(w, r)

				if w.Code == http.StatusOK {
					mu.Lock()
					success++
					mu.Unlock()
				}
			}()
		}

		wg.Wait()

		if success != 50 {
			t.Errorf("expected 50 successful requests, got %d", success)
		}
	})
}

func TestRateLimitKeyFunc(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	t.Run("uses IP as default key", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler := RateLimit(ctx, 10, 20)

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/test", nil)
		r.RemoteAddr = "192.168.1.1:12345"

		engine := gin.New()
		engine.Use(handler)
		engine.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		engine.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

func TestTokenBucket(t *testing.T) {
	t.Parallel()

	t.Run("initial burst capacity set correctly", func(t *testing.T) {
		tb := newRateLimiter(5, 10)
		if tb.burst != 10 {
			t.Errorf("expected burst 10, got %d", tb.burst)
		}
	})

	t.Run("refill mechanism", func(t *testing.T) {
		tb := newRateLimiter(2, 2)

		// Should allow 2 requests
		if !tb.allow("ip1") {
			t.Error("expected allow for first request")
		}
		if !tb.allow("ip1") {
			t.Error("expected allow for second request")
		}

		// Third should be blocked (burst exhausted)
		if tb.allow("ip1") {
			t.Error("expected deny for third request")
		}
	})

	t.Run("cleanup removes stale visitors", func(t *testing.T) {
		tb := newRateLimiter(10, 10)
		tb.allow("stale-ip")

		// Mark as old
		v, _ := tb.visitors.Load("stale-ip")
		v.(*visitor).lastSeenUnixNano.Store(time.Now().Add(-3 * time.Minute).UnixNano())

		tb.cleanup()

		if _, ok := tb.visitors.Load("stale-ip"); ok {
			t.Error("stale visitor should be removed")
		}
	})

	t.Run("different IPs have separate limiters", func(t *testing.T) {
		tb := newRateLimiter(1, 1)

		// IP1 exhausts
		tb.allow("ip1")
		if tb.allow("ip1") {
			t.Error("ip1 should be rate limited")
		}

		// IP2 should still work
		if !tb.allow("ip2") {
			t.Error("ip2 should not be affected by ip1")
		}
	})

	t.Run("allow is thread safe", func(t *testing.T) {
		tb := newRateLimiter(100, 100)
		var wg sync.WaitGroup

		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tb.allow("shared-ip")
			}()
		}

		wg.Wait()
		// If we get here without deadlock, it's thread safe
	})
}
