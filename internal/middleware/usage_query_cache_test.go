package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// usageCacheFakeClock 是可并发读写的假时钟：缓存中间件在并发请求里读它，
// 测试线程写它，所以必须加锁（否则 -race 会报数据竞争）。
type usageCacheFakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newUsageCacheFakeClock() *usageCacheFakeClock {
	return &usageCacheFakeClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *usageCacheFakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *usageCacheFakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// usageCacheTestUserKey 模拟 Task 2 里 JWT 鉴权后的 user_id 取法：真实接线
// 用 c.GetString(ContextUserID)，测试用请求头代替同一位置的身份。
func usageCacheTestUserKey(c *gin.Context) string {
	return c.GetHeader("X-Test-User")
}

// usageCacheWaitFor 轮询等待条件成立（机器高负载时给出宽松超时，不把慢当失败）。
func usageCacheWaitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// usageCacheEnvelope 解析 handler 的 JSON envelope，取 data.seq。
func usageCacheEnvelope(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response body is not JSON: %v (body=%q)", err, body)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. 只缓存同桶内的成功 GET
// ---------------------------------------------------------------------------

func TestUsageQueryCache_CachesOnlySuccessfulGETInSameBucket(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	var summaryCalls atomic.Int64
	var flakyCalls atomic.Int64
	var postCalls atomic.Int64

	engine := gin.New()
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		summaryCalls.Add(1)
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"})
	})
	engine.POST("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		postCalls.Add(1)
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"written": true}, "message": "ok"})
	})
	engine.GET("/usage/flaky", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		if flakyCalls.Add(1) == 1 {
			c.JSON(http.StatusInternalServerError, gin.H{
				"code": 500, "error": "internal_error", "message": "boom",
				"data": nil, "request_id": "req_1",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"})
	})

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w
	}
	post := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w
	}

	first := get("/usage/summary")
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", first.Code)
	}
	second := get("/usage/summary")
	if second.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("cached body differs from the original:\n first  = %q\n second = %q",
			first.Body.String(), second.Body.String())
	}
	if got := summaryCalls.Load(); got != 1 {
		t.Errorf("downstream calls = %d, want 1 (the second identical request must hit the cache)", got)
	}
	if ct := second.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("cached response Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
	}

	// 越过 5 秒桶：旧条目失效，下游重新执行。
	clock.advance(6 * time.Second)
	third := get("/usage/summary")
	if third.Code != http.StatusOK {
		t.Fatalf("request after bucket rollover: status = %d, want 200", third.Code)
	}
	if got := summaryCalls.Load(); got != 2 {
		t.Errorf("downstream calls after the bucket rolled over = %d, want 2", got)
	}

	// 500 不入缓存：紧接着的请求必须重新走下游，且拿到的是成功体。
	failing := get("/usage/flaky")
	if failing.Code != http.StatusInternalServerError {
		t.Fatalf("flaky first request: status = %d, want 500", failing.Code)
	}
	if got := flakyCalls.Load(); got != 1 {
		t.Errorf("flaky downstream calls = %d, want 1", got)
	}
	recovered := get("/usage/flaky")
	if recovered.Code != http.StatusOK {
		t.Errorf("flaky second request: status = %d, want 200 (the 500 body must not be cached)", recovered.Code)
	}
	if got := flakyCalls.Load(); got != 2 {
		t.Errorf("flaky downstream calls = %d, want 2 (the 500 body must not be cached)", got)
	}
	if strings.Contains(recovered.Body.String(), "boom") {
		t.Errorf("the error body was served from the cache: %q", recovered.Body.String())
	}

	// 非 GET 不进缓存：两次 POST 各自走下游，而且不会污染后续 GET。
	written := post("/usage/summary")
	writtenAgain := post("/usage/summary")
	if written.Code != http.StatusOK || writtenAgain.Code != http.StatusOK {
		t.Fatalf("POST status = %d / %d, want 200 / 200", written.Code, writtenAgain.Code)
	}
	if got := postCalls.Load(); got != 2 {
		t.Errorf("POST downstream calls = %d, want 2 (non-GET requests must never be cached)", got)
	}
	if written.Body.String() != writtenAgain.Body.String() {
		t.Errorf("POST bodies differ:\n first  = %q\n second = %q",
			written.Body.String(), writtenAgain.Body.String())
	}
	afterPost := get("/usage/summary")
	if afterPost.Code != http.StatusOK {
		t.Fatalf("GET after the POSTs: status = %d, want 200", afterPost.Code)
	}
	if afterPost.Body.String() != first.Body.String() {
		t.Errorf("the POST response polluted the cached GET entry:\n cached = %q\n got    = %q",
			first.Body.String(), afterPost.Body.String())
	}
	if got := summaryCalls.Load(); got != 2 {
		t.Errorf("GET downstream calls = %d, want 2 (the POSTs must not invalidate or replace the GET entry)", got)
	}
}

