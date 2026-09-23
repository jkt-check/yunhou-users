package httpapi_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openapi_contract_test.go — docs/api/kaya-coding-plan.openapi.yaml 与
// handler 实际响应的对齐断言（轻量文本级；fixture 级逐字段对齐由
// user_views_fixture_test.go 承担）。钉住：每个已挂载端点都在文档中、
// 七个 fixture 都被引用、关键 DTO 字段/枚举/约定不漂移。

func readOpenAPI(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "kaya-coding-plan.openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi file missing: %v", err)
	}
	return string(raw)
}

func TestOpenAPI_CoversMountedEndpoints(t *testing.T) {
	doc := readOpenAPI(t)
	for _, path := range []string{
		"/user/model-quotas:",
		"/user/model-usage/summary:",
		"/user/model-usage/requests:",
		"/user/model-subscriptions:",
		"/user/api-keys:",
		"/user/api-keys/{id}:",
		// Task 13: 编程工具协议面(原生形状)必须留档。
		"/v1/messages:",
		"/v1/responses:",
		// Task 14: 钱包/套餐外/PAYG 面。
		"/user/wallet:",
		"/user/wallet/entries:",
		"/user/wallet/overage:",
		"/user/wallet/payg:",
		"/admin/wallet/adjustments:",
		"/admin/wallet/reversals:",
		"/admin/wallet:",
		"/admin/payg-config:",
		// Task 15: 运营面（批量导入/统计/异常/共享账号检测/补偿追踪/变更预览）。
		"/admin/catalog/bulk-import:",
		"/admin/model-usage/summary:",
		"/admin/model-usage/exceptions:",
		"/admin/upstream-accounts/shared:",
		"/admin/model-adjustments:",
		"/admin/model-prices/preview:",
		"/admin/quota-policies/preview:",
	} {
		if !strings.Contains(doc, path) {
			t.Errorf("openapi missing path %s", path)
		}
	}
	// 七个响应 fixture 全部被引用。
	for _, fx := range []string{
		"model-quotas-zero-limit.json",
		"model-quotas-unactivated.json",
		"model-quotas-exhausted.json",
		"model-quotas-expired.json",
		"model-quotas-reserved.json",
		"model-quotas-cross-month.json",
		"model-usage-requests-reconciliation.json",
	} {
		if !strings.Contains(doc, "fixtures/"+fx) {
			t.Errorf("openapi missing fixture reference %s", fx)
		}
	}
	// 协议面能力矩阵关键词(Task 13):不得夸大兼容。
	for _, kw := range []string{
		"2023-06-01", "previous_response_id", "previous_response_not_found",
		"message_start", "message_stop", "response.completed", "response.incomplete",
		"invalid_request_error", "overloaded_error", "ApiKeyAuth",
		"tool_use", "tool_result", "function_call_output", "item_reference",
		"cache_read_input_tokens", "budget_tokens", "reasoning",
	} {
		if !strings.Contains(doc, kw) {
			t.Errorf("openapi missing protocol keyword %q", kw)
		}
	}
}

func TestOpenAPI_ConventionsAndDTOFields(t *testing.T) {
	doc := readOpenAPI(t)
	// 约定：十进制整数字符串、UTC、计量完整性、未激活提示、keyset 游标。
	for _, kw := range []string{
		"microcredit", "DecimalInt64", "date-time",
		"on_first_consumption", "anchored_duration", "anchored_period_7d", "anchored_calendar_month",
		"reported", "estimated", "unknown", "pending",
		"next_cursor", "complete_through", "as_of", "server_time",
		"quota_exhausted", "entitlement_expired", "no_active_entitlement",
		"bundle_gift", "migration_gift", "kaya_membership",
		"usage_events", // 心跳表不作为用量来源的明确口径
		"不补零行",       // Task 16 minor ③：运营统计零活动分组缺席的明确声明
		// Task 14 钱包口径：账本派生、现金/赠送来源隔离、套餐外默认关、PAYG 显式权益。
		"micromoney", "cash", "bonus", "overage_enabled", "monthly_spend_limit_micros",
		"topup", "consume", "refund", "reversal", "payg",
	} {
		if !strings.Contains(doc, kw) {
			t.Errorf("openapi missing convention keyword %q", kw)
		}
	}
	// DTO 字段（与 handler 输出同源）：逐字段在文档中出现。
	for _, field := range []string{
		"window_start", "resets_at", "remaining", "blocked_by", "entitlement_id",
		"charge_micros", "reversed_micros", "net_micros", "reserved_micros",
		"usage_status", "reconciliation_pending", "in_flight",
		"policy_version_id", "effective_from", "effective_to", "anchor_at",
		"budget_used_micros", "key_prefix",
	} {
		if !strings.Contains(doc, field) {
			t.Errorf("openapi missing DTO field %q", field)
		}
	}
	// 整数精度约定：DecimalInt64 必须是 string 类型而非 integer。
	i := strings.Index(doc, "DecimalInt64:")
	j := strings.Index(doc[i:], "type: string")
	if i < 0 || j < 0 || j > 120 {
		t.Error("DecimalInt64 must be declared as type: string (十进制整数字符串)")
	}
}
