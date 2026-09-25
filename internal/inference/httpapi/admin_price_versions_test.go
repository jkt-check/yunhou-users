package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// admin_price_versions_test.go — 售价版本管理端点（POST/GET /price-versions，
// 挂 /adminm 即 models:manage 组）的 HTTP 级验收：真实库 + 真实
// OperatorAuthz 中间件（复用 admin_bulk_test.go 的 opsFixture）。覆盖
// spec 2026-09-25-admin-price-versions-design.md §6 负面矩阵：鉴权
// 401/403、逐项校验 400、未知模型 404、409 duplicate/conflict 重放、
// 同事务审计行断言、列表过滤/分页/空集形状。

// seedPVModel inserts one catalog model directly via the store.
func seedPVModel(t *testing.T, f *opsFixture, modelID string) {
	t.Helper()
	if err := f.store.InsertModel(context.Background(), &domain.Model{
		ID: modelID, DisplayName: modelID, ContextTokens: 1000, MaxOutputTokens: 100,
	}); err != nil {
		t.Fatalf("insert model: %v", err)
	}
}

func createPVBody(modelID string) map[string]any {
	return map[string]any{
		"model_id": modelID, "kind": "sale_credit",
		"input_micros_per_mtok": 1_000_000, "output_micros_per_mtok": 2_000_000,
		"revision": 1, "effective_from": "2026-09-25T08:00:00Z",
		"reason": "上架定价",
	}
}

func decodePVView(t *testing.T, env envelope) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("decode price version view: %v (%s)", err, env.Data)
	}
	return v
}

func TestAdminPriceVersionsCreate(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	env := doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		createPVBody("deepseek-chat"), http.StatusCreated, h)
	v := decodePVView(t, env)
	if v["price_version_id"] == "" || v["model_id"] != "deepseek-chat" || v["kind"] != "sale_credit" {
		t.Fatalf("view = %s", env.Data)
	}
	// unit 服务端派生；micros string 渲染；sale_credit 省略 currency；
	// effective_to 缺省省略；extra_rates 落默认 {"schema_version":1}。
	if v["unit"] != "microcredit" {
		t.Fatalf("unit = %v, want microcredit (server-derived)", v["unit"])
	}
	if v["input_micros_per_mtok"] != "1000000" || v["output_micros_per_mtok"] != "2000000" ||
		v["cache_read_micros_per_mtok"] != "0" || v["cache_write_micros_per_mtok"] != "0" {
		t.Fatalf("micros must render as strings: %s", env.Data)
	}
	if _, present := v["currency"]; present {
		t.Fatalf("currency must be omitted for sale_credit: %s", env.Data)
	}
	if _, present := v["effective_to"]; present {
		t.Fatalf("effective_to must be omitted when open-ended: %s", env.Data)
	}
	if v["revision"] != float64(1) || v["effective_from"] != "2026-09-25T08:00:00Z" || v["created_at"] == "" {
		t.Fatalf("view = %s", env.Data)
	}
	var extra map[string]any
	if err := json.Unmarshal(mustField(env.Data, "extra_rates"), &extra); err != nil || extra["schema_version"] != float64(1) {
		t.Fatalf("extra_rates = %s err=%v", mustField(env.Data, "extra_rates"), err)
	}

	// DB 行：unit/currency 与 CHECK 口径一致。
	var unit string
	var currency *string
	if err := f.db.QueryRow(
		`SELECT unit, currency FROM inference_price_versions WHERE id = $1`,
		v["price_version_id"]).Scan(&unit, &currency); err != nil {
		t.Fatal(err)
	}
	if unit != "microcredit" || currency != nil {
		t.Fatalf("db row unit=%s currency=%v", unit, currency)
	}

	// 同事务审计：price_version.create 带人员 + 服务双重归因。
	id, _ := v["price_version_id"].(string)
	if n := auditCount(t, f, "price_version.create", id); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	var actorUser, actorApp, objectType, reason string
	var detail []byte
	if err := f.db.QueryRow(
		`SELECT actor_user_id::text, actor_app_id, object_type, reason, detail
		   FROM inference_audit_log WHERE action = 'price_version.create' AND object_id = $1`, id).
		Scan(&actorUser, &actorApp, &objectType, &reason, &detail); err != nil {
		t.Fatalf("audit missing: %v", err)
	}
	if actorUser != op || actorApp != "ops-console" || objectType != "price_version" || reason != "上架定价" {
		t.Fatalf("audit = %s/%s/%s/%s", actorUser, actorApp, objectType, reason)
	}
	var d map[string]any
	if err := json.Unmarshal(detail, &d); err != nil || d["revision"] != float64(1) || d["kind"] != "sale_credit" {
		t.Fatalf("audit detail = %s err=%v", detail, err)
	}
}