// ---------------------------------------------------------------------------
// 2. 参数规范化与 key 隔离
// ---------------------------------------------------------------------------

func TestUsageQueryCache_CanonicalizesAndIsolatesKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	var calls atomic.Int64
	engine := gin.New()
	handler := func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"})
	}
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), handler)
	engine.GET("/usage/timeseries", cache.Handle(usageCacheTestUserKey), handler)

	get := func(path, user string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-User", user)
		engine.ServeHTTP(w, req)
		return w
	}

	steps := []struct {
		name      string
		path      string
		user      string
		wantCalls int64
	}{
		{"first request", "/usage/summary?agent=b&agent=a", "user-a", 1},
		{"same params in another order shares the entry", "/usage/summary?agent=a&agent=b", "user-a", 1},
		{"param names in another order share the entry", "/usage/summary?b=1&a=2", "user-a", 2},
		{"reordered param names share the entry", "/usage/summary?a=2&b=1", "user-a", 2},
		{"swapped param values are a different entry", "/usage/summary?b=2&a=1", "user-a", 3},
		{"a different user has its own entry", "/usage/summary?agent=b&agent=a", "user-b", 4},
		{"a different path has its own entry", "/usage/timeseries?agent=b&agent=a", "user-a", 5},
		{"different param values have their own entry", "/usage/summary?agent=c", "user-a", 6},
		{"a different param set has its own entry", "/usage/summary?agent=b&agent=c", "user-a", 7},
		{"no params is a different entry", "/usage/summary", "user-a", 8},
	}

	for _, s := range steps {
		r := get(s.path, s.user)
		if r.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", s.name, r.Code)
			continue
		}
		if got := calls.Load(); got != s.wantCalls {
			t.Errorf("%s (%s user=%s): downstream calls = %d, want %d",
				s.name, s.path, s.user, got, s.wantCalls)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. 并发 miss 只跑一个 leader
// ---------------------------------------------------------------------------

func TestUsageQueryCache_ConcurrentMissRunsOneLeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	const requests = 80

	var calls atomic.Int64
	var arrived atomic.Int64
	var completed atomic.Int64
	leaderStarted := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		arrived.Add(1)
		c.Next()
	})
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		if calls.Add(1) == 1 {
			close(leaderStarted)
			<-release
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"})
	})

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		completed.Add(1)
		return w
	}

	results := make([]*httptest.ResponseRecorder, requests)
	var wg sync.WaitGroup

	// 先起 leader 并把它卡在下游 handler 上。
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = get()
	}()
	select {
	case <-leaderStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the leader never reached the downstream handler")
	}

	// 再起剩下的 79 个：它们应当合并到 leader 的 in-flight 调用上。
	for i := 1; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = get()
		}(i)
	}

	usageCacheWaitFor(t, "all requests to enter the middleware chain", 30*time.Second, func() bool {
		return arrived.Load() == requests
	})
	// 给跟随者一点时间停在 in-flight 通道上（不依赖睡眠做正确性判定，
	// 只是让并发合并路径真的被走到）。
	time.Sleep(100 * time.Millisecond)
	if got := completed.Load(); got != 0 {
		t.Fatalf("%d requests completed while the leader was blocked, want 0", got)
	}

	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("downstream calls = %d, want 1 (80 concurrent identical queries must collapse into one)", got)
	}
	var body string
	for i, r := range results {
		if r == nil {
			t.Fatalf("request %d produced no response", i)
		}
		if r.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, r.Code)
			continue
		}
		if i == 0 {
			body = r.Body.String()
			continue
		}
		if r.Body.String() != body {
			t.Errorf("request %d: body = %q, want the leader's body %q", i, r.Body.String(), body)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. 写后失效 + in-flight 竞态
// ---------------------------------------------------------------------------

func TestUsageQueryCache_InvalidateOnSuccessAndInflightRace(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	var queryCalls atomic.Int64
	var writeCalls atomic.Int64
	var blockNext atomic.Bool
	blocked := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		seq := queryCalls.Add(1)
		if blockNext.CompareAndSwap(true, false) {
			close(blocked)
			<-release
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"seq": seq}, "message": "ok"})
	})
	engine.POST("/usage/batch", cache.InvalidateOnSuccess(usageCacheTestUserKey), func(c *gin.Context) {
		writeCalls.Add(1)
		status, err := strconv.Atoi(c.Query("status"))
		if err != nil || status == 0 {
			status = http.StatusOK
		}
		c.JSON(status, gin.H{"code": status, "message": "ok"})
	})

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w
	}
	post := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w
	}

	// 基线：查询进缓存。
	if r := get("/usage/summary"); r.Code != http.StatusOK {
		t.Fatalf("priming query: status = %d, want 200", r.Code)
	}
	if got := queryCalls.Load(); got != 1 {
		t.Fatalf("downstream calls after priming = %d, want 1", got)
	}

	// 2xx 写 → 代次递增 → 下一次查询重新走下游。
	if r := post("/usage/batch?status=200"); r.Code != http.StatusOK {
		t.Fatalf("write: status = %d, want 200", r.Code)
	}
	if got := writeCalls.Load(); got != 1 {
		t.Fatalf("write calls = %d, want 1", got)
	}
	if r := get("/usage/summary"); r.Code != http.StatusOK {
		t.Fatalf("query after a successful write: status = %d, want 200", r.Code)
	}
	if got := queryCalls.Load(); got != 2 {
		t.Errorf("downstream calls after a successful write = %d, want 2 (a 2xx write must invalidate)", got)
	}

	// 500 写 → 不失效 → 条目仍然有效。
	if r := post("/usage/batch?status=500"); r.Code != http.StatusInternalServerError {
		t.Fatalf("failing write: status = %d, want 500", r.Code)
	}
	if got := writeCalls.Load(); got != 2 {
		t.Fatalf("write calls = %d, want 2", got)
	}
	if r := get("/usage/summary"); r.Code != http.StatusOK {
		t.Fatalf("query after a failed write: status = %d, want 200", r.Code)
	}
	if got := queryCalls.Load(); got != 2 {
		t.Errorf("downstream calls after a failed write = %d, want 2 (a failed write must not invalidate)", got)
	}

	// in-flight 竞态：查询 A 在下游阻塞期间发生一次成功写，A 结束后
	// 下一条查询必须重新走下游，而不是复用 A 写之前的结果。
	if r := post("/usage/batch?status=200"); r.Code != http.StatusOK {
		t.Fatalf("write before the in-flight query: status = %d, want 200", r.Code)
	}
	blockNext.Store(true)
	var inflightResult *httptest.ResponseRecorder
	var inflightDone sync.WaitGroup
	inflightDone.Add(1)
	go func() {
		defer inflightDone.Done()
		inflightResult = get("/usage/summary")
	}()
	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("the in-flight query never reached the downstream handler")
	}

	// A 还在飞行中时写入成功 → 失效。
	if r := post("/usage/batch?status=200"); r.Code != http.StatusOK {
		t.Fatalf("write during the in-flight query: status = %d, want 200", r.Code)
	}
	close(release)
	inflightDone.Wait()

	if inflightResult == nil {
		t.Fatal("the in-flight query produced no response")
	}
	if inflightResult.Code != http.StatusOK {
		t.Errorf("in-flight query: status = %d, want 200", inflightResult.Code)
	}
	if got := queryCalls.Load(); got != 3 {
		t.Errorf("downstream calls = %d, want 3 (the in-flight query is the third)", got)
	}

	// A 完成之后的那条查询：必须重新执行下游。
	after := get("/usage/summary")
	if after.Code != http.StatusOK {
		t.Fatalf("query after the in-flight query: status = %d, want 200", after.Code)
	}
	if got := queryCalls.Load(); got != 4 {
		t.Errorf("downstream calls = %d, want 4 (the next query must not reuse the pre-write result)", got)
	}
	env := usageCacheEnvelope(t, after.Body.String())
	data, ok := env["data"].(map[string]any)
	if !ok {
		t.Fatalf("response envelope has no data object: %q", after.Body.String())
	}
	if seq, _ := data["seq"].(float64); int(seq) != 4 {
		t.Errorf("response data.seq = %v, want 4 (the stale in-flight result was reused)", data["seq"])
	}
}

