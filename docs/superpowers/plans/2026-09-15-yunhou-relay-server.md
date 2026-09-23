# Yunhou Relay Server 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 yunhou-users(Go/gin)内新增 WebSocket relay 模块,支撑 kaya 桌面端的浏览器/手机远程控制:`POST /relay/ticket` 签发短期 HMAC JWT,`GET /relay/ws` 做 hello 认证、按 user_id 房间路由、presence、JSON 帧双向透传。

**Architecture:** 单实例内存 Hub(`map[user_id]*Room` + RWMutex);relay 是哑管道,只解析信封(v/type/路由字段),payload 不透明。ticket 为 HMAC-SHA256 JWT(`aud="relay"`,TTL 300s),校验纯本地、无状态。复用现有 JWT 中间件、entitlement 判定、envelope 响应、限流器。

**Tech Stack:** Go 1.25 / gin v1.12 / `github.com/coder/websocket`(新增)/ `github.com/golang-jwt/jwt/v5`(现有)/ `github.com/prometheus/client_golang`(新增)/ `golang.org/x/time/rate`(现有)。

**Spec:** `docs/superpowers/specs/2026-09-15-yunhou-relay-server-spec.md`(已从 kaya 仓库复制到本仓库,内容一致;如冲突以 spec 的服务端行为描述为准)。

## Global Constraints

以下数值逐字来自 spec,所有 Task 默认遵守:

- relay ticket:HMAC-SHA256 JWT,`iss="yunhou-users"`,`aud="relay"`,claims 含 `sub/iat/exp/jti`;TTL **300s**;校验时钟偏移宽限 **30s**;secret 支持轮换(校验接受当前 + 上一个 secret)。
- `POST /relay/ticket`:与 /chat 同一套 access_token 校验;entitlement 不足 → 403 固定文案 `remote access requires paid plan`;签发限流 **30 次/min/user**,超限 429 带 `Retry-After`。
- `GET /relay/ws`:ticket **不**走 query string,在首帧 `hello` 携带;带 `Origin` 头的请求必须在白名单内,否则握手阶段 403;不带 Origin(非浏览器 device)不校验。
- 信封协议:JSON 文本帧、snake_case、`v=1`;不认识的 `v` → `closed reason=protocol`。
- hello:必须为首帧,**10s** 超时 → close(WS close 1008,不发帧)。
- 角色:device 必填 `device_id/device_name/app_version`;client 必填 `client_id`。同 id 重复注册 → 旧连收 `closed reason=replaced` 后关闭。
- presence:device 上线/断开(任何原因)→ 向 room 内全部 client 广播;client 加入时 `hello_ok.room_devices` 给全量在线 device;client 上下线不广播。
- 续期:exp 前 **60s** 发一次 `ticket_expiring {retry_after_ms:30000}`;`renew` 携带新 ticket;**exp + 60s 宽限**仍未 renew → `closed reason=auth` 后关闭。
- keepalive:relay 每 **30s** 发 WS 协议层 ping;读侧 **90s** 无任何帧(含 pong)判死 → `closed reason=idle_timeout`(能发尽发)。
- 限额:帧 ≤ **256 KiB**(超限 → closed protocol);帧速率 ≤ **100 帧/s/连接**(超限 → closed protocol);出站缓冲 ≤ **256 帧/连接**(溢出 → closed slow_consumer,只断慢消费者);每 user device ≤ **10**、client ≤ **20**(超限 → 拒绝 hello,closed protocol);hello 失败限流 **5 次/min/IP**(超限临时拒连)。
- close reason 枚举:`auth | entitlement | replaced | shutdown | slow_consumer | protocol | idle_timeout`。
- 优雅停机:SIGTERM → 停止接受新连接 → 全部连接发 `closed reason=shutdown` → 等待 ≤5s → 强制关闭。
- 隐私:任何位置不得持久化帧内容;payload 不得写入日志/错误响应/metrics;日志用 `user_hash`(user_id SHA-256 截断);错误响应用固定文案。
- 测试纪律:`make test` = `go test -race -cover -p 1 ./internal/...`;不用 testify,用标准库 table 风格;时序敏感测试禁止绝对墙钟阈值断言(用配置常量推导上限),所有超时常量必须可通过 Options 注入短值。

## 已决事项(executor 不得再开问)

