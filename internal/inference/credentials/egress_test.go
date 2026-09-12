package credentials

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func resolverOf(ips ...string) func(context.Context, string) ([]netip.Addr, error) {
	addrs := make([]netip.Addr, 0, len(ips))
	for _, s := range ips {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	return func(context.Context, string) ([]netip.Addr, error) {
		return addrs, nil
	}
}

func newTestValidator(t *testing.T, allowlist []string, ips ...string) *EgressValidator {
	t.Helper()
	v, err := NewEgressValidator(allowlist)
	if err != nil {
		t.Fatal(err)
	}
	v.resolve = resolverOf(ips...)
	return v
}

func TestValidateURLBlocksMetadataAndInternal(t *testing.T) {
	v := newTestValidator(t, nil, "93.184.216.34")
	blocked := []string{
		"http://169.254.169.254/latest/meta-data", // cloud metadata (link-local)
		"http://127.0.0.1:8080/v1",
		"http://[::1]/v1",
		"http://0.0.0.0/",
		"http://10.0.0.5/v1",        // RFC1918
		"http://192.168.1.1/v1",     // RFC1918
		"http://172.16.0.1/v1",      // RFC1918
		"http://[fd00::1]/v1",       // ULA
		"http://[fe80::1]/v1",       // link-local v6
		"http://[::ffff:127.0.0.1]/v1", // v4-mapped loopback
		// 评审轮1 I3：非全局特殊用途段。
		"http://100.100.100.200/latest/meta-data", // 阿里云元数据（CGNAT 100.64/10）
		"http://100.64.0.1/v1",        // CGNAT shared space
		"http://192.0.0.1/v1",         // IETF protocol assignments
		"http://192.0.2.1/v1",         // TEST-NET-1
		"http://198.18.0.1/v1",        // benchmarking
		"http://198.51.100.1/v1",      // TEST-NET-2
		"http://203.0.113.1/v1",       // TEST-NET-3
		"http://240.0.0.1/v1",         // reserved
		"http://255.255.255.255/v1",   // limited broadcast
		"http://[64:ff9b::a9fe:a9fe]/v1", // NAT64 → 169.254.169.254（映射后判定）
		"http://[64:ff9b::7f00:1]/v1",    // NAT64 → 127.0.0.1
		"http://[64:ff9b:1::1]/v1",       // local-use NAT64
		"http://[100::1]/v1",             // discard-only
		"http://[2001:db8::1]/v1",        // documentation v6
		"http://[3fff::1]/v1",            // documentation v6
		// 评审轮2 S-3：残余漏网段。
		"http://0.0.0.1/v1",           // 0.0.0.0/8 整段（非只 0.0.0.0）
		"http://0.255.255.255/v1",     // 0.0.0.0/8 整段
		"http://192.88.99.1/v1",       // 6to4 relay anycast
		"http://[2001:2::1]/v1",       // benchmarking v6
		"http://[2001:10::1]/v1",      // ORCHIDv1
		"http://[2001:1::1]/v1",       // PCP anycast
		"http://[2001:1::2]/v1",       // TURN anycast
		// 评审轮2 S-1：带 zone 的地址不得绕过前缀/NAT64 判定。
		"http://[64:ff9b::a9fe:a9fe%25eth0]/v1", // NAT64→metadata，带 zone
		"http://[2001:db8::1%25eth0]/v1",        // 文档段，带 zone
		"http://[fe80::1%25eth0]/v1",            // link-local，带 zone
		"http://[100::1%25eth0]/v1",             // discard-only，带 zone
		"ftp://93.184.216.34/v1",    // scheme
		"http://user:pass@93.184.216.34/v1", // userinfo
		"/relative/path",            // not absolute
		"http://",                   // no host
	}
	for _, u := range blocked {
		if err := v.ValidateURL(context.Background(), u); err == nil {
			t.Errorf("ValidateURL(%q) must be rejected", u)
		}
	}
	// NAT64 映射到公网地址则按公网放行（映射后判定语义对称）。
	if err := v.ValidateURL(context.Background(), "http://[64:ff9b::5db8:d822]/v1"); err != nil {
		t.Errorf("NAT64 of a public address (93.184.216.34) must be allowed: %v", err)
	}
	// 带 zone 的公网地址同样放行（zone 只影响策略判定的剥离，不构成拒绝理由）。
	if err := v.ValidateURL(context.Background(), "http://[2606:4700:4700::1111%25eth0]/v1"); err != nil {
		t.Errorf("public address with a zone must be allowed: %v", err)
	}
}

// 评审轮1 I4：拨号期 DNS 重绑定——ValidateURL 解析返公网、DialContext 再
// 解析返元数据地址时必须拒拨（策略绑定实际拨号 IP，不是陈旧的校验结果）。
func TestDialContextRejectsDNSRebinding(t *testing.T) {
	v, err := NewEgressValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	v.resolve = func(context.Context, string) ([]netip.Addr, error) {
		calls++
		if calls == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil // 校验期公网
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil // 拨号期重绑定
	}
	if err := v.ValidateURL(context.Background(), "https://rebind.example.com/v1"); err != nil {
		t.Fatalf("validate-time public answer must pass: %v", err)
	}
	if _, err := v.DialContext(context.Background(), "tcp", "rebind.example.com:443"); err == nil {
		t.Fatal("dial-time rebind to the metadata address must be rejected")
	}
	if calls < 2 {
		t.Fatalf("DialContext must re-resolve at dial time (calls = %d)", calls)
	}
}

// 拨号期混合答案：被拦候选跳过，只拨通过校验的 IP（allowlist 覆盖语义不
// 变——内网地址显式放行后可拨通）。
func TestDialContextDialsOnlyValidatedIPs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	v, err := NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	v.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("169.254.169.254"), // 被拦候选
			netip.MustParseAddr("127.0.0.1"),       // allowlist 放行
		}, nil
	}
	conn, err := v.DialContext(context.Background(), "tcp", net.JoinHostPort("mixed.example.com", port))
	if err != nil {
		t.Fatalf("allowlisted loopback candidate must dial: %v", err)
	}
	conn.Close()

	// 全部被拦 → 报错（fail closed）。
	v2, err := NewEgressValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	v2.resolve = resolverOf("169.254.169.254")
	if _, err := v2.DialContext(context.Background(), "tcp", "blocked.example.com:443"); err == nil {
		t.Fatal("all-blocked candidate set must fail closed")
	}
}

