package e2e

// usage_tokens_test.go — Task 3 acceptance/regression for the Usage query
// rate-limit + 5 秒缓存改动。真实用户 + 真实路由 + 真实 PostgreSQL。
//
// 门禁：两分钟 10 页面长跑是 USAGE_ACCEPTANCE=1 才执行的负载门禁（与
// tests/integration 的 KAYA_LOAD=1 同约定），所以 make ci-e2e 不会被拖长；
// 80 突发、超限 429、上传回归三个短验收随常规 e2e 一起跑。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

const usageAcceptanceDeviceID = "d_e2eacc0001"

// usageAcceptanceStatsSettle 是 PostgreSQL 关系统计的上报窗口。pgstat 在
// 事务结束时节流上报、并只在后端空闲约 10 秒后才补报，所以读表扫描增量
// 之前必须等这么久，否则会读到 0（不是没有查询，而是统计还没落盘）。
const usageAcceptanceStatsSettle = 11 * time.Second

// usageAcceptanceGate 是长跑压测的非默认门禁。
func usageAcceptanceGate(t *testing.T) {
	t.Helper()
	if os.Getenv("USAGE_ACCEPTANCE") == "" {
		t.Skip("usage acceptance drill: set USAGE_ACCEPTANCE=1 to run (非默认门禁)")
	}
}

type usageAcceptanceResponse struct {
	Status int
	Body   []byte
	Header http.Header
	Took   time.Duration
}

// usageAcceptanceDo 在进程内跑真实 Gin 路由，返回状态码/响应/耗时。
// 不调用 t.Fatalf：它会同时被并发 goroutine 使用。
func usageAcceptanceDo(engine *gin.Engine, method, path, token, body string) usageAcceptanceResponse {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	start := time.Now()
	engine.ServeHTTP(rec, req)
	return usageAcceptanceResponse{
		Status: rec.Code,
		Body:   rec.Body.Bytes(),
		Header: rec.Header(),
		Took:   time.Since(start),
	}
}

// usageAcceptanceLogin 走真实 /test/login（PAYPAL_L3_E2E_MODE=1）拿 JWT。
func usageAcceptanceLogin(t *testing.T, engine *gin.Engine, name string) (token, userID string) {
	t.Helper()
	r := loginAndGetTokens(t, engine, name, "yundian")
	if r.AccessToken == "" || r.User.ID == "" {
		t.Fatalf("login %s: empty token or user id", name)
	}
	return r.AccessToken, r.User.ID
}

func usageAcceptanceDay(offset int) time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+offset, 1, 0, 0, 0, time.UTC)
}

func usageAcceptanceWindow() (from, to string) {
	today := time.Now().UTC()
	return today.AddDate(0, 0, -6).Format("2006-01-02"), today.Format("2006-01-02")
}

// usageAcceptanceDashQueries 是 10 个页面各自每 5 秒发出的 8 个合法查询。
// 页面之间完全相同，所以同一 5 秒桶内 80 个请求只对应 8 个不同的 cache key；
// 扫描增量仅作方向性观察，不能证明精确的 SQL / 下游调用次数。
func usageAcceptanceDashQueries(from, to string) []string {
	window := "from=" + from + "&to=" + to
	return []string{
		"/user/usage/tokens/summary?" + window,
		"/user/usage/tokens/summary?" + window + "&source=chat",
		"/user/usage/tokens/summary?" + window + "&source=gateway",
		"/user/usage/tokens/breakdown?dimension=agent&" + window,
		"/user/usage/tokens/breakdown?dimension=model&" + window,
		"/user/usage/tokens/timeseries?interval=day&group_by=none&tz=UTC&" + window,
		"/user/usage/tokens/timeseries?interval=day&group_by=agent&tz=UTC&" + window,
		"/user/devices",
	}
}