1. **trial 用户**:与 /chat 完全同一判定(`FindActiveByUserID` 有有效订阅 + plan.IsActive + plan.Apps 含 appID),不额外区分 trial(spec §13.1 开放问题,默认对齐 chat)。
2. **renew 失败一律 `closed reason=auth`**:`entitlement` 原因保留在枚举中但 v1 不触发——relay 无状态不查库,无法区分"伪造 renew"与"付费丢失";付费丢失由 `/relay/ticket` 403 兜住,连接在 exp+60s 收敛(spec §7 认可该收敛路径)。
3. **client 的 app 帧缺 `target_device_id` → `closed reason=protocol`**(信封畸形);device 的 app 帧若带 `target_device_id` 字段则忽略,照常广播。
4. **Origin 白名单来源**:项目无 CORS 配置可派生,新增 env `RELAY_ALLOWED_ORIGINS`(逗号分隔,如 `https://www.yunhouai.com,https://www.yunhou.ai`)。**fail-closed**:白名单为空时,一切带 `Origin` 头的握手 403;不带 Origin 的连接不受影响。
5. **`RELAY_TICKET_SECRET` 为空 → relay 整体禁用**:不注册 `/relay/*` 路由,启动打一条 warn 日志(沿用项目"可选功能空值禁用"惯例,避免老环境升级时 Validate 硬失败)。
6. **device_id/client_id 不校验格式**(kaya 侧 ULID,relay 只判非空且 ≤128 字符)。
7. **ws_url 由请求推导**:`wss://<Host>/relay/ws`(X-Forwarded-Proto=https 或 TLS 时用 wss,否则 ws),不新增配置项。
8. **metrics 是 greenfield**(项目现无任何 metrics):引入 `prometheus/client_golang`,`GET /metrics` 无鉴权暴露(生产 compose 仅绑 loopback,由 nginx 决定暴露面),`env` label 取新 env `APP_ENV`(默认 `prod`)。

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/config/config.go`(改) | 新增 `RelayTicketSecret/RelayTicketSecretPrev/RelayAllowedOrigins/AppEnv` |
| `internal/middleware/ratelimit.go`(改) | 429 响应补 `Retry-After` 头 |
| `internal/service/relay_ticket.go`(新) | relay ticket 签发/校验(HMAC JWT,无状态) |
| `internal/service/relay.go`(新) | `RelayService`:entitlement 检查 + ticket 签发编排 |
| `internal/service/errors.go`(改) | 新增 `ErrRelayNoAccess`、`ErrRelayTicketInvalid` |
| `internal/relay/types.go`(新) | 信封协议类型与构造器(入站/出站帧) |
| `internal/relay/hub.go`(新) | Hub/房间/注册/踢连/presence/路由/停机 |
| `internal/relay/conn.go`(新) | 单连接状态机:hello、读写 pump、keepalive、续期定时器、关闭路径 |
| `internal/relay/limiter.go`(新) | hello 失败 per-IP 限流器(5/min) |
| `internal/relay/metrics.go`(新) | Prometheus 指标定义与注册 |
| `internal/relay/logging.go`(新) | user_hash、连接日志、节流 warn |
| `internal/handler/relay.go`(新) | gin handler:`POST /relay/ticket`、`GET /relay/ws`(Origin 校验 + Upgrade) |
| `internal/router/router.go`(改) | 注册 `/relay/ticket`、`/relay/ws`、`/metrics` |
| `cmd/server/main.go`(改) | 构造 Hub/RelayService;timeoutMiddleware skip `/relay/ws`;停机时先 `hub.Shutdown` |
| `deploy/nginx.conf`(改) | `/relay/ws` 的 Upgrade 透传 + `proxy_read_timeout 120s` |
| `.env.example`(改) | 新增 relay 相关 env |

依赖注入关系:`main.go` 持有 `*relay.Hub`;`handler.RelayHandler` 依赖 `*service.RelayService`(ticket)+ `*relay.Hub`(ws);`relay.Hub` 不依赖任何 handler/service(单向)。

---

### Task 1: 依赖、配置项、限流器 Retry-After

**Files:**
- Modify: `go.mod`(新增依赖)
- Modify: `internal/config/config.go`
- Modify: `internal/middleware/ratelimit.go`
- Modify: `.env.example`
- Test: `internal/middleware/ratelimit_test.go`(若不存在则新建)

**Interfaces:**
- Produces: `Config.RelayTicketSecret string`、`Config.RelayTicketSecretPrev string`、`Config.RelayAllowedOrigins []string`、`Config.AppEnv string`;429 响应带 `Retry-After`(Task 3 依赖)。

- [ ] **Step 1: 加依赖**

```bash
go get github.com/coder/websocket@latest
go get github.com/prometheus/client_golang@latest
go mod tidy
```

- [ ] **Step 2: 写限流器 Retry-After 的失败测试**

`internal/middleware/ratelimit_test.go`:

```go
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRateLimit429SetsRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// burst=1:第一请求放行,第二请求必触发 429
	r.GET("/x", RateLimit(context.Background(), 0.0001, 1), func(c *gin.Context) { c.Status(200) })

	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec1.Code != 200 {
		t.Fatalf("first request: got %d want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec2.Code != 429 {
		t.Fatalf("second request: got %d want 429", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatalf("429 response missing Retry-After header")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/middleware/ -run TestRateLimit429SetsRetryAfter -v`
Expected: FAIL(`429 response missing Retry-After header`)

- [ ] **Step 4: 修改 ratelimit.go 的拒绝分支**

`RateLimitWithKey` 返回的 handler 中,把 `if !limiter.allow(k)` 分支改为:

```go
	if !limiter.allow(k) {
		c.Header("Retry-After", retryAfterSeconds(limiter, k))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"code":    429,
			"message": "too many requests",
		})
		return
	}
```

并在文件尾部新增(用 Reserve 估算等待时长;预留后立即 Cancel,不消耗令牌):

```go
// retryAfterSeconds 估算 key 下次获得令牌的等待秒数(向上取整,最小 1)。
// Reserve 成功后立即 Cancel,不实际占用令牌;若突发量永远不足以放行
// (delay 超过 reservation 上限),兜底返回 60s。
func retryAfterSeconds(rl *rateLimiter, key string) string {
	v, ok := rl.visitors.Load(key)
	if !ok {
		return "1"
	}
	res := v.(*visitor).limiter.Reserve()
	if !res.OK() {
		return "60"
	}
	d := res.Delay()
	res.Cancel()
	secs := int(d.Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	return strconv.Itoa(secs)
}
```

(`import` 增加 `"strconv"`。)

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/middleware/ -v`
Expected: PASS(含旧有用例)

- [ ] **Step 6: config.go 新增字段与加载**

`Config` struct 新增(放在 JWT 相关字段附近):

```go
	// Relay(远程控制 WS 中继;RELAY_TICKET_SECRET 为空 = 整体禁用)
	RelayTicketSecret     string
	RelayTicketSecretPrev string   // 轮换期旧 secret,校验时一并接受
	RelayAllowedOrigins   []string // 浏览器 Origin 白名单(host 或 origin 形式)
	AppEnv                string   // metrics 的 env label,默认 prod
```

`Load()` 中:

```go
	cfg.RelayTicketSecret = os.Getenv("RELAY_TICKET_SECRET")
	cfg.RelayTicketSecretPrev = os.Getenv("RELAY_TICKET_SECRET_PREVIOUS")
	cfg.RelayAllowedOrigins = splitCSV(os.Getenv("RELAY_ALLOWED_ORIGINS"))
	cfg.AppEnv = envOr("APP_ENV", "prod")
```

新增 helper(放在 `envOr` 旁边):

```go
// splitCSV 解析逗号分隔 env;空串返回 nil;每项 TrimSpace 后丢弃空项。
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

`Validate()` **不**对 relay 字段做硬校验(secret 空 = 禁用,见"已决事项 5")。

- [ ] **Step 7: .env.example 追加**

```bash
# ---- Relay(kaya 远程控制 WS 中继)----
# HMAC ticket 签名 secret;留空 = relay 禁用(不注册 /relay/* 路由)
RELAY_TICKET_SECRET=
# 轮换期的上一个 secret(校验时与当前 secret 一并接受)
RELAY_TICKET_SECRET_PREVIOUS=
# 浏览器 Origin 白名单,逗号分隔;空 = 拒绝一切带 Origin 头的 WS 握手
RELAY_ALLOWED_ORIGINS=https://www.yunhouai.com,https://www.yunhou.ai
# metrics env label
APP_ENV=prod
```

- [ ] **Step 8: 编译 + 提交**

Run: `go build ./... && go test ./internal/config/ ./internal/middleware/`

```bash
git add go.mod go.sum internal/config/config.go internal/middleware/ratelimit.go internal/middleware/ratelimit_test.go .env.example
git commit -m "feat(relay): 依赖与配置项 + 429 Retry-After"
```

---

### Task 2: relay ticket 签发/校验服务

**Files:**
- Create: `internal/service/relay_ticket.go`
- Modify: `internal/service/errors.go`
- Test: `internal/service/relay_ticket_test.go`

**Interfaces:**
- Produces(Task 3/5 依赖,签名不得改):

```go
type RelayTicketService struct{ /* 不可导出字段 */ }
func NewRelayTicketService(secret, prevSecret string, ttl time.Duration) *RelayTicketService
func (s *RelayTicketService) Issue(userID string) (ticket string, expiresIn int, err error)
func (s *RelayTicketService) Verify(token string) (userID string, exp time.Time, err error)
var ErrRelayTicketInvalid error // errors.go
```

- [ ] **Step 1: errors.go 追加 sentinel**

放在现有 chat sentinel 附近:

```go
	// relay ticket 校验失败(伪造/篡改/过期/错误 aud/错误 secret 统一口径,
	// 不向连接方泄露具体原因)。
	ErrRelayTicketInvalid = errors.New("invalid relay ticket")
```

- [ ] **Step 2: 写失败测试**

`internal/service/relay_ticket_test.go`:

```go
package service

import (
	"testing"
	"time"
)

const (
	testRelaySecret     = "test-secret-current-0123456789abcdef"
	testRelaySecretPrev = "test-secret-previous-0123456789abcdef"
)

func TestRelayTicketIssueVerifyRoundTrip(t *testing.T) {
	svc := NewRelayTicketService(testRelaySecret, "", 300*time.Second)
	tok, expiresIn, err := svc.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if expiresIn != 300 {
		t.Fatalf("expiresIn = %d, want 300", expiresIn)
	}
	userID, exp, err := svc.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if userID != "user-1" {
		t.Fatalf("userID = %q, want user-1", userID)
	}
	if d := time.Until(exp); d < 250*time.Second || d > 330*time.Second {
		t.Fatalf("exp off: %v", d)
	}
}

func TestRelayTicketVerifyRejects(t *testing.T) {
	svc := NewRelayTicketService(testRelaySecret, testRelaySecretPrev, 300*time.Second)
	good, _, _ := svc.Issue("user-1")

	cases := map[string]string{
		"空 token":       "",
		"非 JWT":        "not-a-jwt",
		"错误 secret 签名": mustSignRelayTicket(t, "wrong-secret", "user-1", time.Now().Add(300*time.Second)),
		"已过期(超宽限)": mustSignRelayTicket(t, testRelaySecret, "user-1", time.Now().Add(-time.Minute)),
		"错误 aud":      mustSignRelayTicketAud(t, testRelaySecret, "user-1", "chat"),
	}
	for name, tok := range cases {
		if _, _, err := svc.Verify(tok); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
	// 篡改 payload 段
	tampered := good[:len(good)-4] + "AAAA"
	if _, _, err := svc.Verify(tampered); err == nil {
		t.Fatalf("篡改: expected error, got nil")
	}
}

func TestRelayTicketSecretRotation(t *testing.T) {
	old := NewRelayTicketService(testRelaySecretPrev, "", 300*time.Second)
	tok, _, err := old.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue with old secret: %v", err)
	}
	// 新实例:当前 secret 已轮换,旧 secret 仍应通过
	rotated := NewRelayTicketService(testRelaySecret, testRelaySecretPrev, 300*time.Second)
	if _, _, err := rotated.Verify(tok); err != nil {
		t.Fatalf("prev secret should verify: %v", err)
	}
}
```

`mustSignRelayTicket` / `mustSignRelayTicketAud` 两个 helper 也写在测试文件里(用 jwt/v5 直接签名,构造合法形状但特定字段错误的 token):

```go
func mustSignRelayTicket(t *testing.T, secret, userID string, exp time.Time) string {
	t.Helper()
	return mustSignRelayTicketAud(t, secret, userID, "relay", exp)
}

func mustSignRelayTicketAud(t *testing.T, secret, userID, aud string, exp ...time.Time) string {
	t.Helper()
	e := time.Now().Add(300 * time.Second)
	if len(exp) > 0 {
		e = exp[0]
	}
	claims := jwt.RegisteredClaims{
		Issuer:    "yunhou-users",
		Audience:  jwt.ClaimStrings{aud},
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(e),
		ID:        uuid.New().String(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}
```

注意:上面 `mustSignRelayTicket` 调用 `mustSignRelayTicketAud(t, secret, userID, "relay", exp)` 是可变参数形态,编译时两个 helper 签名以本段代码为准(executor 需保证编译通过,可微调调用形态,但测试语义不得变)。

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/service/ -run TestRelayTicket -v`
Expected: FAIL(compile error:`NewRelayTicketService` undefined)

- [ ] **Step 4: 实现 relay_ticket.go**

```go
package service

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// relayTicketAudience 是 relay ticket 的唯一 aud;access_token 的 aud 是
// app_id,二者互不相通,access_token 不能冒充 relay ticket。
const relayTicketAudience = "relay"

// relayTicketLeeway 是校验 exp/iat 时的时钟偏移宽限(spec §4:30s)。
const relayTicketLeeway = 30 * time.Second

// RelayTicketService 签发/校验 relay ticket(HMAC-SHA256 JWS)。
// 校验是纯本地验签,不查库、无状态(spec §4)。secret 支持轮换:
// 校验时先尝当前 secret,失败再尝 prevSecret。
type RelayTicketService struct {
	secret     []byte
	prevSecret []byte
	ttl        time.Duration
	now        func() time.Time // 测试可注入;nil 用 time.Now
}

func NewRelayTicketService(secret, prevSecret string, ttl time.Duration) *RelayTicketService {
	return &RelayTicketService{
		secret:     []byte(secret),
		prevSecret: []byte(prevSecret),
		ttl:        ttl,
		now:        time.Now,
	}
}

// Issue 为 userID 签发一张 relay ticket,返回 token 与有效期秒数。
func (s *RelayTicketService) Issue(userID string) (string, int, error) {
	now := s.now()
	claims := jwt.RegisteredClaims{
		Issuer:    tokenIssuer, // 复用 token.go 的 "yunhou-users" 常量;若该常量名不同,用字面值 "yunhou-users"
		Audience:  jwt.ClaimStrings{relayTicketAudience},
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		ID:        uuid.New().String(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", 0, err
	}
	return tok, int(s.ttl.Seconds()), nil
}

// Verify 校验 ticket,成功返回 user_id 与 exp;任何失败统一为
// ErrRelayTicketInvalid(不泄露具体原因)。
func (s *RelayTicketService) Verify(token string) (string, time.Time, error) {
	parse := func(secret []byte) (*jwt.RegisteredClaims, error) {
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(token, claims,
			func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, ErrRelayTicketInvalid
				}
				return secret, nil
			},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithAudience(relayTicketAudience),
			jwt.WithIssuer("yunhou-users"),
			jwt.WithLeeway(relayTicketLeeway),
			jwt.WithExpirationRequired(),
		)
		return claims, err
	}

	claims, err := parse(s.secret)
	if err != nil && len(s.prevSecret) > 0 {
		claims, err = parse(s.prevSecret)
	}
	if err != nil || claims.Subject == "" {
		return "", time.Time{}, ErrRelayTicketInvalid
	}
	return claims.Subject, claims.ExpiresAt.Time, nil
}
```

(executor 先查 `internal/service/token.go` 里 issuer 常量的真实名字——若存在常量则复用,否则用字面值 `"yunhou-users"`;jwt v5 的 `WithExpirationRequired` 若版本不支持则去掉,RegisteredClaims 的 exp 校验默认已覆盖。)

- [ ] **Step 5: 跑测试确认通过 + 提交**

Run: `go test ./internal/service/ -run TestRelayTicket -v`
Expected: PASS

```bash
git add internal/service/relay_ticket.go internal/service/relay_ticket_test.go internal/service/errors.go
git commit -m "feat(relay): HMAC relay ticket 签发/校验服务"
```

---

### Task 3: RelayService.CheckAccess + `POST /relay/ticket`

**Files:**
- Create: `internal/service/relay.go`
- Create: `internal/handler/relay.go`(本 Task 只写 ticket 部分,WS 部分在 Task 5 补)
- Modify: `internal/router/router.go`
- Modify: `cmd/server/main.go`(仅构造与路由实参,停机接线在 Task 8)
- Test: `internal/service/relay_test.go`、`internal/handler/relay_test.go`

**Interfaces:**
- Consumes: Task 2 的 `RelayTicketService`;`repo.SubscriptionRepo.FindActiveByUserID`、`repo.PlanRepo.FindByID`;`middleware.JWTAuth`、`middleware.ContextUserID/ContextAppID`。
- Produces:

```go
// service
var ErrRelayNoAccess error // "remote access requires paid plan"
type RelayService struct{ /* 不可导出字段 */ }
func NewRelayService(subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo, tickets *RelayTicketService) *RelayService
func (s *RelayService) CheckAccess(ctx context.Context, userID, appID string) error
func (s *RelayService) IssueTicket(userID string) (ticket string, expiresIn int, err error)

// handler
type RelayHandler struct{ /* 不可导出字段 */ }
func NewRelayHandler(svc *service.RelayService) *RelayHandler
func (h *RelayHandler) IssueTicket(c *gin.Context)
// SetHub 在 Task 5 追加:func (h *RelayHandler) SetHub(hub *relay.Hub)
```

- [ ] **Step 1: 写 service 失败测试**

`internal/service/relay_test.go`(mock 风格照抄 `internal/service/mock_test.go` 的 `mockSubscriptionRepo`/`mockPlanRepo`;若已存在直接复用,不要重复定义):

```go
func TestRelayCheckAccess(t *testing.T) {
	now := time.Now()
	activePlan := &model.Plan{ID: "p1", IsActive: true, Apps: pq.StringArray{"yunhou-website"}}
	future := now.Add(24 * time.Hour)

	cases := []struct {
		name    string
		sub     *model.Subscription
		subErr  error
		plan    *model.Plan
		wantErr error
	}{
		{"有效订阅放行", &model.Subscription{UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: &future}, nil, activePlan, nil},
		{"无订阅拒绝", nil, sql.ErrNoRows, activePlan, ErrRelayNoAccess},
		{"订阅过期拒绝", &model.Subscription{UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: ptrTime(now.Add(-time.Hour))}, nil, activePlan, ErrRelayNoAccess},
		{"plan 停用拒绝", &model.Subscription{UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: &future}, nil, &model.Plan{ID: "p1", IsActive: false}, ErrRelayNoAccess},
		{"plan 不含该 app 拒绝", &model.Subscription{UserID: "u1", PlanID: "p1", Status: "active", ExpiresAt: &future}, nil, &model.Plan{ID: "p1", IsActive: true, Apps: pq.StringArray{"other-app"}}, ErrRelayNoAccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewRelayService(
				&mockSubscriptionRepo{sub: tc.sub, err: tc.subErr},
				&mockPlanRepo{plan: tc.plan},
				NewRelayTicketService(testRelaySecret, "", 300*time.Second),
			)
			err := svc.CheckAccess(context.Background(), "u1", "yunhou-website")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}
```

(mock 字段名以 `mock_test.go` 实际定义为准;`model.Subscription` 字段名以 `internal/model` 实际为准——executor 先读这两个文件对齐,语义不得变。)

- [ ] **Step 2: 实现 service/relay.go**

```go
package service

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/yunhou/users/internal/repo"
)

// relayAccessTimeout 与 chat 的 access 读库超时同口径(chat.go 的
// chatAccessTimeout = 10s)。
const relayAccessTimeout = 10 * time.Second

// RelayService 编排 relay 的 entitlement 判定与 ticket 签发。
// entitlement 语义与 /chat 完全一致(见已决事项 1)。
type RelayService struct {
	subRepo  repo.SubscriptionRepo
	planRepo repo.PlanRepo
	tickets  *RelayTicketService
}

func NewRelayService(subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo, tickets *RelayTicketService) *RelayService {
	return &RelayService{subRepo: subRepo, planRepo: planRepo, tickets: tickets}
}

// CheckAccess 校验用户是否有远程功能权限;无权限返回 ErrRelayNoAccess。
func (s *RelayService) CheckAccess(ctx context.Context, userID, appID string) error {
	ctx, cancel := context.WithTimeout(ctx, relayAccessTimeout)
	defer cancel()

	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelayNoAccess
		}
		return err
	}
	if sub == nil || (sub.ExpiresAt != nil && sub.ExpiresAt.Before(time.Now())) {
		return ErrRelayNoAccess
	}
	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRelayNoAccess
		}
		return err
	}
	if plan == nil || !plan.IsActive || !slices.Contains(plan.Apps, appID) {
		return ErrRelayNoAccess
	}
	return nil
}

func (s *RelayService) IssueTicket(userID string) (string, int, error) {
	return s.tickets.Issue(userID)
}
```

errors.go 追加:`ErrRelayNoAccess = errors.New("remote access requires paid plan")`(spec §3.1 固定文案)。

- [ ] **Step 3: 跑 service 测试确认通过**

Run: `go test ./internal/service/ -run TestRelayCheckAccess -v`
Expected: PASS

- [ ] **Step 4: 写 handler 失败测试**

`internal/handler/relay_test.go`(router 注入模式照抄 `chat_test.go` 的 `chatTestRouter`):

```go
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
	svc := newRelayServiceForHandlerTest(t, nil) // helper:mock repo 放行
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
	svc := newRelayServiceForHandlerTest(t, service.ErrRelayNoAccess) // helper:CheckAccess 拒绝
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
```

(`newRelayServiceForHandlerTest` helper 自行实现:用 mock repo 构造放行/拒绝两种 RelayService;`testRelaySecretForHandler` 用测试常量。)

- [ ] **Step 5: 实现 handler/relay.go(ticket 部分)**

```go
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/service"
)

// RelayHandler 承载 relay 模块的 HTTP 入口。WS 升级逻辑见 serveWS(Task 5)。
type RelayHandler struct {
	svc *service.RelayService
}

func NewRelayHandler(svc *service.RelayService) *RelayHandler {
	return &RelayHandler{svc: svc}
}

// IssueTicket 处理 POST /relay/ticket:access_token(经 JWTAuth 中间件)+
// entitlement → 签发 300s relay ticket。
func (h *RelayHandler) IssueTicket(c *gin.Context) {
	userID := c.GetString(middleware.ContextUserID)
	appID := c.GetString(middleware.ContextAppID)

	if err := h.svc.CheckAccess(c.Request.Context(), userID, appID); err != nil {
		if errors.Is(err, service.ErrRelayNoAccess) {
			c.JSON(http.StatusForbidden, gin.H{"code": 403, "data": nil, "message": service.ErrRelayNoAccess.Error()})
			return
		}
		// 固定文案,不泄露内部错误(与 /chat 先例一致)
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "data": nil, "message": "internal error"})
		return
	}

	ticket, expiresIn, err := h.svc.IssueTicket(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "data": nil, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"ticket":     ticket,
		"expires_in": expiresIn,
		"ws_url":     relayWSURL(c),
	}, "message": "ok"})
}

// relayWSURL 由请求推导 WS 地址(已决事项 7)。
func relayWSURL(c *gin.Context) string {
	scheme := "ws"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "wss"
	}
	return scheme + "://" + c.Request.Host + "/relay/ws"
}
```

- [ ] **Step 6: 跑 handler 测试确认通过**

Run: `go test ./internal/handler/ -run TestRelayTicket -v`
Expected: PASS

- [ ] **Step 7: 路由注册**

`router.Setup` 签名追加参数(放最后):`relaySvc *service.RelayService`。在 chat 路由附近追加:

```go
	// Relay(kaya 远程控制)。relaySvc 为 nil = relay 禁用(RELAY_TICKET_SECRET 未配置)。
	if relaySvc != nil {
		relayHandler := handler.NewRelayHandler(relaySvc)
		// 签发限流 30/min/user(spec §3.1):r=0.5/s,burst=30。
		// JWTAuth 在前,key func 才能读到 user_id。
		ticketLimiter := middleware.RateLimitWithKey(ctx, 0.5, 30, func(c *gin.Context) string {
			return c.GetString(middleware.ContextUserID)
		})
		engine.POST("/relay/ticket", middleware.JWTAuth(tokenSvc), ticketLimiter, relayHandler.IssueTicket)
		// /relay/ws 在 Task 5 注册。
	}
```

`cmd/server/main.go`:构造 `relaySvc`(仅当 `cfg.RelayTicketSecret != ""`,否则 nil 并 `log.Printf("relay: disabled (RELAY_TICKET_SECRET empty)")`),传给 `router.Setup`。构造代码:

```go
	var relaySvc *service.RelayService
	if cfg.RelayTicketSecret != "" {
		relaySvc = service.NewRelayService(subRepo, planRepo,
			service.NewRelayTicketService(cfg.RelayTicketSecret, cfg.RelayTicketSecretPrev, 300*time.Second))
	} else {
		log.Printf("relay: disabled (RELAY_TICKET_SECRET empty)")
	}
```

- [ ] **Step 8: 全量编译测试 + 提交**

Run: `go build ./... && go test ./internal/service/ ./internal/handler/`

```bash
git add internal/service/relay.go internal/service/relay_test.go internal/service/errors.go internal/handler/relay.go internal/handler/relay_test.go internal/router/router.go cmd/server/main.go
git commit -m "feat(relay): POST /relay/ticket(entitlement + 30/min 限流)"
```

---

### Task 4: relay 包——协议类型 + Hub 房间路由核心

**Files:**
- Create: `internal/relay/types.go`
- Create: `internal/relay/hub.go`
- Test: `internal/relay/hub_test.go`

**Interfaces:**
- Consumes: 无(纯内存逻辑;`Conn` 接口使 Task 5 的 WS 实现与测试 fake 可互换)。
- Produces(Task 5/6 依赖,签名不得改):

```go
// types.go
const ProtocolVersion = 1
type Role string            // RoleDevice / RoleClient
type CloseReason string     // ReasonAuth/ReasonEntitlement/ReasonReplaced/ReasonShutdown/ReasonSlowConsumer/ReasonProtocol/ReasonIdleTimeout
type HelloFrame / RenewFrame / AppFrame        // 入站
type DeviceInfo struct { DeviceID, DeviceName, AppVersion string; ConnectedAt int64 }
func HelloOKFrame(devices []DeviceInfo, serverTime int64) []byte
func PresenceFrame(deviceID string, online bool, meta *DeviceInfo) []byte
func TicketExpiringFrame(retryAfterMS int) []byte
func AppFromDeviceFrame(deviceID string, payload json.RawMessage) []byte
func AppFromClientFrame(clientID string, payload json.RawMessage) []byte
func UndeliverableFrame(targetDeviceID string) []byte
func ClosedFrame(reason CloseReason) []byte

// hub.go
type Conn interface {
	UserID() string
	GetRole() Role
	DeviceID() string   // device 角色有效
	ClientID() string   // client 角色有效
	DeviceMeta() DeviceInfo
	Enqueue(frame []byte) // 非阻塞;出站缓冲满时内部触发该连接的 slow_consumer 关闭
}
type Options struct { /* 见 Step 3,全部时序/限额可注入 */ }
type Hub struct{ /* 不可导出字段 */ }
func NewHub(opts Options) *Hub
func (h *Hub) Register(c Conn) (kicked Conn, err error)  // err 为 *RejectError(限额/停机中)
func (h *Hub) Unregister(c Conn)
func (h *Hub) RouteApp(from Conn, targetDeviceID string, payload json.RawMessage)
func (h *Hub) Shutdown(wait time.Duration)
func (h *Hub) ShutdownStarted() bool
type RejectError struct{ Reason CloseReason }; func (e *RejectError) Error() string
```

- [ ] **Step 1: 写 types.go(完整实现,无需先行测试——纯构造器)**

```go
package relay

import "encoding/json"

// ProtocolVersion 是当前信封协议版本(spec §6)。不认识的 v → closed protocol。
const ProtocolVersion = 1

type Role string

const (
	RoleDevice Role = "device"
	RoleClient Role = "client"
)

// CloseReason 是 closed 帧的 reason 枚举(spec §6.2)。
type CloseReason string

const (
	ReasonAuth         CloseReason = "auth"
	ReasonEntitlement  CloseReason = "entitlement" // v1 保留不触发(已决事项 2)
	ReasonReplaced     CloseReason = "replaced"
	ReasonShutdown     CloseReason = "shutdown"
	ReasonSlowConsumer CloseReason = "slow_consumer"
	ReasonProtocol     CloseReason = "protocol"
	ReasonIdleTimeout  CloseReason = "idle_timeout"
)

// ---- 入站帧(连接方 → relay)----

// HelloFrame 必须为首帧。device 必填 DeviceID/DeviceName/AppVersion;
// client 必填 ClientID。
type HelloFrame struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	Ticket     string `json:"ticket"`
	Role       Role   `json:"role"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	ClientName string `json:"client_name,omitempty"`
}

type RenewFrame struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	Ticket string `json:"ticket"`
}

// AppFrame 是业务帧;Payload 对 relay 不透明(不解析、不记录)。
type AppFrame struct {
	V              int             `json:"v"`
	Type           string          `json:"type"`
	TargetDeviceID string          `json:"target_device_id,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

// typePeek 只解析信封外层,用于按 type 分发。
type typePeek struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// PeekType 解析帧的 v/type;坏 JSON 返回错误。
func PeekType(frame []byte) (v int, typ string, err error) {
	var p typePeek
	if err := json.Unmarshal(frame, &p); err != nil {
		return 0, "", err
	}
	return p.V, p.Type, nil
}

// ---- 出站帧(relay → 连接方)----

type DeviceInfo struct {
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	ConnectedAt int64  `json:"connected_at"`
}

func marshalFrame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil { // 构造器入参全部受控,不可达;防御性兜底
		return []byte(`{"v":1,"type":"closed","reason":"protocol"}`)
	}
	return b
}

func HelloOKFrame(devices []DeviceInfo, serverTime int64) []byte {
	if devices == nil {
		devices = []DeviceInfo{}
	}
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "hello_ok",
		"room_devices": devices, "server_time": serverTime,
	})
}

// PresenceFrame:online=true 时 meta 必填;false 时 meta 省略(spec §6.2)。
func PresenceFrame(deviceID string, online bool, meta *DeviceInfo) []byte {
	f := map[string]any{
		"v": ProtocolVersion, "type": "presence",
		"device_id": deviceID, "online": online,
	}
	if online && meta != nil {
		f["meta"] = map[string]any{
			"device_name": meta.DeviceName, "app_version": meta.AppVersion,
		}
	}
	return marshalFrame(f)
}

func TicketExpiringFrame(retryAfterMS int) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "ticket_expiring", "retry_after_ms": retryAfterMS,
	})
}

func AppFromDeviceFrame(deviceID string, payload json.RawMessage) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "app",
		"from_device_id": deviceID, "payload": payload,
	})
}

func AppFromClientFrame(clientID string, payload json.RawMessage) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "app",
		"from_client_id": clientID, "payload": payload,
	})
}

func UndeliverableFrame(targetDeviceID string) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "undeliverable", "target_device_id": targetDeviceID,
	})
}

func ClosedFrame(reason CloseReason) []byte {
	return marshalFrame(map[string]any{
		"v": ProtocolVersion, "type": "closed", "reason": string(reason),
	})
}
```

- [ ] **Step 2: 写 hub 失败测试(先覆盖路由与 presence 核心语义)**

`internal/relay/hub_test.go`,用 fakeConn:

```go
type fakeConn struct {
	userID   string
	role     Role
	deviceID string
	clientID string
	meta     DeviceInfo

	mu     sync.Mutex
	frames [][]byte
}

func (f *fakeConn) UserID() string      { return f.userID }
func (f *fakeConn) GetRole() Role       { return f.role }
func (f *fakeConn) DeviceID() string    { return f.deviceID }
func (f *fakeConn) ClientID() string    { return f.clientID }
func (f *fakeConn) DeviceMeta() DeviceInfo { return f.meta }
func (f *fakeConn) Enqueue(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	f.frames = append(f.frames, cp)
}
func (f *fakeConn) decoded(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.frames[i], &m); err != nil {
		t.Fatalf("frame %d: %v", i, err)
	}
	return m
}
func (f *fakeConn) frameCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.frames) }
```

测试用例(每个都是独立 `func Test...`,table 驱动不适用——场景差异大):

1. `TestHubRegisterKickSameDeviceID`:同 user 同 device_id 两个 fakeConn,第二次 `Register` 返回的 `kicked` 是第一个 conn;第一个 conn 随后应收到 `closed replaced`(由 Hub 在返回 kicked 后 `kicked.Enqueue(ClosedFrame(ReasonReplaced))`——见 Step 4 语义)。
2. `TestHubDevicePresenceBroadcast`:先注册 2 个 client,再注册 device → 两个 client 各收 1 帧 presence online(true, meta 带 device_name);`Unregister(device)` → 两个 client 各再收 presence online=false。
3. `TestHubHelloOKDeviceList`:`RoomSnapshot(userID)` 或经 Register 路径…… 为可测,Hub 提供 `OnlineDevices(userID string) []DeviceInfo`(内部方法,导出仅供 hello_ok 组装);注册 2 个 device 后 `OnlineDevices` 返回 2 条,按 device_id 排序(保证测试稳定)。
4. `TestHubRouteDeviceBroadcast`:device 发 app(payload `{"a":1}`)→ room 内 2 个 client 都收到,逐字节等于 `AppFromDeviceFrame(deviceID, payload)`;其它 user 的 room 内 client 收不到。
5. `TestHubRouteClientToDevice`:client 发 app 指定 target → 目标 device 收到 `AppFromClientFrame`;room 内另一 device 收不到。
6. `TestHubRouteUndeliverable`:client 指定不存在的 device_id → 只有发送方 client 收到 undeliverable;**跨 room 场景**:A 用户的 client 指定 B 用户的 device_id → 同样 undeliverable(B 的 device 收不到,存在性不泄露)。
7. `TestHubConnectionCaps`:Options{MaxDevices: 2, MaxClients: 2} 下注册第 3 个 device → `Register` 返回 `*RejectError{Reason: ReasonProtocol}`;client 同理。
8. `TestHubUnregisterSameIDNewerConnSafe`:旧连被踢后,旧连延迟触发的 `Unregister(旧)` **不得**把新连从房间移除( unregister 时比对 map 里存的是不是自己)。
9. `TestHubShutdown`:`Shutdown(100ms)` 后:`ShutdownStarted()` 为 true;所有连接收到 `closed shutdown`;后续 `Register` 返回 `*RejectError{Reason: ReasonShutdown}`。

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/relay/ -v`
Expected: FAIL(compile error)

- [ ] **Step 4: 实现 hub.go**

```go
package relay

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Options 汇总全部时序/限额参数(spec §8),测试注入短值。
type Options struct {
	MaxFrameBytes    int           // 256 << 10
	MaxFramesPerSec  float64       // 100
	OutboundBuffer   int           // 256
	MaxDevicesPerUser int          // 10
	MaxClientsPerUser int          // 20
	HelloTimeout     time.Duration // 10s
	PingInterval     time.Duration // 30s
	ReadIdleTimeout  time.Duration // 90s(经 ping/pong 探测,见 conn.go)
	TicketExpiringLead time.Duration // 60s:exp 前多久发 ticket_expiring
	RenewGrace       time.Duration // 60s:exp 后未 renew 的宽限
	TicketRetryAfterMS int         // 30000
	MaxIDLen         int           // 128:device_id/client_id 长度上限
}

// DefaultOptions 返回 spec §8 的正式值。
func DefaultOptions() Options {
	return Options{
		MaxFrameBytes:      256 << 10,
		MaxFramesPerSec:    100,
		OutboundBuffer:     256,
		MaxDevicesPerUser:  10,
		MaxClientsPerUser:  20,
		HelloTimeout:       10 * time.Second,
		PingInterval:       30 * time.Second,
		ReadIdleTimeout:    90 * time.Second,
		TicketExpiringLead: 60 * time.Second,
		RenewGrace:         60 * time.Second,
		TicketRetryAfterMS: 30000,
		MaxIDLen:           128,
	}
}

// RejectError 是 Register 的拒绝原因(连接数超限 / 停机中)。
type RejectError struct{ Reason CloseReason }

func (e *RejectError) Error() string { return "relay: connection rejected: " + string(e.Reason) }

type room struct {
	devices map[string]Conn
	clients map[string]Conn
}

// Hub 是单实例内存房间路由中心(spec §5.1)。不解析业务载荷。
type Hub struct {
	opts     Options
	mu       sync.RWMutex
	rooms    map[string]*room
	shutdown atomic.Bool
}

func NewHub(opts Options) *Hub {
	return &Hub{opts: opts, rooms: make(map[string]*room)}
}

func (h *Hub) Options() Options { return h.opts }

func (h *Hub) ShutdownStarted() bool { return h.shutdown.Load() }

// Register 把连接注册进 user 的房间;同角色同 id 踢旧连(给旧连发
// closed replaced,关闭动作由旧连自己的 Enqueue 实现触发)。
// 返回被踢的旧连(无则 nil);拒绝时返回 *RejectError。
func (h *Hub) Register(c Conn) (Conn, error) {
	if h.ShutdownStarted() {
		return nil, &RejectError{Reason: ReasonShutdown}
	}

	h.mu.Lock()
	r := h.rooms[c.UserID()]
	if r == nil {
		r = &room{devices: make(map[string]Conn), clients: make(map[string]Conn)}
		h.rooms[c.UserID()] = r
	}

	var kicked Conn
	switch c.GetRole() {
	case RoleDevice:
		if _, exists := r.devices[c.DeviceID()]; !exists && len(r.devices) >= h.opts.MaxDevicesPerUser {
			h.mu.Unlock()
			return nil, &RejectError{Reason: ReasonProtocol}
		}
		kicked = r.devices[c.DeviceID()]
		r.devices[c.DeviceID()] = c
	case RoleClient:
		if _, exists := r.clients[c.ClientID()]; !exists && len(r.clients) >= h.opts.MaxClientsPerUser {
			h.mu.Unlock()
			return nil, &RejectError{Reason: ReasonProtocol}
		}
		kicked = r.clients[c.ClientID()]
		r.clients[c.ClientID()] = c
	}

	// 收集 presence 广播目标(device 上线才广播)
	var clients []Conn
	if c.GetRole() == RoleDevice {
		for _, cl := range r.clients {
			clients = append(clients, cl)
		}
	}
	h.mu.Unlock()

	if kicked != nil && kicked != c {
		kicked.Enqueue(ClosedFrame(ReasonReplaced))
	}
	if c.GetRole() == RoleDevice {
		meta := c.DeviceMeta()
		frame := PresenceFrame(c.DeviceID(), true, &meta)
		for _, cl := range clients {
			cl.Enqueue(frame)
		}
	}
	return kicked, nil
}

// Unregister 摘除连接;device 断开往 room 内全部 client 广播 presence
// offline。仅当 map 里存的仍是该连接时才摘除(防旧连踢掉新连)。
func (h *Hub) Unregister(c Conn) {
	h.mu.Lock()
	r := h.rooms[c.UserID()]
	if r == nil {
		h.mu.Unlock()
		return
	}
	removed := false
	if c.GetRole() == RoleDevice {
		if cur, ok := r.devices[c.DeviceID()]; ok && cur == c {
			delete(r.devices, c.DeviceID())
			removed = true
		}
	} else {
		if cur, ok := r.clients[c.ClientID()]; ok && cur == c {
			delete(r.clients, c.ClientID())
			removed = true
		}
	}
	var clients []Conn
	if removed && c.GetRole() == RoleDevice {
		for _, cl := range r.clients {
			clients = append(clients, cl)
		}
	}
	if len(r.devices) == 0 && len(r.clients) == 0 {
		delete(h.rooms, c.UserID())
	}
	h.mu.Unlock()

	if removed && c.GetRole() == RoleDevice {
		frame := PresenceFrame(c.DeviceID(), false, nil)
		for _, cl := range clients {
			cl.Enqueue(frame)
		}
	}
}

// OnlineDevices 返回 user 房间当前在线 device 全量表(hello_ok 用)。
// 按 device_id 排序保证输出稳定。
func (h *Hub) OnlineDevices(userID string) []DeviceInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.rooms[userID]
	if r == nil {
		return nil
	}
	out := make([]DeviceInfo, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d.DeviceMeta())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

// RouteApp 按 spec §6 路由业务帧:
//   - device → 广播给 room 内全部 client(附 from_device_id)
//   - client → 指定 device(附 from_client_id);不在线 → 仅回发送方 undeliverable
func (h *Hub) RouteApp(from Conn, targetDeviceID string, payload json.RawMessage) {
	h.mu.RLock()
	r := h.rooms[from.UserID()]
	switch from.GetRole() {
	case RoleDevice:
		if r == nil {
			h.mu.RUnlock()
			return
		}
		frame := AppFromDeviceFrame(from.DeviceID(), payload)
		targets := make([]Conn, 0, len(r.clients))
		for _, cl := range r.clients {
			targets = append(targets, cl)
		}
		h.mu.RUnlock()
		for _, cl := range targets {
			cl.Enqueue(frame)
		}
	case RoleClient:
		var dev Conn
		if r != nil {
			dev = r.devices[targetDeviceID]
		}
		h.mu.RUnlock()
		if dev == nil {
			from.Enqueue(UndeliverableFrame(targetDeviceID))
			return
		}
		dev.Enqueue(AppFromClientFrame(from.ClientID(), payload))
	}
}

// Shutdown 停止接受新连接,给全部连接发 closed shutdown,并在 wait
// 内按房间广播 device presence offline(停机也触发 presence,spec §5.3)。
// 连接的实际关闭由各 conn 的 Enqueue 实现完成。
func (h *Hub) Shutdown(wait time.Duration) {
	h.shutdown.Store(true)
	h.mu.RLock()
	var conns []Conn
	type devRef struct {
		id     string
		targets []Conn
	}
	h.mu.RUnlock()

	h.mu.Lock()
	for _, r := range h.rooms {
		for _, d := range r.devices {
			conns = append(conns, d)
		}
		for _, c := range r.clients {
			conns = append(conns, c)
		}
	}
	rooms := h.rooms
	h.mu.Unlock()

	closedFrame := ClosedFrame(ReasonShutdown)
	for _, c := range conns {
		c.Enqueue(closedFrame)
	}
	// presence offline:device → 其 room 的 clients(此时 client 也已在关闭,
	// 能发尽发)。
	for _, r := range rooms {
		for _, d := range r.devices {
			frame := PresenceFrame(d.DeviceID(), false, nil)
			for _, c := range r.clients {
				c.Enqueue(frame)
			}
		}
	}
	_ = wait // 实际等待在 conn 层(各自 flush 后关闭);Hub 无需睡眠
	_ = devRef{} // 占位防误删:若按此结构重构广播可移除
}
```

(executor 注意:`Shutdown` 里 `devRef`/`wait` 的占位写法应清理——直接简化实现,保证测试语义即可;`sort` import 别漏。)

- [ ] **Step 5: 跑测试确认通过 + 提交**

Run: `go test ./internal/relay/ -v -race`
Expected: 全部 PASS

```bash
git add internal/relay/types.go internal/relay/hub.go internal/relay/hub_test.go
git commit -m "feat(relay): 信封协议类型 + Hub 房间路由核心"
```

---

### Task 5: WS 连接生命周期(conn.go)+ `GET /relay/ws`

**Files:**
- Create: `internal/relay/conn.go`
- Create: `internal/relay/limiter.go`(hello 失败 per-IP 限流)
- Modify: `internal/handler/relay.go`(追加 serveWS + Origin 校验 + SetHub)
- Modify: `internal/router/router.go`(注册 `/relay/ws`)
- Modify: `cmd/server/main.go`(构造 Hub 并传入)
- Test: `internal/relay/conn_test.go`(真实 WS client 端到端)

**Interfaces:**
- Consumes: Task 4 的 `Hub/Conn/Options/types`;Task 2 的 `RelayTicketService`;Task 1 的 `Config.RelayAllowedOrigins`。
- Produces:

```go
// relay 包
func (h *Hub) ServeWS(ctx context.Context, nc net.Conn-ish...) // 不用此形态,见下
// 实际入口:
func HandleWS(w http.ResponseWriter, r *http.Request, hub *Hub, tickets ticketVerifier, helloFails *HelloFailLimiter) 
// 其中 ticketVerifier 是内部接口:type ticketVerifier interface { Verify(string) (string, time.Time, error) }
// (*service.RelayTicketService 天然满足;relay 包不 import service,避免循环依赖)

// limiter.go
type HelloFailLimiter struct{ /* 不可导出 */ }
func NewHelloFailLimiter(maxPerMin int) *HelloFailLimiter // 5
func (l *HelloFailLimiter) Allow(ip string) bool
func (l *HelloFailLimiter) RecordFailure(ip string)

// handler
func (h *RelayHandler) SetHub(hub *relay.Hub, allowedOrigins []string)
func (h *RelayHandler) ServeWS(c *gin.Context)
```

- [ ] **Step 1: 实现 limiter.go(hello 失败限流,5 次/min/IP)**

复用 `golang.org/x/time/rate`,与 middleware 同款 visitor 表 + janitor(精简版):

```go
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
```

- [ ] **Step 2: 写 conn 失败测试(端到端,真实 WS client)**

`internal/relay/conn_test.go`。测试基建:

```go
// wsTestServer 起 httptest.Server,挂 relay 的 WS handler 的 http.HandlerFunc 形态。
// 测试用短超时 Options:HelloTimeout=300ms, PingInterval=100ms, ReadIdleTimeout=300ms,
// TicketExpiringLead/RenewGrace 配合短 TTL ticket 使用。
type wsTestEnv struct {
	server *httptest.Server
	hub    *Hub
	tickets *service.RelayTicketService // 或本地 stub 实现 ticketVerifier
	fails  *HelloFailLimiter
}

func newWSTestEnv(t *testing.T, opts Options, ticketTTL time.Duration) *wsTestEnv { /* ... */ }

func (e *wsTestEnv) dial(t *testing.T, origin string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	dialOpts := &websocket.DialOptions{}
	if origin != "" {
		dialOpts.HTTPHeader = http.Header{"Origin": []string{origin}}
	}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.server.URL, "http")+"/relay/ws", dialOpts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func writeFrame(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := json.Marshal(v)
	if err != nil { t.Fatalf("marshal: %v", err) }
	if err := c.Write(ctx, websocket.MessageText, b); err != nil { t.Fatalf("write: %v", err) }
}

func readFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil { t.Fatalf("read: %v", err) }
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil { t.Fatalf("unmarshal: %v", err) }
	return m
}

func expectClosedReason(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("expected closed frame with reason %q, conn closed without it (err=%v)", want, err)
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && m["type"] == "closed" {
			if m["reason"] != want {
				t.Fatalf("closed reason = %v, want %q", m["reason"], want)
			}
			return
		}
	}
	t.Fatalf("timeout waiting for closed %q", want)
}

func helloDevice(t *testing.T, c *websocket.Conn, ticket, deviceID string) {
	t.Helper()
	writeFrame(t, c, map[string]any{
		"v": 1, "type": "hello", "ticket": ticket, "role": "device",
		"device_id": deviceID, "device_name": "Test Mac", "app_version": "2.4.0",
	})
	if m := readFrame(t, c); m["type"] != "hello_ok" {
		t.Fatalf("expected hello_ok, got %v", m)
	}
}
```

测试用例(逐字对应 spec §10.1,时序断言一律由注入的短常量推导,禁止写死墙钟):

1. `TestWSHelloTimeout`:连接后不发任何帧 → 约 HelloTimeout 后连接被关闭(读返回 error;WS close status 1008)。**不发 closed 帧**(spec §7:超时 close 不发帧)。
2. `TestWSHelloBadTicket`:ticket 伪造/过期 → `closed auth` → 连接关闭;`hello_failures_total` 记录见 Task 7,本 Task 只断言连接行为。
3. `TestWSHelloBadJSONAndBadVersion`:首帧坏 JSON → `closed protocol`;`v:2` → `closed protocol`;缺 `device_id` 的 device hello → `closed protocol`。
4. `TestWSDeviceClientRouting`:1 device + 2 client 完整 hello;device 发 app → 2 client 都收到 `from_device_id` 一致、payload 逐字节一致;client 发 app 指定 device → device 收到 `from_client_id`;client 指定不存在 device → 仅发送方收 `undeliverable`;**跨 room**:B 用户的 client 指定 A 用户的 device_id → undeliverable。
5. `TestWSPresence`:client 在线时 device 上线 → presence online=true 带 meta;device 正常关闭(Close(1000))→ presence online=false;device 杀连接(直接 `c.Close(1006,"")` 或底层 net.Conn 关闭)→ client 在 ReadIdleTimeout 推导的上限内收到 online=false。client 中途加入 → `hello_ok.room_devices` 含全量在线 device。
6. `TestWSTicketExpiringAndRenew`:ticket TTL 用短值(如 2s),Options.TicketExpiringLead=500ms、RenewGrace=500ms → 约 TTL-500ms 时收到 `ticket_expiring {retry_after_ms:30000}`;发送合法 renew(新 ticket)→ 连接存活超过原 exp+grace;**伪造 renew** → `closed auth`;**不 renew** → exp+grace 后 `closed auth`。**续期 ticket 的 sub 必须等于连接 user_id**(用别的 user 的 ticket renew → `closed auth`)。
7. `TestWSReplaceKick`:同 device_id 双连 → 旧连收 `closed replaced`,且旧连此后收不到路由帧(新连广播 app,旧连读不到)。
8. `TestWSSlowConsumer`:Options.OutboundBuffer=8;client 连接后不读;device 连发 20 帧 → 慢 client 收到 `closed slow_consumer`(能发尽发——断言连接最终被关,closed 帧可能收得到也可能收不到,断言以"连接关闭 + device 与另一正常 client 不受影响"为准)。
9. `TestWSConnectionCaps`:Options.MaxDevicesPerUser=2 → 第 3 个 device hello 后收 `closed protocol`。
10. `TestWSHelloFailIPLimit`:HelloFailLimiter(3);同一来源 IP 连续 3 次坏 ticket hello → 第 4 次握手直接 403(websocket.Dial 返回错误且 HTTP 响应 403)。
11. `TestWSOriginCheck`:白名单 `["https://www.yunhouai.com"]`:带该 Origin → 握手成功;带 `https://evil.com` → 403;不带 Origin → 成功(device 路径)。**白名单为空** + 带任意 Origin → 403(fail-closed,已决事项 4)。

- [ ] **Step 3: 跑测试确认失败(compile error)**

Run: `go test ./internal/relay/ -run TestWS -v`
Expected: FAIL(compile)

- [ ] **Step 4: 实现 handler 的 serveWS + Origin 校验**

`internal/handler/relay.go` 追加:

```go
// SetHub 注入 WS 侧依赖(main.go 在 relay 启用时调用)。
// hello 失败限流器(5 次/min/IP,spec §8)由 handler 持有并传入 relay 包。
func (h *RelayHandler) SetHub(hub *relay.Hub, tickets *service.RelayTicketService, allowedOrigins []string) {
	h.hub = hub
	h.tickets = tickets
	h.fails = relay.NewHelloFailLimiter(5)
	h.allowedOrigins = allowedOrigins
}

// ServeWS 处理 GET /relay/ws:Origin 校验(防 CSWSH)→ 移交 relay 包。
func (h *RelayHandler) ServeWS(c *gin.Context) {
	if h.hub.ShutdownStarted() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": "relay shutting down"})
		return
	}
	if origin := c.GetHeader("Origin"); origin != "" && !originAllowed(origin, h.allowedOrigins) {
		c.JSON(http.StatusForbidden, gin.H{"code": 403, "message": "origin not allowed"})
		return
	}
	relay.HandleWS(c.Writer, c.Request, h.hub, h.tickets, h.fails)
}

// originAllowed 比对 Origin 的 scheme://host 与 host 两种形态的白名单条目。
// 白名单为空 → fail-closed(已决事项 4)。解析失败的 Origin 一律拒绝。
func originAllowed(origin string, allowed []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, a := range allowed {
		if a == u.Host || a == u.Scheme+"://"+u.Host {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: 实现 relay/conn.go(核心状态机)**

结构总览(executor 按此实现,细节可自行调整但外部行为必须符合 spec):

```go
package relay

// ticketVerifier 由 *service.RelayTicketService 满足;relay 包不 import
// service,避免 service → relay → service 循环。
type ticketVerifier interface {
	Verify(token string) (userID string, exp time.Time, err error)
}

// wsConn 实现 Conn,包装一条 coder/websocket 连接。
type wsConn struct {
	hub     *Hub
	ws      *websocket.Conn
	userID  string
	role    Role
	id      string // device_id 或 client_id
	meta    DeviceInfo

	send    chan []byte    // 出站缓冲,容量 = Options.OutboundBuffer
	closing atomic.Bool
	closeOnce sync.Once

	mu      sync.Mutex
	exp     time.Time      // 当前 ticket 过期时刻
	timers  *time.Timer    // 续期相关定时器(见 runTimers)

	framesIn  atomic.Int64
	framesOut atomic.Int64
	connectedAt time.Time
}

// HandleWS 是 /relay/ws 的入口:Upgrade → 等 hello → 注册 → 服务循环。
// 全部路径保证:返回前连接一定被关闭或已移交服务循环。
func HandleWS(w http.ResponseWriter, r *http.Request, hub *Hub, tickets ticketVerifier, fails *HelloFailLimiter) {
	opts := hub.Options()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin 已在 handler 层手工校验(已决事项 4)
	})
	if err != nil {
		return // Upgrade 失败,连接未建立
	}
	ws.SetReadLimit(int64(opts.MaxFrameBytes) + 1024) // 余量给信封外层

	c := &wsConn{
		hub: hub, ws: ws, send: make(chan []byte, opts.OutboundBuffer),
		connectedAt: time.Now(),
	}
	c.run(r, tickets, fails)
}
```

`run` 的阶段划分(对应 spec §7 状态机):

```go
func (c *wsConn) run(r *http.Request, tickets ticketVerifier, fails *HelloFailLimiter) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.ws.Close(websocket.StatusNormalClosure, "bye")
	opts := c.hub.Options()

	// 阶段 1:等 hello(HelloTimeout 超时 → 直接关,不发帧,WS close 1008)
	hello, err := c.readHello(ctx, opts.HelloTimeout)
	if err != nil {
		// hello 失败指标(helloFailures)在 Task 6 挂接;此处只记 IP 限流。
		fails.RecordFailure(clientIP(r))
		c.ws.Close(websocket.StatusPolicyViolation, "hello timeout or invalid")
		return
	}

	// 阶段 2:校验 ticket
	userID, exp, err := tickets.Verify(hello.Ticket)
	if err != nil || !validHelloShape(hello, opts.MaxIDLen) {
		reason := ReasonAuth
		if validHelloShape(hello, opts.MaxIDLen) == false && err == nil {
			reason = ReasonProtocol
		}
		if reason == ReasonAuth {
			fails.RecordFailure(clientIP(r))
		}
		c.sendDirect(ClosedFrame(reason)) // 未经缓冲,直接写(此时 writer 未启动)
		c.ws.Close(websocket.StatusPolicyViolation, string(reason))
		return
	}
	c.userID, c.exp = userID, exp
	// helloID 是 conn.go 内的小 helper:按 role 返回 device_id 或 client_id。
	c.role, c.id = hello.Role, helloID(hello)
	c.meta = DeviceInfo{
		DeviceID: hello.DeviceID, DeviceName: hello.DeviceName,
		AppVersion: hello.AppVersion, ConnectedAt: c.connectedAt.Unix(),
	}

	// 阶段 3:注册(踢旧连由 Hub 完成;限额拒绝 → closed protocol)
	if _, err := c.hub.Register(c); err != nil {
		c.sendDirect(ClosedFrame(err.(*RejectError).Reason))
		c.ws.Close(websocket.StatusPolicyViolation, "rejected")
		return
	}
	defer c.hub.Unregister(c)

	// 阶段 4:hello_ok(client 带全量在线 device;device 也发,表为空)
	c.sendDirect(HelloOKFrame(c.hub.OnlineDevices(c.userID), time.Now().Unix()))

	// 阶段 5:服务循环:writer goroutine + 续期定时器 goroutine + 读循环
	go c.writer(ctx)
	go c.runTimers(ctx, tickets)

	c.readLoop(ctx, tickets)
	// readLoop 返回 = 连接终结;initiateClose 已在对应分支触发
}
```

关键子过程(行为契约,executor 按 spec 实现):

- **`readHello`**:用 `context.WithTimeout(ctx, opts.HelloTimeout)` 包一次 `ws.Read`;超时/读错 → err;读到帧 → 检查 ≤ MaxFrameBytes(`io.LimitReader` 读 MaxFrameBytes+1,超出按 protocol 错)→ `json.Unmarshal` 到 `HelloFrame`;要求 `Type=="hello"`、`V==ProtocolVersion`、`Role` 合法。
- **`validHelloShape`**:device 必填 device_id/device_name/app_version 非空且 ≤ MaxIDLen;client 必填 client_id 非空 ≤ MaxIDLen。
- **`readLoop`**:循环 `ws.Read(ctx)`(无超时——活性由 ping 保证,见下);每帧:①帧速率令牌桶(`rate.NewLimiter(rate.Limit(opts.MaxFramesPerSec), int(opts.MaxFramesPerSec))`,每帧 `Allow()`,失败 → `initiateClose(ReasonProtocol)`);②`PeekType`,`v != 1` → `initiateClose(ReasonProtocol)`;③按 type 分发:`app` → 解析 `AppFrame`(client 缺 target → protocol;payload 为空允许——是合法 JSON 即可,**不解析内容**)→ `hub.RouteApp`;`renew` → 校验新 ticket(`sub` 必须 == c.userID,否则 auth)→ 成功:更新 `c.exp`、重置定时器;失败:`initiateClose(ReasonAuth)`;`hello` 重复 → `initiateClose(ReasonProtocol)`;未知 type → `initiateClose(ReasonProtocol)`。读错误(`websocket.CloseStatus(err)` 任意)→ 正常断开,直接 return(Unregister 由 defer 触发 presence)。**framesIn 计数自 hello 后开始**。
- **`writer`**:`for { select { case b := <-c.send: ws.Write(ctx, MessageText, b); framesOut++; 若是 closed 帧(约定:initiateClose 入队的最后一帧)→ return; case <-ctx.Done(): return } }`。另起 ping 循环(可在 writer 内 select 加 ticker):每 `PingInterval` 调 `ws.Ping(pingCtx)`(pingCtx 超时 = `ReadIdleTimeout/3`);Ping 失败 → `initiateClose(ReasonIdleTimeout)`。coder/websocket 的 Ping 阻塞等 pong,等价于 spec 的"90s 无帧判死"(连续 ping 失败即死连接;实际探测周期 ≤ PingInterval + ping 超时,小于 90s 上限)。
- **`runTimers`**:两个 `time.Timer`:①`exp - TicketExpiringLead` 触发 → `Enqueue(TicketExpiringFrame(opts.TicketRetryAfterMS))`;②`exp + RenewGrace` 触发 → 若届时 exp 未更新(renew 成功会重置两 timer)→ `initiateClose(ReasonAuth)`。renew 成功后按新 exp 重建两个 timer(用 `sync.Mutex` 保护 exp/timer)。
- **`Enqueue`(实现 Conn 接口)**:`if c.closing.Load() { return }; select { case c.send <- b: default: c.initiateClose(ReasonSlowConsumer) }`。出站缓冲满 → 慢消费者断开,不影响同 room 其他连接(spec §8)。
- **`initiateClose(reason)`**:`closeOnce.Do`:置 closing;**best-effort 把 ClosedFrame(reason) 压入 send**(若满,丢弃最早一帧腾位:`select { case <-c.send: default: }` 后再压);随后等 writer 把 closed 帧写出(或 2s 超时)后 `ws.Close(StatusPolicyViolation, string(reason))` 并 cancel ctx。所有关闭路径(踢连/停机/协议错/慢消费者/idle/auth)都走这一个出口。
- **`sendDirect`**:writer 启动前的同步写(hello 阶段与 hello_ok 用),带 2s ctx。
- **`clientIP(r)`**:优先 `X-Forwarded-For` 首段( gin 的 TrustedProxies 已在全局中间件保证可信;relay 包内直接读 header 与 RemoteAddr 兜底)。

- [ ] **Step 6: 路由与 main 接线**

`router.go` 在 Task 3 的 `if relaySvc != nil` 块内追加:

```go
		engine.GET("/relay/ws", relayHandler.ServeWS)
