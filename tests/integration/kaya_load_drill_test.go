package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// kaya_load_drill_test.go — Task 16 受控吞吐/长连接压测（控制者决定 5）。
// 非默认门禁：KAYA_LOAD=1 才运行（可重复演练；实测记录写进
// docs/runbooks/kaya-coding-plan-rollout.md §压测）。
//
// 观测面（每项都打到 t.Log 并做宽松正确性断言）：
//   - DB 锁等待：压测期间 pg_stat_activity 中 wait_event_type='Lock' 的
//     会话数峰值 + pg_locks 未授予峰值（50ms 采样）；
//   - 连接池占用：sql.DBStats 的 InUse/WaitCount/WaitDuration 前后差；
//   - SSE 内存：N 条并发慢流期间 HeapAlloc 增量 / 并发数；
//   - 结算延迟：响应返回 → 请求行 status='settled' 的追加等待时间；
//   - 正确性：全部请求恰结算一次、无错误、账本与窗口一枚（drillInvariant）。

func loadGate(t *testing.T) {
	t.Helper()
	if os.Getenv("KAYA_LOAD") == "" {
		t.Skip("load drill: set KAYA_LOAD=1 to run (非默认门禁)")
	}
}

// lockSampler samples lock waits on the current DB until done closes.
type lockSampler struct {
	mu           sync.Mutex
	maxLockWaits int
	maxUngranted int
}

func (s *lockSampler) run(ctx context.Context, db *sqlx.DB, done <-chan struct{}) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var lw, ug int
		if err := db.Get(&lw, `
			SELECT COUNT(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`); err == nil {
			s.mu.Lock()
			if lw > s.maxLockWaits {
				s.maxLockWaits = lw
			}
			s.mu.Unlock()
		}
		if err := db.Get(&ug, `
			SELECT COUNT(*) FROM pg_locks WHERE NOT granted`); err == nil {
			s.mu.Lock()
			if ug > s.maxUngranted {
				s.maxUngranted = ug
			}
			s.mu.Unlock()
		}
	}
}

// settleLag polls until the newest request settles; returns the extra wait
// beyond the HTTP response（结算延迟）.
func settleLag(t *testing.T, db *sqlx.DB, requestID string) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := start.Add(5 * time.Second)
	for {
		var status string
		if err := db.Get(&status, `SELECT status FROM inference_requests WHERE id = $1`, requestID); err == nil &&
			(status == "settled" || status == "released" || status == "failed") {
			return time.Since(start)
		}
		if time.Now().After(deadline) {
			t.Fatalf("request %s not terminal within 5s (结算积压)", requestID)
		}
		time.Sleep(time.Millisecond)
	}
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

// TestLoadDrill_Throughput: 受控吞吐 — 24 路 worker 各 10 次非流式调用
// （240 次），池上限 20。断言全部成功且恰结算一次。
func TestLoadDrill_Throughput(t *testing.T) {
	loadGate(t)
	st := newDrillStack(t, 1_000_000_000)
	st.db.SetMaxOpenConns(20)
	st.db.SetMaxIdleConns(20)

	// 上游同时应答流式/非流式（非流式走 JSON 一次返回）。
	st.upstream.set(openAICompletionHandler)

	direct, err := sqlx.Connect("postgres", dbURL())
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	sampler := &lockSampler{}
	done := make(chan struct{})
	go sampler.run(context.Background(), direct, done)

	before := st.db.Stats()
	start := time.Now()
	const workers, callsEach = 24, 10
	var wg sync.WaitGroup
	var failures int64
	lat := make([]time.Duration, 0, workers*callsEach)
	var latMu sync.Mutex
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < callsEach; i++ {
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
					`{"model":"drill-model","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`))
				req.Header.Set("Authorization", "Bearer "+st.keyPlain)
				rec := httptest.NewRecorder()
				c0 := time.Now()
				st.engine.ServeHTTP(rec, req)
				elapsed := time.Since(c0)
				if rec.Code != http.StatusOK {
					atomic.AddInt64(&failures, 1)
					continue
				}
				latMu.Lock()
				lat = append(lat, elapsed)
				latMu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(done)
	after := st.db.Stats()

	// 结算延迟抽样：最后 20 个请求逐个等到终态。
	lags := []time.Duration{}
	var ids []string
	if err := st.db.Select(&ids, `SELECT id::text FROM inference_requests ORDER BY created_at DESC LIMIT 20`); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		lags = append(lags, settleLag(t, st.db, id))
	}
	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })

	total := workers * callsEach
	var settledCount int
	if err := st.db.Get(&settledCount, `SELECT COUNT(*) FROM inference_requests WHERE status = 'settled'`); err != nil {
		t.Fatal(err)
	}
	var chargeTotal int64
	if err := st.db.Get(&chargeTotal, `SELECT COALESCE(SUM(amount_micros),0) FROM inference_ledger_entries WHERE entry_type='charge'`); err != nil {
		t.Fatal(err)
	}

	t.Logf("THROUGHPUT: %d calls in %s = %.1f req/s (failures=%d)",
		total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), failures)
	t.Logf("LATENCY: p50=%s p95=%s max=%s", pct(lat, 0.50), pct(lat, 0.95), pct(lat, 1.0))
	t.Logf("SETTLE-LAG: p50=%s p95=%s max=%s (n=%d)", pct(lags, 0.50), pct(lags, 0.95), pct(lags, 1.0), len(lags))
	t.Logf("POOL: InUse(max)=%d WaitCount +%d WaitDuration +%s MaxOpen=%d",
		after.MaxIdleClosed, after.WaitCount-before.WaitCount, (after.WaitDuration - before.WaitDuration).Round(time.Millisecond), 20)
	t.Logf("POOL-STATS-BEFORE: %+v", before)
	t.Logf("POOL-STATS-AFTER:  %+v", after)
	sampler.mu.Lock()
	t.Logf("LOCKS: max lock-waiting sessions = %d, max ungranted pg_locks = %d", sampler.maxLockWaits, sampler.maxUngranted)
	sampler.mu.Unlock()

	if failures != 0 {
		t.Errorf("failures = %d", failures)
	}
	if settledCount != total {
		t.Errorf("settled = %d, want %d (每请求恰结算一次)", settledCount, total)
	}
	// 每次 17 micro（7 in + 5 out @ 1e6/2e6 per Mtok）。
	if want := int64(total) * 17; chargeTotal != want {
		t.Errorf("charge total = %d, want %d", chargeTotal, want)
	}
	drillInvariant(t, st)
}