func usageAcceptanceBucketJSON(ts time.Time, prompt, completion, total, requests int) string {
	return fmt.Sprintf(`{"bucket_start":%q,"bucket_size":"hour","source":"gateway",`+
		`"agent_key":"claude-code","provider_key":"preset:deepseek","model_key":"deepseek-v4-pro",`+
		`"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"requests":%d}`,
		ts.Format(time.RFC3339), prompt, completion, total, requests)
}

func usageAcceptanceIngestBody(buckets ...string) string {
	return `{"schema_version":1,"device_id":"` + usageAcceptanceDeviceID + `","buckets":[` +
		strings.Join(buckets, ",") + `]}`
}

// usageAcceptanceUpload 上传契约 §9.2 形状的两个桶（10/5/15 + 27/8/35）。
func usageAcceptanceUpload(t *testing.T, engine *gin.Engine, token string) {
	t.Helper()
	day := usageAcceptanceDay(-2)
	body := usageAcceptanceIngestBody(
		usageAcceptanceBucketJSON(day.Add(1*time.Hour), 10, 5, 15, 1),
		usageAcceptanceBucketJSON(day.Add(2*time.Hour), 27, 8, 35, 2),
	)
	resp := usageAcceptanceDo(engine, http.MethodPost, "/user/usage/tokens/batch", token, body)
	if resp.Status != http.StatusOK {
		t.Fatalf("batch upload: got %d — %s", resp.Status, string(resp.Body))
	}
}

func usageAcceptanceSummaryTotal(t *testing.T, engine *gin.Engine, token string) int64 {
	t.Helper()
	resp := usageAcceptanceDo(engine, http.MethodGet, "/user/usage/tokens/summary", token, "")
	if resp.Status != http.StatusOK {
		t.Fatalf("summary: got %d — %s", resp.Status, string(resp.Body))
	}
	var env struct {
		Data struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		t.Fatalf("decode summary: %v (%s)", err, string(resp.Body))
	}
	return env.Data.TotalTokens
}

// usageAcceptanceTableReads 是 usage 两张查询表在 pg_stat_user_tables 里的
// 扫描计数（seq_scan + idx_scan）总和。它只是**粗粒度观测值**：一条 SQL 可能
// 命中多路索引扫描，pgstat 又是异步上报的，所以这个数字不能换算成「执行了
// 几条 SQL」。用它只能判断「扫描增量是否与缓存合并生效一致」；确定性的
// 「并发 miss 只跑一次 leader」由 internal/middleware 的假时钟/调用计数单测
// 证明（TestUsageQueryCache_ConcurrentMissRunsOneLeader）。
func usageAcceptanceTableReads(t *testing.T, db *sqlx.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Get(&n, `
		SELECT COALESCE(SUM(COALESCE(seq_scan, 0) + COALESCE(idx_scan, 0)), 0)::bigint
		  FROM pg_stat_user_tables
		 WHERE relname IN ('usage_token_buckets', 'usage_devices')`); err != nil {
		t.Fatalf("read pg_stat_user_tables: %v", err)
	}
	return n
}

// usageAcceptanceCPU 是 Users 进程（本测试进程即服务进程）的用户态+内核态
// CPU 时间。spec 要求观察 CPU；不设阈值，只记录。
func usageAcceptanceCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Sec)*time.Second +
		time.Duration(ru.Utime.Usec)*time.Microsecond +
		time.Duration(ru.Stime.Sec)*time.Second +
		time.Duration(ru.Stime.Usec)*time.Microsecond
}

// usageAcceptanceSampleActive 采样 PostgreSQL 活跃会话数峰值。
func usageAcceptanceSampleActive(db *sqlx.DB, peak *atomic.Int64, done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			var n int64
			if err := db.Get(&n, `
				SELECT COUNT(*) FROM pg_stat_activity
				 WHERE datname = current_database() AND state = 'active'`); err != nil {
				continue
			}
			for {
				cur := peak.Load()
				if n <= cur || peak.CompareAndSwap(cur, n) {
					break
				}
			}
		}
	}
}

func usageAcceptancePercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*p)]
}

// TestUsageTokenAcceptance_TenPagesTwoMinutes 是 spec 的核心验收：同一真实
// 用户、10 个页面、每页每 5 秒 8 个查询、持续 120 秒，全程无 429/5xx。
// 同时观察 PostgreSQL 表扫描增量：80 个请求/轮只对应 8 个不同 cache key，
// 扫描增量仅作方向性观察，不能证明精确 SQL / 下游调用次数（扫描计数还包含
// 单条 SQL 的多路索引扫描与 5 秒桶边界重查）。
func TestUsageTokenAcceptance_TenPagesTwoMinutes(t *testing.T) {
	usageAcceptanceGate(t)

	engine, _, db := setupE2EServer(t)
	token, userID := usageAcceptanceLogin(t, engine, "usage-accept-10pages")
	usageAcceptanceUpload(t, engine, token)

	from, to := usageAcceptanceWindow()
	queries := usageAcceptanceDashQueries(from, to)
	const (
		pages        = 10
		rounds       = 25 // 25 个 5 秒间隔 = 120 秒
		roundEvery   = 5 * time.Second
		totalRequest = pages * 8 * rounds
	)

	statuses := make(map[int]int, 4)
	latencies := make([]time.Duration, 0, totalRequest)
	var mu sync.Mutex

	readsBefore := usageAcceptanceTableReads(t, db)
	poolBefore := db.Stats()
	cpuBefore := usageAcceptanceCPU()
	var peakActive atomic.Int64
	done := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go usageAcceptanceSampleActive(db, &peakActive, done, &samplerWG)

	start := time.Now()
	for round := 0; round < rounds; round++ {
		if wait := time.Until(start.Add(time.Duration(round) * roundEvery)); wait > 0 {
			time.Sleep(wait)
		}
		var wg sync.WaitGroup
		for page := 0; page < pages; page++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, q := range queries {
					resp := usageAcceptanceDo(engine, http.MethodGet, q, token, "")
					mu.Lock()
					statuses[resp.Status]++
					latencies = append(latencies, resp.Took)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	elapsed := time.Since(start)
	cpuUsed := usageAcceptanceCPU() - cpuBefore
	close(done)
	samplerWG.Wait()
	// 同上：给 PostgreSQL 的异步关系统计一个上报窗口。
	time.Sleep(usageAcceptanceStatsSettle)

	readsAfter := usageAcceptanceTableReads(t, db)
	poolAfter := db.Stats()

	received := 0
	for status, n := range statuses {
		received += n
		if status != http.StatusOK {
			t.Errorf("10-page drill: %d requests got status %d (want 200 only)", n, status)
		}
	}
	if received != totalRequest {
		t.Fatalf("10-page drill: received %d responses, want %d", received, totalRequest)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	dbReads := readsAfter - readsBefore
	t.Logf("10-page drill: user=%s requests=%d elapsed=%s statuses=%v", userID, received, elapsed, statuses)
	t.Logf("10-page drill: PostgreSQL usage-table reads=%d (requests/reads=%.1f, reads/round=%.1f), p50=%s p95=%s max=%s",
		dbReads, float64(received)/float64(maxInt64(dbReads, 1)), float64(dbReads)/float64(rounds),
		usageAcceptancePercentile(latencies, 0.50),
		usageAcceptancePercentile(latencies, 0.95),
		latencies[len(latencies)-1])
	t.Logf("10-page drill: CPU used=%s, pg active peak=%d, pool maxOpen=%d inUse=%d waitCount=%d waitDuration=%s",
		cpuUsed, peakActive.Load(), poolAfter.MaxOpenConnections, poolAfter.InUse,
		poolAfter.WaitCount-poolBefore.WaitCount, poolAfter.WaitDuration-poolBefore.WaitDuration)

	if elapsed < 2*time.Minute {
		t.Fatalf("10-page drill ran only %s, want at least 2 minutes", elapsed)
	}
	// 扫描增量只作观测值打印（见上面的 Logf），**不设任何硬门限**：这个数字
	// 无法换算成 SQL 条数（一条查询多路索引扫描、pgstat 异步上报、5 秒桶边界
	// 重查），任何固定阈值都只能是看到实测值后的事后拟合，不是从模型推导出来
	// 的验收标准。去重的确定性证据在 internal/middleware 的单测里（假时钟 +
	// 下游调用计数）。本测试的验收口径是状态码：2000 个请求全部 200、无 429/5xx。
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// TestUsageTokenAcceptance_BurstAndOverLimit 覆盖 spec 的另外两条口径：
// 新用户 80 个同步查询全部放行（扫描增量仅用于判断「与合并生效一致」，
// 不据此声称精确 SQL 次数）；持续超过 20 QPS 最终 429 且 Retry-After 为正；
// coverage 的独立桶不受看板桶耗尽影响。
func TestUsageTokenAcceptance_BurstAndOverLimit(t *testing.T) {
	engine, _, db := setupE2EServer(t)

	t.Run("eighty_burst_passes_and_dedupes", func(t *testing.T) {
		token, userID := usageAcceptanceLogin(t, engine, "usage-accept-burst")
		from, to := usageAcceptanceWindow()
		base := "/user/usage/tokens/summary?from=" + from + "&to=" + to
		// 预热限流器的 visitor 桶，并用另一个参数占用缓存 key，避免 80 个
		// 突发请求直接命中预热条目而测不到 leader/跟随者合并。
		warm := usageAcceptanceDo(engine, http.MethodGet, base+"&source=chat", token, "")
		if warm.Status != http.StatusOK {
			t.Fatalf("warm-up: got %d — %s", warm.Status, string(warm.Body))
		}

		readsBefore := usageAcceptanceTableReads(t, db)
		const burst = 80
		var wg sync.WaitGroup
		var limited atomic.Int64
		var nonOK atomic.Int64
		var decodeFail atomic.Int64
		var bodies sync.Map
		start := make(chan struct{})
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp := usageAcceptanceDo(engine, http.MethodGet, base, token, "")
				if resp.Status == http.StatusTooManyRequests {
					limited.Add(1)
					return
				}
				if resp.Status != http.StatusOK {
					nonOK.Add(1)
					return
				}
				var env struct {
					Code int             `json:"code"`
					Data json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(resp.Body, &env); err != nil || env.Data == nil {
					decodeFail.Add(1)
					return
				}
				// 比较**完整响应体**的 hash：只比 total_tokens 无法发现响应里
				// 其他字段（分页、时间框、message 等）不一致的情况。
				bodies.Store(sha256.Sum256(resp.Body), struct{}{})
			}()
		}
		close(start)
		wg.Wait()
		// PostgreSQL 的 pg_stat_user_tables 由各 backend 异步上报，立刻读
		// 会得到 0；等满上报窗口再取真实增量。
		time.Sleep(usageAcceptanceStatsSettle)

		reads := usageAcceptanceTableReads(t, db) - readsBefore
		t.Logf("80-request synchronized burst: user=%s limited=%d nonOK=%d decodeFail=%d dbScanDelta=%d",
			userID, limited.Load(), nonOK.Load(), decodeFail.Load(), reads)

		if got := limited.Load(); got != 0 {
			t.Fatalf("%d/%d legal dashboard requests were rate limited (20 QPS/burst 100 must allow 80)", got, burst)
		}
		if got := nonOK.Load() + decodeFail.Load(); got != 0 {
			t.Fatalf("%d burst requests failed unexpectedly", got)
		}
		if got := len(mapKeys(&bodies)); got > 1 {
			t.Fatalf("burst returned %d distinct response bodies; 80 identical requests must produce one byte-identical body", got)
		}
		// 扫描增量只用于判断「是否与合并生效一致」：80 个相同请求若完全没有
		// 合并，至少会产生 80 次表扫描，所以「增量 < 80」是方向性判据，不是
		// 精确 SQL 计数——一条查询可能命中多路索引扫描、pgstat 异步上报、
		// 预热请求的扫描也可能落在窗口内。「并发 miss 只跑一次 leader」的
		// 确定性证明在 internal/middleware 的单测（假时钟 + 调用计数）。
		if reads < 1 {
			t.Errorf("PostgreSQL scan delta=0 for the burst; the counter was not observable (stats may not have flushed)")
		}
		if reads >= int64(burst) {
			t.Errorf("PostgreSQL scan delta=%d for %d identical requests: not consistent with in-flight coalescing", reads, burst)
		}
	})

	t.Run("drain_returns_429_with_retry_after", func(t *testing.T) {
		token, userID := usageAcceptanceLogin(t, engine, "usage-accept-drain")
		from, to := usageAcceptanceWindow()
		base := "/user/usage/tokens/summary?from=" + from + "&to=" + to

		const maxAttempts = 600
		successes := 0
		firstLimited := -1
		var retryAfter string
		var limitedBody map[string]any
		drainStart := time.Now()
		for i := 0; i < maxAttempts; i++ {
			resp := usageAcceptanceDo(engine, http.MethodGet, base, token, "")
			switch resp.Status {
			case http.StatusOK:
				successes++
			case http.StatusTooManyRequests:
				firstLimited = i
				retryAfter = resp.Header.Get("Retry-After")
				_ = json.Unmarshal(resp.Body, &limitedBody)
			default:
				t.Fatalf("drain attempt %d: unexpected status %d — %s", i, resp.Status, string(resp.Body))
			}
			if firstLimited >= 0 {
				break
			}
		}
		drainElapsed := time.Since(drainStart)
		if firstLimited < 0 {
			t.Fatalf("no 429 within %d sequential requests: the 20 QPS bucket never drained", maxAttempts)
		}
		t.Logf("over-limit drain: user=%s successes=%d firstLimitedAt=%d elapsed=%s observedQPS=%.0f Retry-After=%q",
			userID, successes, firstLimited, drainElapsed,
			float64(successes)/drainElapsed.Seconds(), retryAfter)

		if successes < 100 {
			t.Errorf("bucket allowed only %d requests before 429; burst 100 must allow at least 100", successes)
		}
		// Retry-After 只断言「合理正数」的上界 60s（限流器自身的兜底值就是
		// 60；正常回填约 50ms–1s）。不精确等于任何具体秒数。
		secs, err := strconv.Atoi(retryAfter)
		if err != nil || secs < 1 || secs > 60 {
			t.Errorf("Retry-After=%q is not a reasonable positive integer (want 1..60)", retryAfter)
		}
		if got := limitedBody["error"]; got != "rate_limited" {
			t.Errorf("429 body error=%v, want rate_limited (%v)", got, limitedBody)
		}

		// 看板桶已耗尽，但 coverage 的独立 1/s burst 30 桶必须仍可用。
		// 这里只断言 coverage 200：看板桶按 20 QPS 回填（每 50ms 一个令牌），
		// 在 coverage 请求返回后再断言「看板仍 429」会和回填赛跑，属于时间窗
		// 竞态，不能作为验收条件，因此不设。
		cov := usageAcceptanceDo(engine, http.MethodGet, "/user/usage/tokens/coverage", token, "")
		if cov.Status != http.StatusOK {
			t.Errorf("coverage after dashboard drain: got %d — coverage must own an independent bucket", cov.Status)
		}
	})
}

func mapKeys(m *sync.Map) []any {
	var keys []any
	m.Range(func(k, _ any) bool {
		keys = append(keys, k)
		return true
	})
	return keys
}

// TestUsageTokenAcceptance_UploadSmokeRegression 是上传侧的**冒烟级**回归：
// batch 成功、重复上传幂等覆盖不翻倍、backfill 可用、coverage 配置与设备
// 可见性未变。
//
// 它**不能**证明「写后在同一 5 秒桶内立即失效」：查询之间的耗时无法保证仍
// 落在同一个缓存桶里，写后读到的也可能是 TTL 自然过期后的新结果。因此这里
// 只在功能层断言「写入后能读到新总量」，不断言失效时序。「成功写后立即失效」
// 的确定性结论由 internal/middleware 的假时钟/代次单测
// （TestUsageQueryCache_InvalidateOnSuccessAndInflightRace）覆盖；
// 「上传/幂等/backfill 行为不变」的结论同时依赖 internal/handler 与
// internal/repo 的既有单测，不能由本冒烟测试单独支撑。
func TestUsageTokenAcceptance_UploadSmokeRegression(t *testing.T) {
	engine, _, _ := setupE2EServer(t)
	token, _ := usageAcceptanceLogin(t, engine, "usage-accept-upload")

	if got := usageAcceptanceSummaryTotal(t, engine, token); got != 0 {
		t.Fatalf("fresh user summary = %d, want 0", got)
	}
	usageAcceptanceUpload(t, engine, token)
	if got := usageAcceptanceSummaryTotal(t, engine, token); got != 50 {
		t.Fatalf("summary after batch = %d, want 50 (smoke: a post-write read must reflect the upload; this does NOT pin same-bucket invalidation timing)", got)
	}

	// 第二次上传同一组桶：幂等覆盖，不累加（契约 §9.4.1）。
	usageAcceptanceUpload(t, engine, token)
	if got := usageAcceptanceSummaryTotal(t, engine, token); got != 50 {
		t.Fatalf("summary after repeat upload = %d, want 50 (re-upload must overwrite, not accumulate)", got)
	}

	// backfill 仍可用：补另一个日期，窗口内总量 +10。
	backfillDay := usageAcceptanceDay(-3).Add(2 * time.Hour)
	body := `{"schema_version":1,"device_id":"` + usageAcceptanceDeviceID + `","backfill":true,"max_days":30,"buckets":[` +
		usageAcceptanceBucketJSON(backfillDay, 4, 6, 10, 1) + `]}`
	resp := usageAcceptanceDo(engine, http.MethodPost, "/user/usage/tokens/backfill", token, body)
	if resp.Status != http.StatusOK {
		t.Fatalf("backfill: got %d — %s", resp.Status, string(resp.Body))
	}
	if got := usageAcceptanceSummaryTotal(t, engine, token); got != 60 {
		t.Fatalf("summary after backfill = %d, want 60", got)
	}

	// coverage 的配置发现语义未变（hour / 180 天）。
	cov := usageAcceptanceDo(engine, http.MethodGet, "/user/usage/tokens/coverage", token, "")
	if cov.Status != http.StatusOK {
		t.Fatalf("coverage: got %d — %s", cov.Status, string(cov.Body))
	}
	var covEnv struct {
		Data struct {
			BucketSize    string `json:"bucket_size"`
			RetentionDays int    `json:"retention_days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cov.Body, &covEnv); err != nil {
		t.Fatalf("decode coverage: %v (%s)", err, string(cov.Body))
	}
	if covEnv.Data.BucketSize != "hour" || covEnv.Data.RetentionDays != 180 {
		t.Errorf("coverage config = %s/%d, want hour/180", covEnv.Data.BucketSize, covEnv.Data.RetentionDays)
	}

	// 设备列表能看到上传自动登记的设备（device read 与看板同桶，仍 200）。
	devices := usageAcceptanceDo(engine, http.MethodGet, "/user/devices", token, "")
	if devices.Status != http.StatusOK {
		t.Fatalf("devices: got %d — %s", devices.Status, string(devices.Body))
	}
	if !strings.Contains(string(devices.Body), usageAcceptanceDeviceID) {
		t.Errorf("devices response does not list %s: %s", usageAcceptanceDeviceID, string(devices.Body))
	}
}
