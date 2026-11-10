package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// protocol_error_edges_test.go — Task 16 覆盖率补强：Messages/Responses
// 的错误分类学与 handler 分支（各自协议的原生形状；拒绝先于准入）。

func callRawProto(f *protoFixture, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

func TestV1Messages_ErrorTaxonomy(t *testing.T) {
	f := newProtoFixture(t)
	call := func(body string, headers map[string]string) (int, map[string]any) {
		t.Helper()
		w := callRawProto(f, "/v1/messages", body, headers)
		var doc map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return w.Code, doc
	}

	// 401：无凭据 → authentication_error（Anthropic 信封）。
	code, doc := call(`{"model":"glm-4.6","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, nil)
	if code != http.StatusUnauthorized || doc["type"] != "error" {
		t.Fatalf("401 = %d %v", code, doc)
	}
	if doc["error"].(map[string]any)["type"] != "authentication_error" {
		t.Errorf("401 type = %v", doc)
	}

	// 403：权益吊销后调用 → permission_error（model_not_allowed）。
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET status = 'revoked'`); err != nil {
		t.Fatal(err)
	}
	code, doc = call(`{"model":"glm-4.6","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`,
		map[string]string{"Authorization": "Bearer " + f.keyPlain})
	if code != http.StatusForbidden || doc["error"].(map[string]any)["type"] != "permission_error" {
		t.Fatalf("403 = %d %v", code, doc)
	}
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET status = 'active'`); err != nil {
		t.Fatal(err)
	}

	// 400：无法兼容字段（多模态）→ invalid_request_error。
	code, doc = call(`{"model":"glm-4.6","max_tokens":1,
		"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}]}]}`,
		map[string]string{"Authorization": "Bearer " + f.keyPlain})
	if code != http.StatusBadRequest || doc["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("400 = %d %v", code, doc)
	}

	// 429：额度耗尽 → rate_limit_error + Retry-After。
	if _, err := f.db.Exec(`UPDATE inference_policy_versions SET five_hour_limit_micros = 1 WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	code, doc = call(`{"model":"glm-4.6","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`,
		map[string]string{"Authorization": "Bearer " + f.keyPlain})
	if code != http.StatusTooManyRequests {
		t.Fatalf("429 = %d %v", code, doc)
	}
	if doc["error"].(map[string]any)["type"] != "rate_limit_error" {
		t.Errorf("429 type = %v, want rate_limit_error", doc)
	}
	if _, err := f.db.Exec(`UPDATE inference_policy_versions SET five_hour_limit_micros = 1000000000 WHERE id = $1`, f.policyID); err != nil {
		t.Fatal(err)
	}
	// 上述拒绝全部先于准入：零请求行。
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("requests = %d, want 0 (拒绝先于准入)", n)
	}
	// 529：上游不可用（唯一账号观测额度归零）→ overloaded_error。
	if _, err := f.db.Exec(`UPDATE inference_upstream_accounts
		SET quota_limit_micros = 1000, quota_remaining_micros = 0,
		    quota_observed_at = now(), quota_source = 'reported',
		    quota_reset_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	w := callRawProto(f, "/v1/messages", `{"model":"glm-4.6","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`,
		map[string]string{"Authorization": "Bearer " + f.keyPlain})
	if w.Code != 529 {
		t.Fatalf("upstream exhausted = %d %s, want 529", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "overloaded_error") {
		t.Errorf("529 body = %s, want overloaded_error", w.Body.String())
	}
	// 529 的请求行：released + 零扣费（预占释放有审计，非"无痕迹"——
	// 与失败演练同一语义）。
	var status string
	var charges int
	if err := f.db.QueryRow(
		`SELECT (SELECT status FROM inference_requests),
		        (SELECT COUNT(*) FROM inference_ledger_entries)`).Scan(&status, &charges); err != nil {
		t.Fatal(err)
	}
	if status != "released" || charges != 0 {
		t.Errorf("529 request = %s charges=%d, want released/0", status, charges)
	}
}

func TestV1Responses_HandlerBranches(t *testing.T) {
	f := newProtoFixture(t)
	call := func(body string) (int, string) {
		t.Helper()
		w := callRawProto(f, "/v1/responses", body,
			map[string]string{"Authorization": "Bearer " + f.keyPlain})
		return w.Code, w.Body.String()
	}

	// 未知模型 → 404 OpenAI 形状。
	code, body := call(`{"model":"no-such","input":"hi"}`)
	if code != http.StatusNotFound || !strings.Contains(body, "model_not_found") {
		t.Fatalf("unknown model = %d %s", code, body)
	}
	// 权益吊销 → 403 model_not_allowed。
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET status = 'revoked'`); err != nil {
		t.Fatal(err)
	}
	code, body = call(`{"model":"glm-4.6","input":"hi"}`)
	if code != http.StatusForbidden || !strings.Contains(body, "model_not_allowed") {
		t.Fatalf("unentitled = %d %s", code, body)
	}
	if _, err := f.db.Exec(`UPDATE inference_entitlements SET status = 'active'`); err != nil {
		t.Fatal(err)
	}
	// 畸形 body → 400。
	code, body = call(`{not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad body = %d %s", code, body)
	}
	// previous_response_id 不存在 → 404 previous_response_not_found（跨客户无差别）。
	code, body = call(`{"model":"glm-4.6","input":"hi","previous_response_id":"resp_00000000-0000-0000-0000-000000000000"}`)
	if code != http.StatusNotFound || !strings.Contains(body, "previous_response_not_found") {
		t.Fatalf("stale chain ref = %d %s", code, body)
	}
	// truncation:auto → 400 且零痕迹。
	code, body = call(`{"model":"glm-4.6","input":"hi","truncation":"auto"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "truncation") {
		t.Fatalf("truncation = %d %s", code, body)
	}
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM inference_requests`); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("requests = %d, want 0 (拒绝先于准入)", n)
	}
}

// ---------------------------------------------------------------------------
// T6 协议矩阵：三个 /v1 入口 × 五类限制
//
// （业务 RPM、业务 TPM、并发、金额额度、永久 Key 预算 + 窗口；外围每 IP
// 那一格在 internal/router 的装配测试里断言。）
//
// 每一格都必须同时满足三件事：
//  1. 状态码与业务码正确（业务速率 / 金额额度 / 并发三者严格分开）；
//  2. 错误按**实际入口协议**渲染 —— OpenAI 的 error 对象、Anthropic 的
//     原生结构 —— 绝不出现管理 envelope（设计 §9.1）；
//  3. 拒绝发生在任何上游调用之前（零上游触达）。
//
// 外围每 IP 429 由通用中间件产生，挂在 /v1 group 外层，形状由
// router 装配决定 —— 那一格在 internal/router 的装配测试里断言。
// ---------------------------------------------------------------------------

// protoEntry 是协议矩阵里的一个 /v1 入口。
type protoEntry struct {
	name string
	path string
	body map[string]any
	// anthropic = true → 该入口用 Anthropic 原生错误结构
	// （{"type":"error","error":{"type":…,"message":…}}）。
	anthropic bool
}

func protoMatrixEntries() []protoEntry {
	return []protoEntry{
		{"chat_completions", "/v1/chat/completions", map[string]any{
			"model":    "glm-4.6",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		}, false},
		{"messages", "/v1/messages", map[string]any{
			"model":      "glm-4.6",
			"max_tokens": 64,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		}, true},
		{"responses", "/v1/responses", map[string]any{
			"model": "glm-4.6",
			"input": "hi",
		}, false},
	}
}

// assertNativeLimitError 断言一个 429 拒绝按入口协议渲染：状态码、JSON
// Content-Type、原生错误结构、业务码与业务说明。
func assertNativeLimitError(t *testing.T, w *httptest.ResponseRecorder, anthropic bool, wantCode, wantMsg string) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if _, leaked := doc["code"]; leaked {
		t.Errorf("management envelope leaked into /v1: %s", w.Body.String())
	}
	errObj, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("native error object missing: %s", w.Body.String())
	}
	if anthropic {
		if doc["type"] != "error" {
			t.Errorf("anthropic envelope = %v, want type=error: %s", doc["type"], w.Body.String())
		}
		if errObj["type"] != "rate_limit_error" {
			t.Errorf("anthropic error.type = %v, want rate_limit_error: %s", errObj["type"], w.Body.String())
		}
	} else {
		if errObj["type"] != "rate_limit_error" {
			t.Errorf("error.type = %v, want rate_limit_error: %s", errObj["type"], w.Body.String())
		}
		if errObj["code"] != wantCode {
			t.Errorf("error.code = %v, want %s: %s", errObj["code"], wantCode, w.Body.String())
		}
	}
	msg, _ := errObj["message"].(string)
	if wantMsg != "" && !strings.Contains(msg, wantMsg) {
		t.Errorf("error.message = %q, want it to name %q", msg, wantMsg)
	}
}

