package httpapi_test

// user_api_keys_concurrency_test.go — Task 5：Key 级并发上限（concurrency_limit）
// 的客户自管面契约。字段语义与 rpm_limit 同构：缺省不加限制、PATCH 缺席
// 不修改、显式 null 清除、非正整数拒绝；越权矩阵与其它 Key 字段一致。
//
// 本文件只用既有 HTTP 契约（不引用任何新的 Go 符号），因此可以直接在未
// 修改的生产代码上跑出行为级 RED。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// keyConcurrencyLimit reads the persisted Key concurrency limit (NULL = no
// Key-level limit).
func keyConcurrencyLimit(t *testing.T, f *accessFixture, keyID string) sql.NullInt64 {
	t.Helper()
	var limit sql.NullInt64
	if err := f.db.QueryRow(`SELECT concurrency_limit FROM inference_api_keys WHERE id = $1`, keyID).
		Scan(&limit); err != nil {
		t.Fatalf("read key concurrency limit: %v", err)
	}
	return limit
}

// decodeKeyData decodes the {code,data,message} envelope's data object.
func decodeKeyData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env.Data
}

// TestUserAPIKeys_ConcurrencyLimitCreateReadUpdate: concurrency_limit 的
// 创建/读取/更新三处语义。
func TestUserAPIKeys_ConcurrencyLimitCreateReadUpdate(t *testing.T) {
	f := newAccessFixture(t)
	_, tokA := f.addUser(t)

	// 缺省：创建不带该字段 → 没有 Key 级并发限制（响应为 null，行内为 NULL）。
	plain := f.createKey(t, tokA, map[string]any{"name": "plain"})
	if v, ok := plain["concurrency_limit"]; !ok || v != nil {
		t.Errorf("concurrency_limit = %v (present=%v), want null (缺省不加 Key 限制)", v, ok)
	}
	plainID := plain["id"].(string)
	if got := keyConcurrencyLimit(t, f, plainID); got.Valid {
		t.Errorf("stored concurrency_limit = %v, want NULL (缺省不加 Key 限制)", got)
	}

	// 显式设置：创建响应、详情、列表都带该值。
	capped := f.createKey(t, tokA, map[string]any{"name": "capped", "concurrency_limit": 2})
	if capped["concurrency_limit"] != float64(2) {
		t.Errorf("concurrency_limit = %v, want 2", capped["concurrency_limit"])
	}
	keyID := capped["id"].(string)
	if got := keyConcurrencyLimit(t, f, keyID); !got.Valid || got.Int64 != 2 {
		t.Errorf("stored concurrency_limit = %v, want 2", got)
	}
	w := f.do(t, http.MethodGet, "/user/api-keys/"+keyID, tokA, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if got := decodeKeyData(t, w.Body.Bytes())["concurrency_limit"]; got != float64(2) {
		t.Errorf("detail concurrency_limit = %v, want 2", got)
	}
	w = f.do(t, http.MethodGet, "/user/api-keys", tokA, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var listEnv struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listEnv); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, item := range listEnv.Data.Items {
		if item["id"] == keyID {
			found = true
			if item["concurrency_limit"] != float64(2) {
				t.Errorf("list concurrency_limit = %v, want 2", item["concurrency_limit"])
			}
		}
	}
	if !found {
		t.Error("the key is missing from the list")
	}

	// PATCH 缺席该字段：不修改现值。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokA, map[string]any{"name": "renamed"})
	if w.Code != http.StatusOK {
		t.Fatalf("patch without the field: %d %s", w.Code, w.Body.String())
	}
	if got := decodeKeyData(t, w.Body.Bytes())["concurrency_limit"]; got != float64(2) {
		t.Errorf("concurrency_limit after an unrelated patch = %v, want 2 (PATCH 缺席不修改)", got)
	}
	if got := keyConcurrencyLimit(t, f, keyID); !got.Valid || got.Int64 != 2 {
		t.Errorf("stored concurrency_limit after an unrelated patch = %v, want 2", got)
	}

	// PATCH 显式设置。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokA, map[string]any{"concurrency_limit": 5})
	if w.Code != http.StatusOK {
		t.Fatalf("patch the limit: %d %s", w.Code, w.Body.String())
	}
	if got := decodeKeyData(t, w.Body.Bytes())["concurrency_limit"]; got != float64(5) {
		t.Errorf("concurrency_limit = %v, want 5", got)
	}
	if got := keyConcurrencyLimit(t, f, keyID); !got.Valid || got.Int64 != 5 {
		t.Errorf("stored concurrency_limit = %v, want 5", got)
	}

	// PATCH 显式 null：清除该限制。
	w = f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokA, map[string]any{"concurrency_limit": nil})
	if w.Code != http.StatusOK {
		t.Fatalf("patch null: %d %s", w.Code, w.Body.String())
	}
	if got := decodeKeyData(t, w.Body.Bytes())["concurrency_limit"]; got != nil {
		t.Errorf("concurrency_limit after an explicit null = %v, want null (显式清除)", got)
	}
	if got := keyConcurrencyLimit(t, f, keyID); got.Valid {
		t.Errorf("stored concurrency_limit after an explicit null = %v, want NULL", got)
	}
}