// TestAdminPriceVersionsCreateMoneyKind: sale_money 带币种 → unit=micromoney，
// currency 出现在视图里；effective_from 缺省 = 服务器当前时刻。
func TestAdminPriceVersionsCreateMoneyKind(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "glm-4.6")

	body := map[string]any{
		"model_id": "glm-4.6", "kind": "sale_money", "currency": "USD",
		"input_micros_per_mtok": 250, "output_micros_per_mtok": 1000,
		"revision": 1, "reason": "PAYG 价目",
	}
	env := doH(t, f.engine, http.MethodPost, "/adminm/price-versions", body, http.StatusCreated, h)
	v := decodePVView(t, env)
	if v["unit"] != "micromoney" || v["currency"] != "USD" {
		t.Fatalf("money view = %s", env.Data)
	}
	if v["effective_from"] == "" || v["created_at"] == "" {
		t.Fatalf("server-defaulted effective_from missing: %s", env.Data)
	}
}

// TestAdminPriceVersionsIdempotent: (model_id, kind, revision) 自然键幂等——
// 同内容重放 409 duplicate + 已存在视图（不建行、不追加审计）；同 revision
// 不同内容 409 conflict + 已存在视图。
func TestAdminPriceVersionsIdempotent(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	first := decodePVView(t, doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		createPVBody("deepseek-chat"), http.StatusCreated, h))

	// 同内容重放 → 409 duplicate，data = 已存在视图。
	env := doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		createPVBody("deepseek-chat"), http.StatusConflict, h)
	if !strings.Contains(env.Message, "duplicate") {
		t.Fatalf("replay message = %q, want duplicate marker", env.Message)
	}
	dup := decodePVView(t, env)
	if dup["price_version_id"] != first["price_version_id"] {
		t.Fatalf("409 view id = %v, want existing %v", dup["price_version_id"], first["price_version_id"])
	}

	// 同 revision 不同费率 → 409 conflict，data 仍带已存在视图。
	conflicting := createPVBody("deepseek-chat")
	conflicting["output_micros_per_mtok"] = 9_000_000
	env = doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		conflicting, http.StatusConflict, h)
	if !strings.Contains(env.Message, "conflict") {
		t.Fatalf("conflict message = %q, want conflict marker", env.Message)
	}
	conf := decodePVView(t, env)
	if conf["price_version_id"] != first["price_version_id"] || conf["output_micros_per_mtok"] != "2000000" {
		t.Fatalf("conflict view = %s, want the STORED revision", env.Data)
	}

	// 不建行、不追加审计。
	if n := tableCount(t, f, "inference_price_versions"); n != 1 {
		t.Fatalf("price version rows = %d, want 1", n)
	}
	id, _ := first["price_version_id"].(string)
	if n := auditCount(t, f, "price_version.create", id); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (no audit on 409)", n)
	}

	// 下一 revision 正常追加（纠正错误的方式 = 发布新 revision）。
	next := createPVBody("deepseek-chat")
	next["revision"] = 2
	next["output_micros_per_mtok"] = 3_000_000
	doH(t, f.engine, http.MethodPost, "/adminm/price-versions", next, http.StatusCreated, h)
	if n := tableCount(t, f, "inference_price_versions"); n != 2 {
		t.Fatalf("price version rows = %d, want 2", n)
	}
}