```

(`Setup` 需要拿到 hub:把 `relayHandler.SetHub(...)` 放在 main.go 完成,或把 hub 作为 Setup 参数。选择:**main.go 构造 hub 与 handler 后调用 SetHub,Setup 只负责路由**——为此 `relayHandler` 需从 main 传进 Setup。改 Setup 签名为接收 `relayHandler *handler.RelayHandler`(nil = 禁用),替换 Task 3 的 `relaySvc` 参数;Setup 内只做判空与路由注册。)

`cmd/server/main.go`:

```go
	var relayHandler *handler.RelayHandler
	var relayHub *relay.Hub
	if cfg.RelayTicketSecret != "" {
		ticketSvc := service.NewRelayTicketService(cfg.RelayTicketSecret, cfg.RelayTicketSecretPrev, 300*time.Second)
		relaySvc := service.NewRelayService(subRepo, planRepo, ticketSvc)
		relayHub = relay.NewHub(relay.DefaultOptions())
		relayHandler = handler.NewRelayHandler(relaySvc)
		relayHandler.SetHub(relayHub, ticketSvc, cfg.RelayAllowedOrigins)
	} else {
		log.Printf("relay: disabled (RELAY_TICKET_SECRET empty)")
	}
```

`timeoutMiddleware` 的 skip 列表(main.go:247 附近)把 `"/relay/ws"` 加入(WS 是长连接,绝不能套 20s 超时)。

- [ ] **Step 7: 跑 conn 全部测试 + 全量回归**

Run: `go test ./internal/relay/ -v -race && go build ./... && go test ./internal/handler/ ./internal/service/`
Expected: PASS

```bash
git add internal/relay/conn.go internal/relay/conn_test.go internal/relay/limiter.go internal/handler/relay.go internal/router/router.go cmd/server/main.go
git commit -m "feat(relay): WS 连接生命周期(hello/续期/keepalive/踢连/限额/Origin)"
```

---

### Task 6: metrics + 日志

**Files:**
- Create: `internal/relay/metrics.go`
- Create: `internal/relay/logging.go`
- Modify: `internal/relay/hub.go`、`internal/relay/conn.go`(挂接指标与日志)
- Modify: `internal/router/router.go`(`/metrics`)
- Modify: `cmd/server/main.go`(promhttp)
- Test: `internal/relay/metrics_test.go`

**Interfaces:**
- Produces:

```go
type Metrics struct{ /* prometheus 收集器句柄 */ }
func NewMetrics(reg prometheus.Registerer, env string) *Metrics
// Hub 挂载点:NewHub(opts) 改为 NewHub(opts, m *Metrics)(m 为 nil = 不记录,测试方便)
func UserHash(userID string) string
```

- [ ] **Step 1: 写 metrics 失败测试**

`internal/relay/metrics_test.go`:用 `prometheus.NewRegistry()` 构造 Metrics,经 hub 走一遍 device+client 连接与断开(fakeConn 级,不走 WS),用 `reg.Gather()` 断言:

- 连接后 `relay_connections{role="device",env="test"} == 1`
- `relay_rooms_active == 1`
- device 发 1 帧 app 后 `relay_frames_total{direction="in",role="device"} == 1`,两个 client 各 1 帧 → `direction="out",role="client" == 2`
- undeliverable 1 次 → `relay_undeliverable_total == 1`
- 断开后 `relay_connections` 归零、`relay_connection_duration_seconds_count{role="device"} == 1`

- [ ] **Step 2: 实现 metrics.go**

按 spec §9 逐一定义(label 名:`role`、`env`、`direction`、`reason`):

```go
package relay

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	env              string
	connections      *prometheus.GaugeVec   // relay_connections{role,env}
	roomsActive      prometheus.Gauge       // relay_rooms_active{env}
	framesTotal      *prometheus.CounterVec // relay_frames_total{direction,role,env}
	undeliverable    prometheus.Counter     // relay_undeliverable_total{env}
	helloFailures    *prometheus.CounterVec // relay_hello_failures_total{reason,env}
	renewFailures    prometheus.Counter     // relay_renew_failures_total{env}
	slowConsumer     prometheus.Counter     // relay_slow_consumer_closes_total{env}
	connDuration     *prometheus.HistogramVec // relay_connection_duration_seconds{role,env}
}