// ---------------------------------------------------------------------------
// 5. 跟随者被取消：立刻返回，不执行下游
// ---------------------------------------------------------------------------

func TestUsageQueryCache_FollowerCancelDoesNotWaitForLeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	var calls atomic.Int64
	var arrived atomic.Int64
	leaderStarted := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		arrived.Add(1)
		c.Next()
	})
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		calls.Add(1)
		close(leaderStarted)
		<-release
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"})
	})

	// leader 先起并堵在下游：它持有 in-flight，跟随者只能等它。
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
	}()
	select {
	case <-leaderStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the leader never reached the downstream handler")
	}

	// 跟随者带一个可取消的 context。
	ctx, cancel := context.WithCancel(context.Background())
	followerDone := make(chan struct{})
	go func() {
		defer close(followerDone)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil).WithContext(ctx)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
	}()

	usageCacheWaitFor(t, "the follower to enter the middleware chain", 30*time.Second, func() bool {
		return arrived.Load() == 2
	})
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case <-followerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled follower kept waiting for the leader instead of returning")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("downstream calls = %d, want 1 (a cancelled follower must not run the downstream)", got)
	}

	close(release)
	select {
	case <-leaderDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the leader never finished")
	}
}

// ---------------------------------------------------------------------------
// 6. leader panic：等待者共享同一次失败，不得各自执行下游
// ---------------------------------------------------------------------------

