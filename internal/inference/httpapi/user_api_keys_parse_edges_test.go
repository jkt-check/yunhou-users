package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// user_api_keys_parse_edges_test.go — Task 16 覆盖率补强：Key 创建入参解
// 析分支（budget 字符串/裸整数、expires RFC3339、名称上限、未知字段拒绝）。

func TestUserAPIKeys_CreateParseBranches(t *testing.T) {
	f := newAccessFixture(t, 0)
	userID, token := f.addUser(t)
	f.grantEntitlement(t, userID, "glm-4.6")

	// budget 裸 JSON 整数 → 创建端严格字符串契约（400；裸整数便利仅在
	// PATCH 的 parseMicrosJSON 路径，见下方）。
	w := f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-int", "budget_micros": 5000,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bare-int budget at create = %d %s, want 400 (十进制字符串契约)", w.Code, w.Body.String())
	}
	// budget 十进制字符串 → 通过。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-str", "budget_micros": "6000",
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("string budget = %d %s", w.Code, w.Body.String())
	}
	// budget 非数字 → 400。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-bad", "budget_micros": "6k",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad budget = %d %s", w.Code, w.Body.String())
	}
	// expires_at 合法 RFC3339 → 通过；非法 → 400。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-exp", "expires_at": "2099-01-01T00:00:00Z",
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("rfc3339 expiry = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-exp2", "expires_at": "next friday",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad expiry = %d %s", w.Code, w.Body.String())
	}
	// 名称超长 → 400。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": strings.Repeat("n", 200),
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("long name = %d %s", w.Code, w.Body.String())
	}
	// 未知字段 → 400（strictBindJSON）。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-x", "secret_override": "yk-forged",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown field = %d %s", w.Code, w.Body.String())
	}
	// 负 rpm_limit → 400。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{
		"name": "k-rpm", "rpm_limit": -1,
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("negative rpm = %d %s", w.Code, w.Body.String())
	}

	// PATCH 的 parseMicrosJSON：裸整数便利形状放行；非数字字符串 400。
	w = f.do(t, http.MethodPost, "/user/api-keys", token, map[string]any{"name": "k-patch"})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("seed key: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Data.ID == "" {
		t.Fatalf("seed key decode: %s", w.Body.String())
	}
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+env.Data.ID, token, map[string]any{
		"budget_micros": 7000,
	})
	if w.Code != http.StatusOK {
		t.Errorf("patch bare-int budget = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+env.Data.ID, token, map[string]any{
		"budget_micros": "7k",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("patch bad budget = %d %s", w.Code, w.Body.String())
	}
}
