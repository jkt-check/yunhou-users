package domain

import (
	"errors"
	"strings"
	"testing"
)

// domain_edges_test.go — Task 16 覆盖率补强：自定义头白名单/黑名单、
// 规范头键、错误码与包装、金额解析的纯规则边界。

func TestValidateCustomHeaders_Boundary(t *testing.T) {
	// 黑名单（认证/追踪边界）逐一拒绝。
	for _, bad := range []map[string]string{
		{"Authorization": "Bearer x"},
		{"X-Api-Key": "k"},
		{"X-Request-Id": "r"},
		{"Proxy-Authorization": "p"},
		{"Host": "h"},
	} {
		if err := ValidateCustomHeaders(bad); err == nil {
			t.Errorf("blacklisted header accepted: %v", bad)
		}
	}
	// 大小写规避必须被规范化命中（canonical header key）。
	if err := ValidateCustomHeaders(map[string]string{"authorization": "Bearer x"}); err == nil {
		t.Error("lowercase authorization bypassed the blacklist")
	}
	// 黑名单之外的形状不属本函数职责（畸形头由部署写路径另行约束）：
	// 良性头与非常规但非保留头放行。
	if err := ValidateCustomHeaders(map[string]string{
		"X-Team": "ops", "X-Env": "prod", "Accept-Language": "zh-CN",
	}); err != nil {
		t.Errorf("benign headers rejected: %v", err)
	}
	// nil 安全。
	if err := ValidateCustomHeaders(nil); err != nil {
		t.Errorf("nil headers: %v", err)
	}
}

func TestExtensionHeaders_RoundTrip(t *testing.T) {
	// 存取往返 + 非 map 形状容错。
	h, err := ExtensionHeaders(ExtensionConfig{})
	if err != nil || len(h) != 0 {
		t.Errorf("empty config headers = %v/%v", h, err)
	}
	cfg := ExtensionConfig{SchemaVersion: 1, Raw: []byte(`{"schema_version":1,"headers":{"X-Team":"ops"}}`)}
	h, err = ExtensionHeaders(cfg)
	if err != nil || h["X-Team"] != "ops" {
		t.Errorf("headers = %v/%v", h, err)
	}
	bad := ExtensionConfig{SchemaVersion: 1, Raw: []byte(`{"schema_version":1,"headers":"nope"}`)}
	if _, err := ExtensionHeaders(bad); err == nil {
		t.Error("malformed headers must error")
	}
}

func TestErrorWrapping_CodesAndMessages(t *testing.T) {
	e := NewError(CodeInvalidInput, "bad input")
	if CodeOf(e) != CodeInvalidInput {
		t.Errorf("code = %s", CodeOf(e))
	}
	if e.Error() == "" {
		t.Error("empty error message")
	}
	w := WrapError(CodeConflict, "outer", e)
	if CodeOf(w) != CodeConflict {
		t.Errorf("wrapped code = %s", CodeOf(w))
	}
	if !strings.Contains(w.Error(), "outer") || !strings.Contains(w.Error(), "bad input") {
		t.Errorf("wrapped message = %q", w.Error())
	}
	// 非 domain 错误 → CodeInternal。
	if CodeOf(errPlain) != CodeInternal {
		t.Errorf("plain error code = %s", CodeOf(errPlain))
	}
	// Unwrap 链。
	if w.Cause == nil || !errors.Is(w, e) {
		t.Error("wrapped error lost the cause")
	}
}

type plainErr struct{}

func (plainErr) Error() string { return "plain" }

var errPlain = plainErr{}

func TestMoney_ParseAndConvert(t *testing.T) {
	m, err := NewMoney(1234, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if m.Currency != "USD" {
		t.Errorf("currency = %q", m.Currency)
	}
	// 负金额拒绝。
	if _, err := NewMoney(-1, "USD"); err == nil {
		t.Error("negative money accepted")
	}
	// 小写币种拒绝（ISO-4217 大写）。
	if _, err := NewMoney(1, "usd"); err == nil {
		t.Error("lowercase currency accepted")
	}
}