// TestLoadDrill_LongConnections: 长连接 — 32 条并发慢速 SSE（上游每 50ms
// 滴一个 chunk，约 1s/流），观测 SSE 内存增量与池占用，全部正确结算。
func TestLoadDrill_LongConnections(t *testing.T) {
	loadGate(t)
	st := newDrillStack(t, 1_000_000_000)
	st.db.SetMaxOpenConns(20)
	st.db.SetMaxIdleConns(20)

	// 慢速上游：20 个内容 chunk × 50ms，随后 usage + [DONE]。
	st.upstream.set(func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-slow\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-slow\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})

	direct, err := sqlx.Connect("postgres", dbURL())
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	sampler := &lockSampler{}
	done := make(chan struct{})
	go sampler.run(context.Background(), direct, done)

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	before := st.db.Stats()

	const streams = 32
	start := time.Now()
	var wg sync.WaitGroup
	var failures int64
	wg.Add(streams)
	for i := 0; i < streams; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"drill-model","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":true}`))
			req.Header.Set("Authorization", "Bearer "+st.keyPlain)
			rec := httptest.NewRecorder()
			st.engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data: [DONE]") {
				atomic.AddInt64(&failures, 1)
			}
		}()
	}
	// 流中点采样内存（所有流都应活着）。
	time.Sleep(500 * time.Millisecond)
	var memMid runtime.MemStats
	runtime.ReadMemStats(&memMid)
	wg.Wait()
	elapsed := time.Since(start)
	close(done)

	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	after := st.db.Stats()

	heapMidKB := int64(memMid.HeapAlloc) / 1024
	heapBeforeKB := int64(memBefore.HeapAlloc) / 1024
	perStreamKB := (heapMidKB - heapBeforeKB) / streams
	t.Logf("LONG-CONN: %d concurrent SSE × ~1s in %s (failures=%d)", streams, elapsed.Round(time.Millisecond), failures)
	t.Logf("SSE-MEM: heap before %d KiB → mid %d KiB（Δ %d KiB ≈ %d KiB/stream）→ after %d KiB",
		heapBeforeKB, heapMidKB, heapMidKB-heapBeforeKB, perStreamKB, int64(memAfter.HeapAlloc)/1024)
	t.Logf("POOL: WaitCount +%d WaitDuration +%s",
		after.WaitCount-before.WaitCount, (after.WaitDuration - before.WaitDuration).Round(time.Millisecond))
	sampler.mu.Lock()
	t.Logf("LOCKS: max lock-waiting sessions = %d, max ungranted pg_locks = %d", sampler.maxLockWaits, sampler.maxUngranted)
	sampler.mu.Unlock()

	var settledCount int
	if err := st.db.Get(&settledCount, `SELECT COUNT(*) FROM inference_requests WHERE status = 'settled'`); err != nil {
		t.Fatal(err)
	}
	if failures != 0 || settledCount != streams {
		t.Errorf("failures = %d settled = %d, want 0/%d (每条长连接正确结算)", failures, settledCount, streams)
	}
	drillInvariant(t, st)
}