// TestAdminPriceVersionsCreateNegatives: spec §6 负面矩阵的 4xx 逐项。
func TestAdminPriceVersionsCreateNegatives(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)
	seedPVModel(t, f, "deepseek-chat")

	post := func(body map[string]any, want int) envelope {
		return doH(t, f.engine, http.MethodPost, "/adminm/price-versions", body, want, h)
	}

	t.Run("unknown field -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["unit"] = "micromoney" // unit 不入请求（服务端派生）
		post(body, http.StatusBadRequest)
	})
	t.Run("missing model_id -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		delete(body, "model_id")
		post(body, http.StatusBadRequest)
	})
	t.Run("missing kind -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		delete(body, "kind")
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "unknown kind") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("invalid kind -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["kind"] = "retail"
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "unknown kind") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("unknown model -> 404", func(t *testing.T) {
		env := post(createPVBody("no-such-model"), http.StatusNotFound)
		if !strings.Contains(env.Message, "model not found") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("sale_credit with currency -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["currency"] = "USD"
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "currency must be empty for sale_credit") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("sale_money without currency -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["kind"] = "sale_money"
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "currency") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("lowercase currency -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["kind"] = "upstream_cost"
		body["currency"] = "usd"
		post(body, http.StatusBadRequest)
	})
	t.Run("negative rate -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["cache_read_micros_per_mtok"] = -1
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "rates must be >= 0") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("missing revision -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		delete(body, "revision")
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "revision must be > 0") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("negative revision -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["revision"] = -1
		post(body, http.StatusBadRequest)
	})
	t.Run("effective_to before from -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["effective_to"] = "2026-09-25T08:00:00Z" // == effective_from
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "effective_to must be after effective_from") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("extra_rates not an object -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		body["extra_rates"] = []int{1, 2}
		env := post(body, http.StatusBadRequest)
		if !strings.Contains(env.Message, "invalid extra_rates") {
			t.Fatalf("message = %q", env.Message)
		}
	})
	t.Run("missing reason -> 400", func(t *testing.T) {
		body := createPVBody("deepseek-chat")
		delete(body, "reason")
		post(body, http.StatusBadRequest)
	})

	// 尾随垃圾同样 400（strictBindJSON 单值后必须 EOF）。
	req := httptest.NewRequest(http.MethodPost, "/adminm/price-versions",
		bytes.NewReader([]byte(`{"model_id":"deepseek-chat"} garbage`)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("trailing garbage: status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// TestAdminPriceVersionsAuthChain: 鉴权链（401 缺身份 / 403 auditor / 403
// 无角色）；GET 与 POST 同组（models:manage）。
func TestAdminPriceVersionsAuthChain(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	seedPVModel(t, f, "deepseek-chat")

	// 无身份双腿 → 401。
	doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		createPVBody("deepseek-chat"), http.StatusUnauthorized, nil)
	doH(t, f.engine, http.MethodGet, "/adminm/price-versions", nil, http.StatusUnauthorized, nil)

	// auditor 无 models:manage → 403（写与读同拒）。
	auditor := uuid.NewString()
	f.grantOp(t, auditor, management.RoleAuditor)
	doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		createPVBody("deepseek-chat"), http.StatusForbidden, f.headers(auditor))
	doH(t, f.engine, http.MethodGet, "/adminm/price-versions", nil, http.StatusForbidden, f.headers(auditor))

	// 非运营用户（无角色）→ 403。
	plain := uuid.NewString()
	f.grantOp(t, plain)
	doH(t, f.engine, http.MethodPost, "/adminm/price-versions",
		createPVBody("deepseek-chat"), http.StatusForbidden, f.headers(plain))
}

// TestAdminPriceVersionsList: 过滤/排序/分页/钳制与空集形状（items 恒 []
// 非 null）。
func TestAdminPriceVersionsList(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleOperator)
	h := f.headers(op)

	// 空集：items 是 [] 而非 null，limit/offset 回显。
	env := doH(t, f.engine, http.MethodGet, "/adminm/price-versions", nil, http.StatusOK, h)
	var page struct {
		Items  []json.RawMessage `json:"items"`
		Limit  int               `json:"limit"`
		Offset int               `json:"offset"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.Limit != 100 || page.Offset != 0 {
		t.Fatalf("empty page = %s, want items [] limit 100 offset 0", env.Data)
	}

	seedPVModel(t, f, "m-a")
	seedPVModel(t, f, "m-b")
	create := func(modelID, kind string, revision int, currency string) {
		body := createPVBody(modelID)
		body["kind"] = kind
		body["revision"] = revision
		if currency != "" {
			body["currency"] = currency
		}
		doH(t, f.engine, http.MethodPost, "/adminm/price-versions", body, http.StatusCreated, h)
	}
	create("m-a", "sale_credit", 1, "")
	create("m-a", "sale_credit", 2, "")
	create("m-a", "upstream_cost", 1, "USD")
	create("m-b", "sale_money", 1, "CNY")

	// 排序 (model_id, kind, revision DESC)。
	env = doH(t, f.engine, http.MethodGet, "/adminm/price-versions", nil, http.StatusOK, h)
	var listed struct {
		Items []struct {
			ModelID  string `json:"model_id"`
			Kind     string `json:"kind"`
			Revision int    `json:"revision"`
		} `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &listed); err != nil || len(listed.Items) != 4 {
		t.Fatalf("list = %s err=%v", env.Data, err)
	}
	want := [][3]any{
		{"m-a", "sale_credit", 2}, {"m-a", "sale_credit", 1},
		{"m-a", "upstream_cost", 1}, {"m-b", "sale_money", 1},
	}
	for i, w := range want {
		it := listed.Items[i]
		if it.ModelID != w[0] || it.Kind != w[1] || it.Revision != w[2] {
			t.Fatalf("order[%d] = %s/%s/%d, want %v", i, it.ModelID, it.Kind, it.Revision, w)
		}
	}

	// 过滤：model_id / kind / 两者组合。
	env = doH(t, f.engine, http.MethodGet, "/adminm/price-versions?model_id=m-a", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &listed); err != nil || len(listed.Items) != 3 {
		t.Fatalf("model filter = %s err=%v", env.Data, err)
	}
	env = doH(t, f.engine, http.MethodGet, "/adminm/price-versions?kind=sale_credit", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &listed); err != nil || len(listed.Items) != 2 {
		t.Fatalf("kind filter = %s err=%v", env.Data, err)
	}
	env = doH(t, f.engine, http.MethodGet, "/adminm/price-versions?model_id=m-a&kind=upstream_cost", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &listed); err != nil || len(listed.Items) != 1 {
		t.Fatalf("combined filter = %s err=%v", env.Data, err)
	}

	// 分页：limit=1&offset=1 取第二项（m-a/sale_credit rev1）；回显分页参数。
	env = doH(t, f.engine, http.MethodGet, "/adminm/price-versions?limit=1&offset=1", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &page); err != nil || len(page.Items) != 1 ||
		page.Limit != 1 || page.Offset != 1 {
		t.Fatalf("paged = %s err=%v", env.Data, err)
	}
	var item map[string]any
	if err := json.Unmarshal(page.Items[0], &item); err != nil || item["revision"] != float64(1) {
		t.Fatalf("paged item = %s", page.Items[0])
	}

	// limit 超上限钳到 500（不报错）。
	env = doH(t, f.engine, http.MethodGet, "/adminm/price-versions?limit=9999", nil, http.StatusOK, h)
	if err := json.Unmarshal(env.Data, &page); err != nil || page.Limit != 500 {
		t.Fatalf("clamped limit = %s err=%v", env.Data, err)
	}

	// 非法查询：kind 非法 / offset < 0 / offset 非数字 → 400。
	doH(t, f.engine, http.MethodGet, "/adminm/price-versions?kind=retail", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminm/price-versions?offset=-1", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminm/price-versions?offset=abc", nil, http.StatusBadRequest, h)
}
