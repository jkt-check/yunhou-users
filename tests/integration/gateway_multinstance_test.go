// gateway_multinstance_test.go — T10：两个独立 Service 实例 + 两个独立
// DB pool 的共享限流验收。
//
// 这里的“两个实例”是真的独立：第二个实例有自己的 *sqlx.DB 连接池、自己的
// postgres.Store、自己的 quota.Service / routing.Service /
// gateway.Service、自己的 gin 引擎 —— 唯一共享的是 PostgreSQL。计数必须
// 经数据库共享；进程内的 goroutine 计数器（同一个 Service 跑两个 goroutine）
// 证明不了多实例语义。
//
// 覆盖：账户级 RPM / TPM / 并发，以及 Key 级 RPM / 并发（Key 作用域）。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
)

// accInstance 是一个独立实例（独立连接池 + 独立 Service + 独立引擎）。
type accInstance struct {
	engine *gin.Engine
	db     *sqlx.DB
}

// newInstance 建第二个（及以后）独立实例：独立 DB pool、独立 store、独立
// 网关 Service、独立 HTTP 引擎 —— 只有 PostgreSQL 与实例 1 共享。
func (st *acceptanceStack) newInstance(t *testing.T) *accInstance {
	t.Helper()
	db2, err := sqlx.Connect("postgres", dbURL())
	if err != nil {
		t.Fatalf("second pool connect: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	store2 := postgres.NewStore(db2)
	gw := gateway.NewService(staticSnapshot{st.snap}, store2,
		access.NewEntitlementResolver(store2, nil), quota.NewService(store2, nil),
		routing.NewService(store2, st.adapters, nil), st.credSvc,
		providers.NewHTTPClient(st.egress), st.egress, nil)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	resolver := access.NewResolver(store2, nil)
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(resolver))
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(gw).Create)
	v1.POST("/messages", httpapi.NewMessagesHandler(gw).Create)
	v1.POST("/responses", httpapi.NewResponsesHandler(gw, store2, nil).Create)
	return &accInstance{engine: engine, db: db2}
}

// setPolicyLimits 改策略上的 RPM / TPM / 并发上限（0 = 不限制）。
func (st *acceptanceStack) setPolicyLimits(t *testing.T, rpm int, tpm int64, concurrency int) {
	t.Helper()
	var rpmArg, tpmArg, concArg interface{}
	if rpm > 0 {
		rpmArg = rpm
	}
	if tpm > 0 {
		tpmArg = tpm
	}
	if concurrency > 0 {
		concArg = concurrency
	}
	if _, err := st.db.Exec(`UPDATE inference_policy_versions
	                        SET rpm_limit=$2, tpm_limit=$3, concurrency_limit=$4 WHERE id=$1`,
		st.policyID, rpmArg, tpmArg, concArg); err != nil {
		t.Fatalf("set policy limits: %v", err)
	}
}

// setKeyLimits 改客户 Key 上的 RPM / 并发上限（0 = 不限制）。
func (st *acceptanceStack) setKeyLimits(t *testing.T, keyID string, rpm, concurrency int) {
	t.Helper()
	var rpmArg, concArg interface{}
	if rpm > 0 {
		rpmArg = rpm
	}
	if concurrency > 0 {
		concArg = concurrency
	}
	if _, err := st.db.Exec(`UPDATE inference_api_keys
	                        SET rpm_limit=$2, concurrency_limit=$3 WHERE id=$1`,
		keyID, rpmArg, concArg); err != nil {
		t.Fatalf("set key limits: %v", err)
	}
}

// callOn 在指定引擎上发起一次入口调用。
func (st *acceptanceStack) callOn(t *testing.T, engine *gin.Engine, e accEntry) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, e.path, strings.NewReader(e.body))
	if e.hasKey {
		req.Header.Set("Authorization", "Bearer "+st.keyPlain)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// errCode 取出 /v1 原生错误形状里的 code（OpenAI 口径）。
func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return payload.Error.Code
}

