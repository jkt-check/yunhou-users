package httpapi_test

import (
	"net/http"
	"testing"
)

// admin_models_m6_test.go — 安全审查 M-6：catalog 写面统一 strict JSON 绑定
// （未知字段 400，与 credentials/accounts 等运营面同口径）+ 所有写变更
// reason 必填（空串/纯空白/缺省一律 400，不 unnamed 落审计）。

func TestAdminCatalogWrite_StrictBindingAndRequiredReason(t *testing.T) {
	engine, _ := newReasonTestServer(t)

	call := func(method, path string, body any) envelope {
		t.Helper()
		return do(t, engine, method, path, body, http.StatusBadRequest)
	}

	// 每个有请求体的写端点：未知字段 → 400。
	call(http.MethodPost, "/admin/models", map[string]any{
		"id": "m6-unk", "display_name": "M", "context_tokens": 1024, "max_output_tokens": 128,
		"protocols": []string{"openai_chat"}, "reason": "seed", "lifecycle_smuggle": "active",
	})
	call(http.MethodPost, "/admin/providers", map[string]any{
		"code": "m6-prov", "display_name": "P", "access_type": "official_api",
		"reason": "seed", "forged_actor": "user:evil@app:x",
	})
	call(http.MethodPost, "/admin/deployments", map[string]any{
		"provider_id": "00000000-0000-0000-0000-000000000000", "upstream_model": "up",
		"base_url": "https://api.example.com", "protocol": "openai_chat",
		"reason": "seed", "unknown_field": true,
	})

	// 每个写端点：缺 reason / 空串 / 纯空白 → 400。先造一个合法模型作靶子。
	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "m6-reason", "display_name": "M", "context_tokens": 1024, "max_output_tokens": 128,
		"protocols": []string{"openai_chat"}, "reason": "seed",
	}, http.StatusOK)

	call(http.MethodPost, "/admin/models", map[string]any{
		"id": "m6-nr", "display_name": "M", "context_tokens": 1024, "max_output_tokens": 128,
		"protocols": []string{"openai_chat"},
	})
	call(http.MethodPost, "/admin/models/m6-reason/lifecycle", map[string]any{
		"lifecycle": "deprecated",
	})
	call(http.MethodPost, "/admin/models/m6-reason/lifecycle", map[string]any{
		"lifecycle": "deprecated", "reason": "   ",
	})
	call(http.MethodDelete, "/admin/models/m6-reason?reason=", nil)
	call(http.MethodDelete, "/admin/models/m6-reason", nil)
	call(http.MethodPost, "/admin/catalog/rollback", map[string]any{"to_revision": 1})
	call(http.MethodPost, "/admin/catalog/rollback", map[string]any{"to_revision": 1, "reason": ""})
	call(http.MethodPost, "/admin/catalog/publish", nil)

	// DELETE 带合法 reason 放行（且模型确被删除，证明 400 只卡理由不卡路径）。
	do(t, engine, http.MethodDelete, "/admin/models/m6-reason?reason=cleanup", nil, http.StatusOK)
}

func TestAdminCatalogWrite_ReasonLandsInAudit(t *testing.T) {
	engine, rec := newReasonTestServer(t)

	// publish 的 ?reason= 同样进审计（此前只有 body 端点被断言过）。
	do(t, engine, http.MethodPost, "/admin/catalog/publish?reason=weekly%20ship", nil, http.StatusOK)
	if ev := rec.last(); ev.Action != "catalog.publish" || ev.Reason != "weekly ship" {
		t.Fatalf("publish audit = %+v, want catalog.publish with reason", ev)
	}
}