func TestUsageQueryCache_LeaderPanicSharesFailureWithWaiters(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	const waiters = 20

	var calls atomic.Int64
	var arrived atomic.Int64
	var leaderOnce sync.Once
	leaderStarted := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.Use(gin.RecoveryWithWriter(io.Discard))
	engine.Use(func(c *gin.Context) {
		arrived.Add(1)
		c.Next()
	})
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		calls.Add(1)
		leaderOnce.Do(func() { close(leaderStarted) })
		<-release
		panic("downstream exploded")
	})

	// leader 先起并堵在下游。
	leaderStatus := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		leaderStatus <- w.Code
	}()
	select {
	case <-leaderStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the leader never reached the downstream handler")
	}

	// 等待者进入链路，停在 leader 的 in-flight 上。
	waiterStatus := make(chan int, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
			req.Header.Set("X-Test-User", "user-a")
			engine.ServeHTTP(w, req)
			waiterStatus <- w.Code
		}()
	}
	usageCacheWaitFor(t, "all waiters to enter the middleware chain", 30*time.Second, func() bool {
		return arrived.Load() == waiters+1
	})
	time.Sleep(200 * time.Millisecond)

	close(release) // leader 现在 panic

	select {
	case code := <-leaderStatus:
		if code != http.StatusInternalServerError {
			t.Errorf("leader status = %d, want 500 (Gin's recovery must handle the panic)", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the leader never returned")
	}

	// 等待者共享这一次失败（同样的 500），并且绝不各自执行下游。
	for i := 0; i < waiters; i++ {
		select {
		case code := <-waiterStatus:
			if code != http.StatusInternalServerError {
				t.Errorf("waiter %d status = %d, want 500 (the waiters must share the leader's failure)", i, code)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("waiter %d never returned", i)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("downstream calls = %d, want 1 (the waiters must not fan out to the downstream)", got)
	}
}

// ---------------------------------------------------------------------------
// 7. 缓存禁用（ttl <= 0）：不缓存，也不累积代次
// ---------------------------------------------------------------------------

func TestUsageQueryCache_DisabledCacheSkipsGenerationUpdates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// ttl <= 0 → 缓存禁用。
	cache := NewUsageQueryCache(0, nil)

	var queryCalls atomic.Int64
	var writeCalls atomic.Int64

	engine := gin.New()
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		queryCalls.Add(1)
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"})
	})
	engine.POST("/usage/batch", cache.InvalidateOnSuccess(usageCacheTestUserKey), func(c *gin.Context) {
		writeCalls.Add(1)
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
	})

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w
	}
	post := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w
	}

	// 禁用态下写请求照常执行，但不得累积代次（否则 generations 无限增长）。
	if r := post("/usage/batch"); r.Code != http.StatusOK {
		t.Fatalf("write: status = %d, want 200", r.Code)
	}
	if got := writeCalls.Load(); got != 1 {
		t.Errorf("write calls = %d, want 1", got)
	}
	cache.mu.Lock()
	generations := len(cache.generations)
	cache.mu.Unlock()
	if generations != 0 {
		t.Errorf("cached generations = %d, want 0 (a disabled cache must not accumulate generations)", generations)
	}

	// 禁用态下缓存不生效：每次 GET 都走下游。
	if r := get("/usage/summary"); r.Code != http.StatusOK {
		t.Fatalf("first GET: status = %d, want 200", r.Code)
	}
	if r := get("/usage/summary"); r.Code != http.StatusOK {
		t.Fatalf("second GET: status = %d, want 200", r.Code)
	}
	if got := queryCalls.Load(); got != 2 {
		t.Errorf("downstream calls = %d, want 2 (a disabled cache must not cache anything)", got)
	}
}

