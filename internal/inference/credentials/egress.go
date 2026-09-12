// egress.go — 出站（SSRF）防护。设计 §5：地址校验拒绝元数据地址及未授权内网
// 目标；自托管内网模型只能经运营显式配置的允许列表接入。校验在两层生效：
// 配置写入时（deployment.base_url）拒绝非法目标；网关出站（Task 8）在真正
// 拨号前再次校验并重定向（ValidateRedirect），防止 DNS 重绑定穿越配置期校验。

package credentials

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// nonGlobalPrefixes are the IANA special-purpose ranges NOT covered by
// netip's IsLoopback/IsPrivate/IsLinkLocal*/IsMulticast/IsUnspecified that
// must still never be upstream targets (评审轮1 I3): 100.64.0.0/10 is the
// CGNAT shared space that carries the Alibaba cloud metadata endpoint
// 100.100.100.200; 240.0.0.0/4 subsumes the limited broadcast
// 255.255.255.255/32. The operator allowlist can still override each of
// these (same semantics as the other checks).
var nonGlobalPrefixes = []struct {
	prefix netip.Prefix
	reason string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "this-host/source addresses (0.0.0.0/8)"},
	{netip.MustParsePrefix("100.64.0.0/10"), "shared address space (CGNAT; carries cloud metadata 100.100.100.200)"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation range (TEST-NET-1)"},
	{netip.MustParsePrefix("192.88.99.0/24"), "6to4 relay anycast (deprecated)"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking range"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation range (TEST-NET-2)"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation range (TEST-NET-3)"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved range (includes limited broadcast 255.255.255.255)"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "local-use NAT64 prefix"},
	{netip.MustParsePrefix("100::/64"), "discard-only prefix"},
	{netip.MustParsePrefix("2001:1::1/128"), "Port Control Protocol anycast"},
	{netip.MustParsePrefix("2001:1::2/128"), "Traversal Using Relays around NAT anycast"},
	{netip.MustParsePrefix("2001:2::/48"), "benchmarking prefix"},
	{netip.MustParsePrefix("2001:10::/28"), "ORCHIDv1 (deprecated)"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation prefix"},
	{netip.MustParsePrefix("3fff::/20"), "documentation prefix"},
}

// nat64WellKnown is 64:ff9b::/96 (RFC 6052 well-known NAT64 prefix): the
// low 32 bits are a translated IPv4 address, so the address is judged by
// the mapped IPv4 (评审轮1 I3: 映射回 IPv4 的按映射后判定).
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// blockedReason describes why an address is not a permitted upstream target.
// Anything that is not a globally routable unicast address is denied by
// default; the operator allowlist is the ONLY way to permit non-global
// targets (self-hosted intranet deployments).
//
// 评审轮2 S-1：策略判定剥离 IPv6 zone——netip.Prefix.Contains 对带 zone
// 地址一律 false，URL 字面 host 可携带 zone（%25 编码）绕过全部前缀与
// NAT64 映射判定；拨号仍用原始带 zone 形式（DialContext 侧不剥）。
func blockedReason(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() && nat64WellKnown.Contains(ip) {
		b := ip.As16()
		return blockedReason(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	switch {
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLoopback():
		return "loopback address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// covers 169.254.169.254 (cloud metadata), fe80::/10
		return "link-local address"
	case ip.IsMulticast():
		return "multicast address"
	case ip.IsPrivate():
		// RFC1918 + fc00::/7 (fd00::/8 ULA)
		return "private address"
	}
	for _, np := range nonGlobalPrefixes {
		if np.prefix.Contains(ip) {
			return np.reason
		}
	}
	return ""
}

// EgressValidator validates upstream URLs against the SSRF policy.
type EgressValidator struct {
	allowNets  []netip.Prefix
	allowHosts map[string]bool
	// resolve is swappable in tests; production resolves via the system
	// resolver with a bounded timeout.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

// NewEgressValidator builds a validator from the allowlist config: a comma-
// separated list of CIDRs (e.g. 10.0.0.0/8, fd00::/8) and/or exact hostnames
// (e.g. llm.internal). An empty allowlist permits only globally routable
// targets.
func NewEgressValidator(allowlist []string) (*EgressValidator, error) {
	v := &EgressValidator{
		allowHosts: map[string]bool{},
		resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			r := &net.Resolver{}
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return r.LookupNetIP(ctx, "ip", host)
		},
	}
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("egress allowlist: invalid CIDR %q: %v", entry, err)
			}
			v.allowNets = append(v.allowNets, p.Masked())
			continue
		}
		if _, err := netip.ParseAddr(entry); err == nil {
			return nil, fmt.Errorf("egress allowlist: bare IP %q must be a CIDR (e.g. %s/32)", entry, entry)
		}
		v.allowHosts[entry] = true
	}
	return v, nil
}

