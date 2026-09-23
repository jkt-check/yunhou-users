package relay

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// HelloFailLimiter 限制单 IP 的 hello 认证失败频率(spec §8:5 次/min)。
// 防 ticket 爆破的兜底(HMAC 本身不可伪造)。超限的 IP 在 WS 握手
// 阶段直接 403,不升级。
type HelloFailLimiter struct {
	mu       sync.Mutex
	visitors map[string]*rate.Limiter
	lastSeen map[string]time.Time
	rate     rate.Limit
	burst    int
	stop     chan struct{}
	stopOnce sync.Once
}

func NewHelloFailLimiter(perMin int) *HelloFailLimiter {
	l := &HelloFailLimiter{
		visitors: make(map[string]*rate.Limiter),
		lastSeen: make(map[string]time.Time),
		rate:     rate.Limit(float64(perMin) / 60),
		burst:    perMin,
		stop:     make(chan struct{}),
	}
	go l.janitor()
	return l
}

// Allow 报告该 IP 当前是否还允许尝试(不消耗配额)。
func (l *HelloFailLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim := l.visitors[ip]
	if lim == nil {
		return true
	}
	return lim.Tokens() >= 1
}

// RecordFailure 记一次 hello 失败(消耗 1 个令牌)。
func (l *HelloFailLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim := l.visitors[ip]
	if lim == nil {
		lim = rate.NewLimiter(l.rate, l.burst)
		l.visitors[ip] = lim
	}
	l.lastSeen[ip] = time.Now()
	lim.Allow()
}

func (l *HelloFailLimiter) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.mu.Lock()
			for ip, ts := range l.lastSeen {
				if time.Since(ts) > 2*time.Minute {
					delete(l.visitors, ip)
					delete(l.lastSeen, ip)
				}
			}
			l.mu.Unlock()
		case <-l.stop:
			return
		}
	}
}

func (l *HelloFailLimiter) Stop() { l.stopOnce.Do(func() { close(l.stop) }) }
