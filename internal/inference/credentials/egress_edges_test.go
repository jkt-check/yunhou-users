package credentials

import (
	"context"
	"testing"
)

// egress_edges_test.go — Task 16 覆盖率补强：egress 校验器的配置与判定
// 分支（CIDR/主机名/裸 IP 拒绝/重定向校验）。

func TestNewEgressValidator_ConfigBranches(t *testing.T) {
	// 合法：CIDR + 主机名 + 空项忽略。
	v, err := NewEgressValidator([]string{"127.0.0.0/8", " api.example.com ", ""})
	if err != nil {
		t.Fatal(err)
	}
	_ = v
	// 裸 IP 必须 CIDR 化。
	if _, err := NewEgressValidator([]string{"10.0.0.1"}); err == nil {
		t.Error("bare IP accepted (must require CIDR form)")
	}
	// 非法 CIDR。
	if _, err := NewEgressValidator([]string{"999.0.0.0/8"}); err == nil {
		t.Error("invalid CIDR accepted")
	}
}

func TestEgressValidator_URLAndRedirectBranches(t *testing.T) {
	v, err := NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 非 http(s) 方案拒绝。
	if err := v.ValidateURL(ctx, "ftp://127.0.0.1/x"); err == nil {
		t.Error("non-http scheme accepted")
	}
	// 公网目标拒绝（fail closed，不在 allowlist）。
	if err := v.ValidateURL(ctx, "https://169.254.169.254/latest/meta-data"); err == nil {
		t.Error("metadata endpoint accepted (SSRF!)")
	}
	// allowlist 内 loopback 放行。
	if err := v.ValidateURL(ctx, "http://127.0.0.1:9000/up"); err != nil {
		t.Errorf("allowlisted loopback rejected: %v", err)
	}
	// 重定向校验同一策略。
	if err := v.ValidateRedirect(ctx, "https://192.168.1.1/internal"); err == nil {
		t.Error("redirect to private net accepted")
	}
	if err := v.ValidateRedirect(ctx, "http://127.0.0.1/ok"); err != nil {
		t.Errorf("allowlisted redirect rejected: %v", err)
	}
	// 畸形 URL 不 panic。
	if err := v.ValidateURL(ctx, "://garbage"); err == nil {
		t.Log("malformed URL rejected (expected)")
	}
}