func NewMetrics(reg prometheus.Registerer, env string) *Metrics {
	m := &Metrics{env: env}
	// …逐收集器 prometheus.NewXxx,ConstLabels: {"env": env},
	// reg.MustRegister(...) —— connDuration buckets 用
	// prometheus.ExponentialBuckets(1, 4, 8)(1s 到 ~1.6h)。
	return m
}
```

- [ ] **Step 3: 实现 logging.go**

```go
package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sync"
	"time"
)

// UserHash 是 user_id 的 SHA-256 截断(12 hex 字符),日志不对齐明文(spec §9)。
func UserHash(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(sum[:])[:12]
}

// 连接建立/关闭各一行(spec §9);绝不包含 payload / ticket 本体。
func logConnect(c *wsConn) {
	log.Printf("relay: connect user_hash=%s role=%s id=%s app_version=%s",
		UserHash(c.userID), c.role, c.id, c.meta.AppVersion)
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
```

- [ ] **Step 4: 挂接**

- `NewHub(opts Options, m *Metrics)`;Hub 持有 `warn *warnThrottled` 与 metrics。
- Register 成功 → `connections.WithLabelValues(role, env).Inc()`、`roomsActive` 按房间数刷新;Unregister → 对应 Dec、`connDuration` 观测;RouteApp → framesTotal in/out 计数、undeliverable 计数;conn.go 各关闭原因分支 → helloFailures/renewFailures/slowConsumer 计数 + `warn.Logf`。
- main.go:`relayMetrics = relay.NewMetrics(prometheus.DefaultRegisterer, cfg.AppEnv)`;router 暴露:

```go
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
```

(挂在 publicLimiter 后。)

- [ ] **Step 5: 测试通过 + 提交**

Run: `go test ./internal/relay/ -v -race && go build ./...`

```bash
git add internal/relay/metrics.go internal/relay/metrics_test.go internal/relay/logging.go internal/relay/hub.go internal/relay/conn.go internal/router/router.go cmd/server/main.go go.mod go.sum
git commit -m "feat(relay): Prometheus metrics + 连接日志(零 payload)"
```

---

### Task 7: 优雅停机 + nginx + 文档收尾

**Files:**
- Modify: `cmd/server/main.go`(停机序列)
- Modify: `deploy/nginx.conf`
- Modify: `docs/deployment.md`(relay 段落)
- Test: `internal/relay/conn_test.go` 追加 `TestWSGracefulShutdown`

**Interfaces:**
- Consumes: 全部前序 Task。

- [ ] **Step 1: 写停机失败测试**

`TestWSGracefulShutdown`:1 device + 1 client 在线 → 调用 `hub.Shutdown(5s)` → 两条连接都收到 `closed shutdown` 并被关闭;此后新握手被拒绝(503 或 closed shutdown);`ShutdownStarted()` 为 true。

- [ ] **Step 2: 接 main.go 停机序列**

现有:`rootCtx.Done()` → `sweeper.Stop()` → `srv.Shutdown(10s)`。在两者之间插入:

```go
	if relayHub != nil {
		relayHub.Shutdown(5 * time.Second) // 全部连接发 closed shutdown
		time.Sleep(500 * time.Millisecond) // 给 closed 帧一点 flush 时间(≤5s 预算内)
	}
```

(`hub.Shutdown` 本身把 closed 帧压入各连接缓冲;wsConn 的 initiateClose 路径负责 flush 后关闭。若 Task 5 实现里 Shutdown 的等待已在 conn 层完成,此 sleep 可去掉——以测试通过为准。)

- [ ] **Step 3: nginx.conf 追加 WS location**

照 `/chat` 的独立 location 先例(deploy/nginx.conf:45-60)追加:

```nginx
	# Relay WS(kaya 远程控制):Upgrade 透传;读超时 > 30s ping 周期(spec §3.2)
	location = /relay/ws {
		proxy_pass http://127.0.0.1:8080;
		proxy_http_version 1.1;
		proxy_set_header Upgrade $http_upgrade;
		proxy_set_header Connection "upgrade";
		proxy_set_header Host $host;
		proxy_set_header X-Real-IP $remote_addr;
		proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
		proxy_set_header X-Forwarded-Proto $scheme;
		proxy_buffering off;
		proxy_read_timeout 120s;
	}
```

- [ ] **Step 4: docs/deployment.md 追加 relay 段落**

简述:两个端点、env 变量表(`RELAY_TICKET_SECRET` 生成方式 `openssl rand -hex 32`、轮换用 `RELAY_TICKET_SECRET_PREVIOUS`、`RELAY_ALLOWED_ORIGINS`、`APP_ENV`)、nginx 依赖、单实例约束(spec §11)、`/metrics` 暴露面提醒。

- [ ] **Step 5: 全量验证 + 提交**

Run: `make test`(或 `go test -race -cover -p 1 ./internal/...`)+ `make lint` + `go build ./cmd/...`
Expected: 全绿

```bash
git add cmd/server/main.go deploy/nginx.conf docs/deployment.md internal/relay/conn_test.go
git commit -m "feat(relay): 优雅停机接线 + nginx WS 透传 + 部署文档"
```

---

## 验收对照(spec §10.1 → 测试)

| spec 场景 | 测试 |
|---|---|
| 1. ticket 签发 200/401/403/429 + HMAC 五类拒绝 | Task 2 全部 + Task 3 `TestRelayTicketIssue*` + 401 由 JWTAuth 中间件既有行为覆盖(中间件本身已有测试);429 由 Task 1 `TestRateLimit429SetsRetryAfter` + 路由参数(0.5/s,burst=30)覆盖 |
| 2. hello 超时/Origin/坏 JSON/缺字段/v 不支持 | Task 5 用例 1、3、10、11 |
| 3. 路由(广播/定向/undeliverable/跨 room) | Task 4 用例 4-6 + Task 5 用例 4(WS 级) |
| 4. presence 三路径 + 中途加入全量表 | Task 4 用例 2-3 + Task 5 用例 5 |
| 5. 续期(expiring/合法 renew/伪造 renew/宽限收敛) | Task 5 用例 6 |
| 6. 同 device_id 踢旧连 | Task 4 用例 1 + Task 5 用例 7 |
| 7. 背压 slow_consumer | Task 5 用例 8 |
| 8. 帧大小/帧速率/第 11 个 device | Task 5 用例 9 + conn.go 帧大小/速率分支(readLoop 契约①与 SetReadLimit;executor 补 `TestWSOversizeFrame`、`TestWSFrameRateLimit` 两个用例,语义:257 KiB 帧 → closed protocol 或连接被关;>100 帧/s 突发 → closed protocol) |
| 9. 优雅停机 | Task 7 `TestWSGracefulShutdown` |
