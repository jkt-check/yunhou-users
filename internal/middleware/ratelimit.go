package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type visitor struct {
	last   time.Time
	tokens int
}

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rate     int
	burst    int
}

func newRateLimiter(rate, burst int) *rateLimiter {
	return &rateLimiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		burst:    burst,
	}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, ok := rl.visitors[key]
	if !ok || now.Sub(v.last) > time.Minute {
		rl.visitors[key] = &visitor{last: now, tokens: rl.burst - 1}
		return true
	}

	elapsed := now.Sub(v.last)
	v.tokens += int(elapsed.Seconds()) * rl.rate
	if v.tokens > rl.burst {
		v.tokens = rl.burst
	}
	v.last = now

	if v.tokens <= 0 {
		return false
	}
	v.tokens--
	return true
}

func RateLimit(rate, burst int) gin.HandlerFunc {
	limiter := newRateLimiter(rate, burst)

	go func() {
		for {
			time.Sleep(time.Minute)
			limiter.mu.Lock()
			for k, v := range limiter.visitors {
				if time.Since(v.last) > 2*time.Minute {
					delete(limiter.visitors, k)
				}
			}
			limiter.mu.Unlock()
		}
	}()

	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !limiter.allow(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "too many requests",
			})
			return
		}
		c.Next()
	}
}