// drainUpstream 清空已记录的上游请求体,返回清掉的条数。
func drainUpstream(f *protoFixture) int {
	n := 0
	for {
		select {
		case <-f.lastBodies:
			n++
		default:
			return n
		}
	}
}

// TestV1ProtocolMatrix_BusinessRateLimit 业务速率限制（Key RPM）：第一个
// 请求放行并计入滚动窗口，第二个必须被拒 —— 429 rate_limited，零上游触达。
func TestV1ProtocolMatrix_BusinessRateLimit(t *testing.T) {
	for _, e := range protoMatrixEntries() {
		t.Run(e.name, func(t *testing.T) {
			f := newProtoFixture(t)
			// Key 级 RPM = 1：第二个请求必然超限。
			if _, err := f.db.Exec(`UPDATE inference_api_keys SET rpm_limit = 1 WHERE id = $1`, f.keyID); err != nil {
				t.Fatal(err)
			}
			// 第一个请求放行（计入滚动窗口）。
			w := f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			if w.Code != http.StatusOK {
				t.Fatalf("first request = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if n := drainUpstream(f); n != 1 {
				t.Fatalf("upstream calls = %d, want 1", n)
			}
			// 第二个请求：被业务速率闸门拒绝，零上游触达。
			w = f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			assertNativeLimitError(t, w, e.anthropic, "rate_limited", "rate limit exceeded")
			if n := drainUpstream(f); n != 0 {
				t.Errorf("upstream calls after the rejection = %d, want 0", n)
			}
			// RPM 的恢复时刻可确定（最旧计数行滚出 60 秒窗口）→ 带 Retry-After。
			if retry := w.Header().Get("Retry-After"); retry == "" {
				t.Error("Retry-After must be present when every blocker has a determinable recovery instant")
			}
		})
	}
}

// TestV1ProtocolMatrix_BusinessTokenLimit 账户级 TPM：策略 TPM 上限被击穿
// 时是独立的 429 rate_limited；在途/未知用量无法确定恢复时刻时**不得**
// 编造 Retry-After。
func TestV1ProtocolMatrix_BusinessTokenLimit(t *testing.T) {
	for _, e := range protoMatrixEntries() {
		t.Run(e.name, func(t *testing.T) {
			f := newProtoFixture(t)
			if _, err := f.db.Exec(`UPDATE inference_policy_versions SET tpm_limit = 1 WHERE id = $1`, f.policyID); err != nil {
				t.Fatal(err)
			}
			w := f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			assertNativeLimitError(t, w, e.anthropic, "rate_limited", "policy_tpm")
			if n := drainUpstream(f); n != 0 {
				t.Errorf("upstream calls = %d, want 0 (拒绝先于准入)", n)
			}
			// 恢复时刻不可确定（没有任何已结算行可滚出窗口）→ 不编造倒计时。
			if retry := w.Header().Get("Retry-After"); retry != "" {
				t.Errorf("Retry-After = %q, want none (恢复时刻不可确定时不得编造)", retry)
			}
		})
	}
}

// TestV1ProtocolMatrix_ConcurrencyLimit 并发上限（Key 级）：第一个请求在途
// 占住槽位，第二个必须被拒 —— 429 insufficient_capacity，零上游触达。
func TestV1ProtocolMatrix_ConcurrencyLimit(t *testing.T) {
	for _, e := range protoMatrixEntries() {
		t.Run(e.name, func(t *testing.T) {
			f := newProtoFixture(t)
			if _, err := f.db.Exec(`UPDATE inference_api_keys SET concurrency_limit = 1 WHERE id = $1`, f.keyID); err != nil {
				t.Fatal(err)
			}
			// 上游阻塞：让第一个请求一直占着 Key 槽位。
			release := make(chan struct{})
			arrived := make(chan struct{}, 4)
			orig := f.upstream.Config.Handler
			f.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case arrived <- struct{}{}:
				default:
				}
				<-release
				orig.ServeHTTP(w, r)
			})
			defer func() { f.upstream.Config.Handler = orig }()

			first := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				first <- f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			}()
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				close(release)
				t.Fatal("the first request never reached the upstream")
			}
			// 槽位已占：第二个请求必须在准入阶段被拒。
			w := f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			close(release)
			<-first
			assertNativeLimitError(t, w, e.anthropic, "insufficient_capacity", "concurrency limit")
			if n := drainUpstream(f); n != 1 {
				t.Errorf("upstream calls = %d, want 1 (只有第一个请求出站)", n)
			}
		})
	}
}