// AllowlistEntry reports whether host is explicitly allowlisted (exact
// hostname match or CIDR containment of the resolved address).
func (v *EgressValidator) allowlistEntry(host string, ips []netip.Addr) bool {
	if v.allowHosts[strings.ToLower(host)] {
		return true
	}
	for _, ip := range ips {
		for _, p := range v.allowNets {
			if p.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// ValidateURL checks that rawURL is an acceptable upstream base URL:
// absolute http(s), no userinfo, and every address the host resolves to is
// globally routable unless explicitly allowlisted. Hostnames that fail to
// resolve are rejected (fail closed) — a typo'd deployment must surface at
// configuration time, not as a per-request 502.
func (v *EgressValidator) ValidateURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("upstream URL %q is not parseable: %v", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("upstream URL %q must use http or https", rawURL)
	}
	if u.Host == "" {
		return fmt.Errorf("upstream URL %q has no host", rawURL)
	}
	if u.User != nil {
		return fmt.Errorf("upstream URL %q must not embed credentials in the userinfo", rawURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("upstream URL %q has an empty hostname", rawURL)
	}

	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip.Unmap()}
	} else {
		ips, err = v.resolve(ctx, host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("upstream host %q does not resolve: %v", host, err)
		}
	}

	for _, ip := range ips {
		ip = ip.Unmap()
		if reason := blockedReason(ip); reason != "" {
			if v.allowlistEntry(host, []netip.Addr{ip.WithZone("")}) {
				continue
			}
			return fmt.Errorf("upstream host %q resolves to %s (%s); add it to INFERENCE_UPSTREAM_ALLOWLIST to permit an internal target", host, ip, reason)
		}
	}
	return nil
}

// ValidateRedirect re-applies the URL policy to an outbound redirect target.
// Cross-origin redirects are allowed (upstream gateways legitimately redirect
// between hosts), but the target must independently pass ValidateURL — a
// redirect must not become the channel that smuggles a blocked address past
// the initial check (e.g. DNS-rebinding between config time and dial time).
func (v *EgressValidator) ValidateRedirect(ctx context.Context, target string) error {
	return v.ValidateURL(ctx, target)
}

// DialContext resolves host AT DIAL TIME and dials only addresses that pass
// the same blockedReason/allowlist policy as ValidateURL (评审轮1 I4): the
// resolution ValidateURL performed moments earlier and the resolver answer
// at connection time can differ (DNS rebinding TOCTOU), so the policy must
// bind to the actual dialed IP, not to a stale validation. Blocked
// candidates are skipped (only validated IPs are dialed); the last failure
// surfaces when nothing is dialable. TLS ServerName and the Host header
// come from the request URL inside http.Transport, not from the dialed
// address, so dialing by validated IP preserves both.
func (v *EgressValidator) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("egress dial: split host/port %q: %v", addr, err)
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	allowed := func(host string, ip netip.Addr) (string, bool) {
		ip = ip.Unmap()
		if reason := blockedReason(ip); reason != "" && !v.allowlistEntry(host, []netip.Addr{ip.WithZone("")}) {
			return reason, false
		}
		return "", true
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if reason, ok := allowed(host, ip); !ok {
			return nil, fmt.Errorf("egress dial: %s is blocked (%s)", ip.Unmap(), reason)
		}
		return d.DialContext(ctx, network, addr)
	}
	ips, err := v.resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("egress dial: host %q does not resolve: %v", host, err)
	}
	var lastErr error
	for _, ip := range ips {
		if reason, ok := allowed(host, ip); !ok {
			lastErr = fmt.Errorf("egress dial: host %q resolves to %s (%s) — blocked", host, ip.Unmap(), reason)
			continue
		}
		conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// reservedHeaderPrefixes blocks custom operator-configured request headers
// that would collide with the gateway's own auth / tracing boundary (设计
// §5：自定义请求头不得覆盖网关认证/追踪边界或泄漏秘密).
// ValidateCustomHeaders rejects operator-supplied headers that would
// override the gateway's authentication or tracing boundaries. The blacklist
// lives in the domain package (single source of truth); deployment writes
// enforce it via domain.ExtensionHeaders + this check on the write path.
func ValidateCustomHeaders(headers map[string]string) error {
	return domain.ValidateCustomHeaders(headers)
}

// ErrNoValidator is returned by lifecycle helpers when the deployment did
// not configure an egress validator (fail closed).
var ErrNoValidator = errors.New("egress validator not configured")
