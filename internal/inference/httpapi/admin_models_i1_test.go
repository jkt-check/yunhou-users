package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// admin_models_i1_test.go — 安全审查 I-1：PATCH /admin/models/:id 不再接受
// lifecycle（DTO 无该字段，strictBindJSON 按未知字段 400——生命周期只能经
// 专用 POST /models/:id/lifecycle 状态机端点流转）；PATCH 省略字段 read-
// modify-write 保留存量值；POST /models 拒收显式非 draft lifecycle。

func TestAdminModelPatch_RejectsLifecycleField(t *testing.T) {
	engine, _ := newReasonTestServer(t)

	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "i1-patch-lc", "display_name": "M",
		"context_tokens": 1024, "max_output_tokens": 128,
		"protocols": []string{"openai_chat"},
	}, http.StatusOK)

	// 无论值是否「合法」，PATCH 携带 lifecycle 字段一律 400——绕过状态机的
	// 通道必须不存在，而不是按值挑着放行。
	env := do(t, engine, http.MethodPatch, "/admin/models/i1-patch-lc", map[string]any{
		"lifecycle": "active",
	}, http.StatusBadRequest)
	if env.Message == "" {
		t.Error("lifecycle rejection must carry a message")
	}
}

func TestAdminModelCreate_RejectsDirectActive(t *testing.T) {
	engine, _ := newReasonTestServer(t)

	for _, lc := range []string{"active", "deprecated", "retired"} {
		do(t, engine, http.MethodPost, "/admin/models", map[string]any{
			"id": "i1-create-" + lc, "display_name": "M", "lifecycle": lc,
			"context_tokens": 1024, "max_output_tokens": 128,
			"protocols": []string{"openai_chat"},
		}, http.StatusBadRequest)
	}

	// 显式 draft 是合法的，且与缺省等价。
	for _, body := range []map[string]any{
		{"id": "i1-create-draft", "display_name": "M", "lifecycle": "draft",
			"context_tokens": 1024, "max_output_tokens": 128, "protocols": []string{"openai_chat"}},
		{"id": "i1-create-omit", "display_name": "M",
			"context_tokens": 1024, "max_output_tokens": 128, "protocols": []string{"openai_chat"}},
	} {
		env := do(t, engine, http.MethodPost, "/admin/models", body, http.StatusOK)
		var got struct {
			Lifecycle string `json:"lifecycle"`
		}
		if err := json.Unmarshal(env.Data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Lifecycle != "draft" {
			t.Errorf("create lifecycle = %q, want draft", got.Lifecycle)
		}
	}
}

func TestAdminModelPatch_PartialUpdatePreservesFields(t *testing.T) {
	engine := newTestServer(t)

	do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "i1-patch-partial", "display_name": "Before",
		"context_tokens": 4096, "max_output_tokens": 512,
		"protocols": []string{"openai_chat"}, "aliases": []string{"alias-1"},
		"supports_tools": true,
	}, http.StatusOK)

	// 激活后只改 display_name（I-1 场景：运营改名绝不允许把在售模型打回
	// 草稿，也不得碰其它字段）。
	do(t, engine, http.MethodPost, "/admin/models/i1-patch-partial/lifecycle", map[string]any{
		"lifecycle": "active", "reason": "onboard",
	}, http.StatusOK)

	env := do(t, engine, http.MethodGet, "/admin/models/i1-patch-partial", nil, http.StatusOK)
	var before struct {
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(env.Data, &before); err != nil {
		t.Fatal(err)
	}

	do(t, engine, http.MethodPatch, "/admin/models/i1-patch-partial", map[string]any{
		"display_name": "After", "updated_at": before.UpdatedAt,
	}, http.StatusOK)

	env = do(t, engine, http.MethodGet, "/admin/models/i1-patch-partial", nil, http.StatusOK)
	var after struct {
		DisplayName     string   `json:"display_name"`
		Lifecycle       string   `json:"lifecycle"`
		ContextTokens   int      `json:"context_tokens"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Protocols       []string `json:"protocols"`
		Aliases         []string `json:"aliases"`
		SupportsTools   bool     `json:"supports_tools"`
		ModelVersion    string   `json:"model_version"`
	}
	if err := json.Unmarshal(env.Data, &after); err != nil {
		t.Fatal(err)
	}
	if after.DisplayName != "After" {
		t.Errorf("display_name = %q, want After", after.DisplayName)
	}
	if after.Lifecycle != "active" {
		t.Errorf("lifecycle = %q, want active (改名不得重置生命周期)", after.Lifecycle)
	}
	if after.ContextTokens != 4096 || after.MaxOutputTokens != 512 {
		t.Errorf("tokens = %d/%d, want 4096/512 preserved", after.ContextTokens, after.MaxOutputTokens)
	}
	if len(after.Protocols) != 1 || after.Protocols[0] != "openai_chat" {
		t.Errorf("protocols = %v, want preserved", after.Protocols)
	}
	if len(after.Aliases) != 1 || after.Aliases[0] != "alias-1" {
		t.Errorf("aliases = %v, want preserved", after.Aliases)
	}
	if !after.SupportsTools {
		t.Error("supports_tools must be preserved")
	}
}

func TestAdminModelWrite_RejectsUnknownFields(t *testing.T) {
	engine, _ := newReasonTestServer(t)

	// create 面上的伪造字段（如自报 actor）被 strictBindJSON 拒绝。
	env := do(t, engine, http.MethodPost, "/admin/models", map[string]any{
		"id": "i1-unk", "display_name": "M",
		"context_tokens": 1024, "max_output_tokens": 128,
		"protocols": []string{"openai_chat"}, "actor": "user:evil@app:x",
	}, http.StatusBadRequest)
	if env.Message == "" {
		t.Error("unknown-field rejection must carry a message")
	}
}
