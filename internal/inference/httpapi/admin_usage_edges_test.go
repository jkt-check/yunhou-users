package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/management"
)

// admin_usage_edges_test.go — Task 16 覆盖率补强：运营统计端点的参数校
// 验分支（时间格式、kind、limit 钳制、预览 400）。主路径与权限门已在
// admin_bulk_test.go。

func TestAdminUsage_ParameterValidationEdges(t *testing.T) {
	f := newOpsFixture(t)
	op := uuid.NewString()
	f.grantOp(t, op, management.RoleAdmin)
	h := f.headers(op)

	// Summary：非法 from/to → 400。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?from=not-a-time", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?from=2026-01-01T00:00:00Z&to=nope", nil, http.StatusBadRequest, h)
	// from > to → 400。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", nil, http.StatusBadRequest, h)
	// 超 92 天 → 400。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z", nil, http.StatusBadRequest, h)
	// 合法 group_by 变体 → 200。
	for _, g := range []string{"model", "provider", "customer"} {
		doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?group_by="+g, nil, http.StatusOK, h)
	}
	// 非法 group_by → 400。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/summary?group_by=region", nil, http.StatusBadRequest, h)

	// Exceptions：非法 kind → 400；合法 kind + limit 钳制（>500 钳到 500）→ 200。
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/exceptions?kind=whatever", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/exceptions?kind=stuck_reservations&limit=9999", nil, http.StatusOK, h)
	doH(t, f.engine, http.MethodGet, "/adminu/model-usage/exceptions?kind=reauth_required_accounts", nil, http.StatusOK, h)

	// SharedAccounts：非法时间 → 400；limit 钳制 → 200。
	doH(t, f.engine, http.MethodGet, "/adminu/upstream-accounts/shared?from=nope", nil, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodGet, "/adminu/upstream-accounts/shared?limit=9999", nil, http.StatusOK, h)

	// Adjustments 列表（limit 钳制）→ 200。
	doH(t, f.engine, http.MethodGet, "/adminu/model-adjustments?limit=9999", nil, http.StatusOK, h)

	// 预览：缺字段 → 400。
	doH(t, f.engine, http.MethodPost, "/adminm/model-prices/preview", map[string]any{
		"model_id": "glm-4.6",
	}, http.StatusBadRequest, h)
	doH(t, f.engine, http.MethodPost, "/adminm/quota-policies/preview", map[string]any{}, http.StatusBadRequest, h)
}
