package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// shortHash 是 SHA-256 截断(12 hex 字符),日志/ip 等敏感值的统一脱敏。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// UserHash 是 user_id 的 SHA-256 截断(12 hex 字符),日志不对齐明文(spec §9)。
func UserHash(userID string) string { return shortHash(userID) }

// 连接建立/关闭各一行(spec §9);绝不包含 payload / ticket 本体。
// origin 只记录是否通过校验,绝不记录 Origin 值本身。
func logConnect(c *wsConn) {
	origin := "none"
	if c.originPass {
		origin = "pass"
	}
	log.Printf("relay: connect user_hash=%s role=%s id=%s app_version=%s origin=%s",
		UserHash(c.userID), c.role, c.id, c.meta.AppVersion, origin)
}

func logClose(c *wsConn, reason CloseReason) {
	log.Printf("relay: close user_hash=%s role=%s id=%s reason=%s alive=%s frames_in=%d frames_out=%d",
		UserHash(c.userID), c.role, c.id, reason,
		time.Since(c.connectedAt).Round(time.Millisecond),
		c.framesIn.Load(), c.framesOut.Load())
}

// warnThrottled 节流 warn(同 key 1/min,spec §9),用于 hello 失败、
// 限流命中、slow_consumer 等异常分支。
type warnThrottled struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newWarnThrottled() *warnThrottled { return &warnThrottled{last: make(map[string]time.Time)} }

func (w *warnThrottled) Logf(key, format string, args ...any) {
	w.mu.Lock()
	if ts, ok := w.last[key]; ok && time.Since(ts) < time.Minute {
		w.mu.Unlock()
		return
	}
	w.last[key] = time.Now()
	w.mu.Unlock()
	log.Printf("relay: WARN "+format, args...)
}