// accHoldStream 让上游保持流打开（模拟在途请求），直到 release 关闭；
// 释放后补上用量块与 [DONE]，让这条请求正常结算（不留悬挂预占）。
func accHoldStream(release <-chan struct{}, started chan<- struct{}, once *sync.Once) func(w http.ResponseWriter, r *http.Request, body []byte) {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(string(body), `"stream":true`) {
			accJSONOpenAI(w, r, body)
			return
		}
		writeOpenAIChunks(w,
			`{"id":"chatcmpl-hold","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hold"}}]}`)
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			return
		}
		writeOpenAIChunks(w,
			`{"id":"chatcmpl-hold","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
			"[DONE]")
	}
}

// startInFlight 在后台发起一条流式请求并等它真正到达上游（此时准入已提交：
// 预占、速率预占与并发租约都已在数据库里）。
func (st *acceptanceStack) startInFlight(t *testing.T, engine *gin.Engine, e accEntry,
	started <-chan struct{}) {
	t.Helper()
	go func() {
		req := httptest.NewRequest(http.MethodPost, e.path, strings.NewReader(e.body))
		if e.hasKey {
			req.Header.Set("Authorization", "Bearer "+st.keyPlain)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
	}()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the upstream never received the in-flight request (the admission may have been rejected)")
	}
}

// ---------------------------------------------------------------------------
// 账户级 RPM：两个实例共享同一个账户计数
// ---------------------------------------------------------------------------

func TestAcceptance_TwoInstancesShareAccountRPM(t *testing.T) {
	st := newAcceptanceStack(t)
	st.setPolicyLimits(t, 2, 0, 0)
	second := st.newInstance(t)
	e := st.entries()[1] // /v1/chat/completions

	engines := []*gin.Engine{st.engine, second.engine}
	codes := make([]string, 3)
	statuses := make([]int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := st.callOn(t, engines[i%2], e)
			statuses[i] = w.Code
			if w.Code != http.StatusOK {
				codes[i] = errCode(t, w)
			}
		}()
	}
	wg.Wait()

	admitted, limited := 0, 0
	for i := range statuses {
		switch {
		case statuses[i] == http.StatusOK:
			admitted++
		case statuses[i] == http.StatusTooManyRequests && codes[i] == "rate_limited":
			limited++
		default:
			t.Errorf("call %d: status %d code %q, want 200 or 429/rate_limited", i, statuses[i], codes[i])
		}
	}
	if admitted > 2 {
		t.Errorf("admitted = %d, want <= 2 (两个实例必须共享同一个账户 RPM 计数)", admitted)
	}
	if limited < 1 {
		t.Errorf("rate-limited = %d, want >= 1 (RPM=2 时第三次必须被拒)", limited)
	}
	// 计数落在数据库里：账户级速率预占恰好 2 行。
	var rows int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='account'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("account rate rows = %d, want 2 (计数必须持久化在 PostgreSQL)", rows)
	}
}

// ---------------------------------------------------------------------------
// 账户级 TPM：在途预占跨实例阻断
// ---------------------------------------------------------------------------

func TestAcceptance_TwoInstancesShareAccountTPM(t *testing.T) {
	st := newAcceptanceStack(t)
	st.setPolicyLimits(t, 0, 100, 0)
	second := st.newInstance(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	var once sync.Once
	st.openaiUp.set(accHoldStream(release, started, &once))

	// 实例 1：一条在途流（输入上界 20 + 输出 cap 40 = 60 token 预占）。
	inflight := st.entries()[1]
	inflight.body = `{"model":"` + st.openaiModel + `","max_tokens":40,"stream":true,"messages":[{"role":"user","content":"x"}]}`
	st.startInFlight(t, st.engine, inflight, started)

	// 实例 2：再申请 50 token（20 + 30）→ 60 + 50 > 100 → 速率拒绝。
	second2 := st.entries()[1]
	second2.body = `{"model":"` + st.openaiModel + `","max_tokens":30,"stream":true,"messages":[{"role":"user","content":"x"}]}`
	w := st.callOn(t, second.engine, second2)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429 body = %s", w.Code, w.Body.String())
	}
	if code := errCode(t, w); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
}

// ---------------------------------------------------------------------------
// 账户级并发：并发槽跨实例共享
// ---------------------------------------------------------------------------

func TestAcceptance_TwoInstancesShareAccountConcurrency(t *testing.T) {
	st := newAcceptanceStack(t)
	st.setPolicyLimits(t, 0, 0, 1)
	second := st.newInstance(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	var once sync.Once
	st.openaiUp.set(accHoldStream(release, started, &once))

	e := st.entries()[1]
	st.startInFlight(t, st.engine, e, started)

	w := st.callOn(t, second.engine, e)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429 body = %s", w.Code, w.Body.String())
	}
	if code := errCode(t, w); code != "insufficient_capacity" {
		t.Errorf("error code = %q, want insufficient_capacity", code)
	}
}

// ---------------------------------------------------------------------------
// Key 级并发：Key 作用域的槽跨实例共享
// ---------------------------------------------------------------------------

func TestAcceptance_TwoInstancesShareKeyConcurrency(t *testing.T) {
	st := newAcceptanceStack(t)
	// 账户级不限并发，只用 Key 作用域的槽。
	st.setPolicyLimits(t, 0, 0, 0)
	st.setKeyLimits(t, st.keyID, 0, 1)
	second := st.newInstance(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	var once sync.Once
	st.openaiUp.set(accHoldStream(release, started, &once))

	e := st.entries()[1]
	st.startInFlight(t, st.engine, e, started)

	w := st.callOn(t, second.engine, e)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429 body = %s", w.Code, w.Body.String())
	}
	if code := errCode(t, w); code != "insufficient_capacity" {
		t.Errorf("error code = %q, want insufficient_capacity", code)
	}
	// Key 作用域的租约确实落在数据库里（不是进程内的计数器）。
	var keyLeases int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_concurrency_leases
	                          WHERE scope='api_key' AND state='held'`).Scan(&keyLeases); err != nil {
		t.Fatal(err)
	}
	if keyLeases != 1 {
		t.Errorf("held api_key leases = %d, want 1", keyLeases)
	}
}

