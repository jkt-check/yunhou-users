package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