// ---------------------------------------------------------------------------
// 8. 跟随者的「服务端 deadline」：必须共享 leader 的结果，不能返回空 200
//
// 生产 timeoutMiddleware 只做 context.WithTimeout + c.Next()：它取消 context，
// 但**不写任何响应**。所以跟随者在 deadline 上直接 Abort，客户端会收到一个
// 空的 200——把超时伪装成「成功但没数据」。deadline 与「客户端主动断连」必须
// 区别对待：断连可以静默结束，deadline 必须等 leader 的结果（leader 受同一个
// 服务端 deadline 约束，会先返回），并且绝不自己重跑下游。
// ---------------------------------------------------------------------------

func TestUsageQueryCache_FollowerDeadlineSharesLeaderResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		leaderCode int
		leaderBody gin.H
	}{
		{
			name:       "leader publishes 200",
			leaderCode: http.StatusOK,
			leaderBody: gin.H{"code": 0, "data": gin.H{"total_tokens": 42}, "message": "ok"},
		},
		{
			name:       "leader publishes non-200",
			leaderCode: http.StatusServiceUnavailable,
			leaderBody: gin.H{"code": 503, "error": "upstream_unavailable", "message": "database is warming up"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newUsageCacheFakeClock()
			cache := NewUsageQueryCache(5*time.Second, clock.now)

			var calls atomic.Int64
			var arrived atomic.Int64
			leaderStarted := make(chan struct{})
			release := make(chan struct{})

			engine := gin.New()
			engine.Use(func(c *gin.Context) {
				arrived.Add(1)
				c.Next()
			})
			engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
				calls.Add(1)
				close(leaderStarted) // 只有 leader 会执行到下游
				<-release
				c.JSON(tc.leaderCode, tc.leaderBody)
			})

			// leader 堵在下游。
			leaderDone := make(chan struct{})
			go func() {
				defer close(leaderDone)
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil)
				req.Header.Set("X-Test-User", "user-a")
				engine.ServeHTTP(w, req)
			}()
			select {
			case <-leaderStarted:
			case <-time.After(30 * time.Second):
				t.Fatal("the leader never reached the downstream handler")
			}

			// 跟随者带服务端 deadline（与 timeoutMiddleware 同形：只取消，不写响应）。
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			followerStatus := make(chan int, 1)
			followerBody := make(chan string, 1)
			go func() {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/usage/summary", nil).WithContext(ctx)
				req.Header.Set("X-Test-User", "user-a")
				engine.ServeHTTP(w, req)
				followerStatus <- w.Code
				followerBody <- w.Body.String()
			}()

			usageCacheWaitFor(t, "the follower to enter the middleware chain", 30*time.Second, func() bool {
				return arrived.Load() == 2
			})
			// 等过 deadline：只 Abort 的实现会在这里返回一个空 200。
			time.Sleep(400 * time.Millisecond)

			close(release) // leader 现在发布结果

			var gotStatus int
			var gotBody string
			select {
			case gotStatus = <-followerStatus:
				gotBody = <-followerBody
			case <-time.After(30 * time.Second):
				t.Fatal("the follower never returned")
			}

			if gotStatus != tc.leaderCode {
				t.Errorf("follower status = %d, want %d (a server deadline must share the leader's result, not abort with an empty 200)", gotStatus, tc.leaderCode)
			}
			if gotBody == "" {
				t.Errorf("follower body is empty: a deadline must not surface as an empty success")
			}
			if tc.leaderCode != http.StatusOK && !strings.Contains(gotBody, "upstream_unavailable") {
				t.Errorf("follower body = %q, want the leader's error payload", gotBody)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("downstream calls = %d, want 1 (the follower must never re-run the downstream)", got)
			}
			select {
			case <-leaderDone:
			case <-time.After(30 * time.Second):
				t.Fatal("the leader never finished")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 9. 重复的单值参数：顺序必须保持语义（只有数组参数才排序）
//
// handler 对 source/from/to/dimension/limit/interval/group_by/tz 都只取第一个
// 值（c.Query / DefaultQuery），所以 ?source=all&source=chat 与
// ?source=chat&source=all 是两种不同语义，不能共享缓存条目。只有
// agent/provider_key/model_key/device_id 走 QueryArray（集合语义），排序安全。
// ---------------------------------------------------------------------------

func TestUsageQueryCache_RepeatedSingleValueParamsStayOrderSensitive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	var calls atomic.Int64
	engine := gin.New()
	engine.GET("/usage/summary", cache.Handle(usageCacheTestUserKey), func(c *gin.Context) {
		calls.Add(1)
		// 与真实 handler 的 DefaultQuery 取法一致：只看第一个值。
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"source": c.Query("source")}, "message": "ok"})
	})

	get := func(path string) int {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-User", "user-a")
		engine.ServeHTTP(w, req)
		return w.Code
	}

	// source 是单值参数：两个顺序解析出的第一个值不同，不能共享条目。
	if code := get("/usage/summary?source=all&source=chat"); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("downstream calls after the first request = %d, want 1", got)
	}
	if code := get("/usage/summary?source=chat&source=all"); code != http.StatusOK {
		t.Fatalf("reordered request: status = %d, want 200", code)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("downstream calls = %d, want 2: ?source=all&source=chat and ?source=chat&source=all resolve to different first values and must not share an entry", got)
	}

	// agent 是数组参数（集合语义）：顺序不同仍必须共享条目。
	if code := get("/usage/summary?agent=beta&agent=alpha"); code != http.StatusOK {
		t.Fatalf("array first request: status = %d, want 200", code)
	}
	before := calls.Load()
	if code := get("/usage/summary?agent=alpha&agent=beta"); code != http.StatusOK {
		t.Fatalf("array reordered request: status = %d, want 200", code)
	}
	if got := calls.Load(); got != before {
		t.Errorf("downstream calls = %d, want %d: array params must stay order-insensitive", got, before)
	}
}