// ---------------------------------------------------------------------------
// Key 级 RPM：Key 作用域的计数跨实例共享，且不影响别的 Key
// ---------------------------------------------------------------------------

func TestAcceptance_TwoInstancesShareKeyRPM(t *testing.T) {
	st := newAcceptanceStack(t)
	st.setKeyLimits(t, st.keyID, 1, 0)
	second := st.newInstance(t)
	e := st.entries()[1]

	// 实例 1 用掉 Key A 的一分钟额度。
	w1 := st.callOn(t, st.engine, e)
	if w1.Code != http.StatusOK {
		t.Fatalf("call 1: code = %d body = %s", w1.Code, w1.Body.String())
	}
	// 实例 2 用同一把 Key → 必须被拒（Key 级计数经数据库共享）。
	w2 := st.callOn(t, second.engine, e)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("call 2: code = %d, want 429 body = %s", w2.Code, w2.Body.String())
	}
	if code := errCode(t, w2); code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", code)
	}
	// 另一个 Key 不受这把 Key 的额度影响（但仍受账户级约束）。
	otherKey, err := access.NewKeyService(st.store, nil).CreateKey(context.Background(), st.userID,
		access.CreateParams{Name: "second-key"})
	if err != nil {
		t.Fatal(err)
	}
	saved := st.keyPlain
	st.keyPlain = otherKey.Plaintext
	w3 := st.callOn(t, second.engine, e)
	st.keyPlain = saved
	if w3.Code != http.StatusOK {
		t.Fatalf("call 3 (other key): code = %d body = %s", w3.Code, w3.Body.String())
	}
	// Key 级速率预占：Key A 两行（被拒的那次没有计数行），Key B 一行。
	var keyA, keyB int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='key' AND api_key_id=$1`,
		st.keyID).Scan(&keyA); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_rate_reservations WHERE scope='key' AND api_key_id=$1`,
		otherKey.Key.ID).Scan(&keyB); err != nil {
		t.Fatal(err)
	}
	if keyA != 1 || keyB != 1 {
		t.Errorf("key rate rows = %d/%d, want 1/1 (速率拒绝不计数，另一把 Key 独立计数)", keyA, keyB)
	}
}

// ---------------------------------------------------------------------------
// 两个实例的账本/事实共享同一份数据库账本（不是两份本地账本）
// ---------------------------------------------------------------------------

func TestAcceptance_TwoInstancesShareOneLedger(t *testing.T) {
	st := newAcceptanceStack(t)
	second := st.newInstance(t)
	e := st.entries()[1]

	w1 := st.callOn(t, st.engine, e)
	if w1.Code != http.StatusOK {
		t.Fatalf("call 1: code = %d body = %s", w1.Code, w1.Body.String())
	}
	w2 := st.callOn(t, second.engine, e)
	if w2.Code != http.StatusOK {
		t.Fatalf("call 2: code = %d body = %s", w2.Code, w2.Body.String())
	}
	// 两个请求 → 两条 charge 行，且都在实例 2 的连接池上也能读到。
	var charges int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM inference_ledger_entries WHERE entry_type='charge'`).
		Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if charges != 2 {
		t.Errorf("ledger charges = %d, want 2 (两个实例写同一本账)", charges)
	}
	var requests int
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM inference_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2", requests)
	}
	// 两次消费都记进同一批窗口（同一权益）。
	var used []int64
	rows, err := st.db.Query(`SELECT used_micros FROM inference_quota_windows ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		used = append(used, v)
	}
	rows.Close()
	if len(used) != 3 {
		t.Fatalf("windows = %d, want 3", len(used))
	}
	for i, v := range used {
		if v != 2*accChargeMicros {
			t.Errorf("window %d used = %d, want %d", i, v, 2*accChargeMicros)
		}
	}
}