// TestV1ProtocolMatrix_MoneyQuota 金额额度耗尽：429 quota_exceeded，与业务
// 速率限制严格分开；窗口重置时刻可确定 → 带 Retry-After。
func TestV1ProtocolMatrix_MoneyQuota(t *testing.T) {
	for _, e := range protoMatrixEntries() {
		t.Run(e.name, func(t *testing.T) {
			f := newProtoFixture(t)
			if _, err := f.db.Exec(`UPDATE inference_policy_versions SET five_hour_limit_micros = 1 WHERE id = $1`, f.policyID); err != nil {
				t.Fatal(err)
			}
			w := f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			assertNativeLimitError(t, w, e.anthropic, "quota_exceeded", "quota exceeded")
			if n := drainUpstream(f); n != 0 {
				t.Errorf("upstream calls = %d, want 0 (拒绝先于准入)", n)
			}
			if retry := w.Header().Get("Retry-After"); retry == "" {
				t.Error("Retry-After must be present when every blocking window has a determinable reset")
			}
		})
	}
}

// TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown Key 的累计预算
// （永久阻断项）与窗口同时阻断时：不得给出编造的倒计时，且必须点名
// key budget —— 窗口重置永远解不开一个耗尽的 Key 预算。
func TestV1ProtocolMatrix_PermanentKeyBudgetCarriesNoCountdown(t *testing.T) {
	for _, e := range protoMatrixEntries() {
		t.Run(e.name, func(t *testing.T) {
			f := newProtoFixture(t)
			// Key 累计预算耗尽（不按周期清零 → 永久）。
			if _, err := f.db.Exec(`UPDATE inference_api_keys SET budget_limit_micros = 1 WHERE id = $1`, f.keyID); err != nil {
				t.Fatal(err)
			}
			// 同时五小时窗口也阻断。
			if _, err := f.db.Exec(`UPDATE inference_policy_versions SET five_hour_limit_micros = 1 WHERE id = $1`, f.policyID); err != nil {
				t.Fatal(err)
			}
			w := f.call(t, http.MethodPost, e.path, f.keyPlain, e.body)
			assertNativeLimitError(t, w, e.anthropic, "quota_exceeded", "quota exceeded")
			if !strings.Contains(w.Body.String(), "key budget exhausted") {
				t.Errorf("the permanent blocker must be named: %s", w.Body.String())
			}
			if retry := w.Header().Get("Retry-After"); retry != "" {
				t.Errorf("Retry-After = %q, want none (Key 预算耗尽是永久阻断项)", retry)
			}
			if n := drainUpstream(f); n != 0 {
				t.Errorf("upstream calls = %d, want 0 (拒绝先于准入)", n)
			}
		})
	}
}
