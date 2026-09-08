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
)

// blockedReason describes why an address is not a permitted upstream target.
// Anything that is not a globally routable unicast address is denied by
// default; the operator allowlist is the ONLY way to permit non-global
// targets (self-hosted intranet deployments).
func blockedReason(ip netip.Addr) string {
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
	default:
		return ""
	}
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
			if v.allowlistEntry(host, []netip.Addr{ip}) {
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

// reservedHeaderPrefixes blocks custom operator-configured request headers
// that would collide with the gateway's own auth / tracing boundary (设计
// §5：自定义请求头不得覆盖网关认证/追踪边界或泄漏秘密).
var reservedHeaderPrefixes = []string{"Authorization", "Proxy-Authorization", "Host"}

// reservedHeaderExact blocks exact header names the gateway sets itself.
var reservedHeaderExact = map[string]bool{
	"X-Api-Key":    true, // gateway injects the upstream credential
	"X-Request-Id": true, // tracing boundary
}

// ValidateCustomHeaders rejects operator-supplied headers that would
// override the gateway's authentication or tracing boundaries. Header names
// are canonicalised before comparison so case tricks don't slip through.
func ValidateCustomHeaders(headers map[string]string) error {
	for name := range headers {
		canonical := httpCanonicalHeaderKey(name)
		if reservedHeaderExact[canonical] {
			return fmt.Errorf("custom header %q is managed by the gateway and must not be overridden", name)
		}
		for _, prefix := range reservedHeaderPrefixes {
			if canonical == prefix {
				return fmt.Errorf("custom header %q is managed by the gateway and must not be overridden", name)
			}
		}
	}
	return nil
}

// httpCanonicalHeaderKey is textproto.CanonicalMIMEHeaderKey without importing
// net/http into this file's API surface twice; kept local for clarity.
func httpCanonicalHeaderKey(s string) string {
	// net/http's canonical form: first letter and letters after '-' upper.
	upper := true
	b := []byte(s)
	for i, c := range b {
		if upper && 'a' <= c && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
		upper = c == '-'
	}
	return string(b)
}

// ErrNoValidator is returned by lifecycle helpers when the deployment did
// not configure an egress validator (fail closed).
var ErrNoValidator = errors.New("egress validator not configured")
