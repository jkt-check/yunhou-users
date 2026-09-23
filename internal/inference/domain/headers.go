// headers.go — 网关边界的自定义请求头约束（设计 §5：自定义请求头不得
// 覆盖网关认证/追踪边界或泄漏秘密）。
//
// 这里是唯一的黑名单实现源：credentials.ValidateCustomHeaders 对外 re-export，
// catalog 的部署写入路径（ValidateDeployment）直接调用，Task 8 网关引入
// 协议头时任何新的 header 通路必须先过 ValidateCustomHeaders。

package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// reservedHeaderPrefixes blocks header names the gateway sets itself.
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
		canonical := canonicalHeaderKey(name)
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

// canonicalHeaderKey is textproto.CanonicalMIMEHeaderKey without importing
// net/http into the domain package; kept local for clarity.
func canonicalHeaderKey(s string) string {
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

// ExtensionHeaders extracts the optional "headers" object from an
// ExtensionConfig payload. Absent (or null) → (nil, nil): there is no
// headers convention yet, so a config without the key is untouched. Present
// but not a JSON object of string→string → error (fail closed): smuggled or
// malformed header payloads must not reach the gateway silently.
func ExtensionHeaders(cfg ExtensionConfig) (map[string]string, error) {
	if len(cfg.Raw) == 0 || string(cfg.Raw) == "null" {
		return nil, nil
	}
	var probe struct {
		Headers json.RawMessage `json:"headers"`
	}
	if err := json.NewDecoder(bytes.NewReader(cfg.Raw)).Decode(&probe); err != nil {
		return nil, NewError(CodeInvalidInput, "deployment config is not valid JSON: "+err.Error())
	}
	if len(probe.Headers) == 0 || string(probe.Headers) == "null" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal(probe.Headers, &headers); err != nil {
		return nil, NewError(CodeInvalidInput,
			`deployment config "headers" must be an object of string header name to string value`)
	}
	return headers, nil
}
