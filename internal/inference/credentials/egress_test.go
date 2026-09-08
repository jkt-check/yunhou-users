package credentials

import (
	"context"
	"errors"
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