func TestValidateURLUnresolvableHostRejected(t *testing.T) {
	v, err := NewEgressValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	v.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no such host")
	}
	if err := v.ValidateURL(context.Background(), "https://does-not-exist.invalid/v1"); err == nil {
		t.Fatal("unresolvable host must fail closed")
	}
}

func TestValidateURLAllowlistPermitsInternal(t *testing.T) {
	v := newTestValidator(t, []string{"10.0.0.0/8", "llm.internal"},
		"10.1.2.3")
	internal := []string{
		"http://10.1.2.3:9000/v1",
		"http://llm.internal/v1", // hostname allowlisted without resolution check
	}
	for _, u := range internal {
		if err := v.ValidateURL(context.Background(), u); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want allowed", u, err)
		}
	}
	// Private target NOT covered by the allowlist still rejected.
	v.resolve = resolverOf("172.16.0.1")
	if err := v.ValidateURL(context.Background(), "http://172.16.0.1/v1"); err == nil {
		t.Fatal("private target outside the allowlist must be rejected")
	}
}

func TestValidateURLAllowlistCIDRBoundary(t *testing.T) {
	v := newTestValidator(t, []string{"10.0.0.0/8"}, "192.168.1.5")
	if err := v.ValidateURL(context.Background(), "http://192.168.1.5/v1"); err == nil {
		t.Fatal("private target outside allowlist must be rejected")
	}
	v2 := newTestValidator(t, nil, "93.184.216.34")
	if err := v2.ValidateURL(context.Background(), "https://api.deepseek.com"); err != nil {
		t.Fatalf("public target must be allowed: %v", err)
	}
}

func TestValidateRedirect(t *testing.T) {
	v, err := NewEgressValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	v.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "other-public.example" {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	if err := v.ValidateRedirect(context.Background(), "https://other-public.example/v1"); err != nil {
		t.Fatalf("public redirect target: %v", err)
	}
	// A redirect to a blocked address must not smuggle past the policy.
	if err := v.ValidateRedirect(context.Background(), "http://metadata-hop.example/"); err == nil {
		t.Fatal("redirect to metadata address must be rejected")
	}
}

func TestValidateCustomHeaders(t *testing.T) {
	ok := map[string]string{"X-Tenant": "acme", "x-extra-flag": "1"}
	if err := ValidateCustomHeaders(ok); err != nil {
		t.Fatalf("benign headers: %v", err)
	}
	blocked := []map[string]string{
		{"Authorization": "Bearer x"},
		{"authorization": "Bearer x"}, // case trick
		{"Proxy-Authorization": "Basic x"},
		{"Host": "evil"},
		{"X-Api-Key": "stolen"},
		{"x-api-key": "stolen"},
		{"X-Request-Id": "forged"},
	}
	for _, h := range blocked {
		if err := ValidateCustomHeaders(h); err == nil {
			t.Errorf("headers %v must be rejected", h)
		}
	}
}

func TestNewEgressValidatorBadAllowlist(t *testing.T) {
	for _, bad := range []string{"not-a-cidr/8", "10.0.0.1", "10.0.0.256/8"} {
		if _, err := NewEgressValidator([]string{bad}); err == nil {
			t.Errorf("allowlist entry %q must be rejected", bad)
		}
	}
}