// ---------------------------------------------------------------------------
// 10. 只写用户：成功写也要触发惰性清扫，代次不能只靠未来查询回收
//
// 老实现只在查询路径（Handle）里调用 sweepLocked。只写不读的用户永远不会
// 触发清扫，generations 条目会一直留着；虽然每个用户只占一项，但这是纯泄漏，
// 而且没有上界。成功写自己扫一次当前桶即可回收。
// ---------------------------------------------------------------------------

func TestUsageQueryCache_WriteOnlyUserGenerationsAreSwept(t *testing.T) {
	gin.SetMode(gin.TestMode)

	clock := newUsageCacheFakeClock()
	cache := NewUsageQueryCache(5*time.Second, clock.now)

	engine := gin.New()
	engine.POST("/usage/batch", cache.InvalidateOnSuccess(usageCacheTestUserKey), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
	})

	post := func() {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/usage/batch", nil)
		req.Header.Set("X-Test-User", "write-only-user")
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("write: status = %d, want 200", w.Code)
		}
	}
	generationOf := func(user string) (uint64, bool) {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		g, ok := cache.generations[user]
		return g, ok
	}

	// 该用户只写不读：没有任何缓存条目。
	post()
	if g, ok := generationOf("write-only-user"); !ok || g != 1 {
		t.Fatalf("generation after the first write = %d (present=%v), want 1", g, ok)
	}

	// 换两个桶再写：没有生命周期条目可保留，代次应被清扫后重新从 1 开始，
	// 而不是无限累加（老实现这里会得到 2）。
	clock.advance(2 * 5 * time.Second)
	post()
	g, ok := generationOf("write-only-user")
	if !ok {
		t.Fatalf("generation entry disappeared; the write's own increment must re-create it")
	}
	if g != 1 {
		t.Errorf("generation after a later write = %d, want 1: the write path must sweep stale generations instead of accumulating them", g)
	}
}