// TestUserAPIKeys_ConcurrencyLimitRejectsNonPositive: 非正整数一律拒绝
// （创建与更新同构），且被拒的写入不改变现值。
func TestUserAPIKeys_ConcurrencyLimitRejectsNonPositive(t *testing.T) {
	f := newAccessFixture(t)
	_, tokA := f.addUser(t)

	for _, bad := range []any{0, -1} {
		w := f.do(t, http.MethodPost, "/user/api-keys", tokA, map[string]any{"concurrency_limit": bad})
		if w.Code != http.StatusBadRequest {
			t.Errorf("create with concurrency_limit=%v: got %d, want 400 (%s)", bad, w.Code, w.Body.String())
		}
	}

	created := f.createKey(t, tokA, map[string]any{"concurrency_limit": 3})
	keyID := created["id"].(string)
	for _, bad := range []any{0, -1} {
		w := f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokA, map[string]any{"concurrency_limit": bad})
		if w.Code != http.StatusBadRequest {
			t.Errorf("patch with concurrency_limit=%v: got %d, want 400 (%s)", bad, w.Code, w.Body.String())
		}
		if got := keyConcurrencyLimit(t, f, keyID); !got.Valid || got.Int64 != 3 {
			t.Errorf("stored concurrency_limit after a rejected patch = %v, want 3 (被拒的写入不改现值)", got)
		}
	}
}

// TestUserAPIKeys_ConcurrencyLimitKeepsOwnerChecks: 越权矩阵与其它 Key
// 字段一致 —— 别人的 Key 一律 404（与不存在无差别），不得改写其限制。
func TestUserAPIKeys_ConcurrencyLimitKeepsOwnerChecks(t *testing.T) {
	f := newAccessFixture(t)
	_, tokA := f.addUser(t)
	_, tokB := f.addUser(t)

	created := f.createKey(t, tokA, map[string]any{"concurrency_limit": 4})
	keyID := created["id"].(string)

	w := f.do(t, http.MethodPatch, "/user/api-keys/"+keyID, tokB, map[string]any{"concurrency_limit": 99})
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-customer patch: got %d, want 404 (%s)", w.Code, w.Body.String())
	}
	if got := keyConcurrencyLimit(t, f, keyID); !got.Valid || got.Int64 != 4 {
		t.Errorf("stored concurrency_limit after a cross-customer patch = %v, want 4", got)
	}
	// 不存在的 Key 与别人的 Key 答复无差别（不泄漏存在性）。
	missing := uuid.NewString()
	wMissing := f.do(t, http.MethodPatch, "/user/api-keys/"+missing, tokB, map[string]any{"concurrency_limit": 99})
	if wMissing.Code != w.Code || wMissing.Body.String() != w.Body.String() {
		t.Errorf("foreign-id and missing-id responses differ:\nforeign: %d %s\nmissing: %d %s",
			w.Code, w.Body.String(), wMissing.Code, wMissing.Body.String())
	}
	// 未认证的管理调用仍然 401。
	if w := f.do(t, http.MethodPost, "/user/api-keys", "", map[string]any{"concurrency_limit": 1}); w.Code != http.StatusUnauthorized {
		t.Errorf("no-JWT create: got %d, want 401", w.Code)
	}
}
