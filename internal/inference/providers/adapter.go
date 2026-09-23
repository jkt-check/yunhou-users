// Package providers holds the upstream protocol adapters of the inference
// module (设计 §3 providers: 协议适配、流式解析、上游能力声明).
//
// The OpenAI Chat Completions and Anthropic Messages adapters are
// selectively adapted from the feat/multi-model-gateway candidate branch
// (internal/llm/{openai,anthropic,anthropic_stream}.go, 基线报告 §3.2
// 定向复用清单). The candidate's lossy billing semantics are NOT inherited:
// usage is tracked incrementally with nil-vs-zero fidelity (unknown never
// collapses into 0), streaming usage is always explicitly requested, and
// the terminal state follows the protocol end marker ([DONE] /
// message_stop), never a bare EOF.
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// ChatRequest is the protocol-neutral internal chat call. Messages use the
// OpenAI-compatible shape (internal/model.ChatMessage); Anthropic upstreams
// translate from it. Fields the module cannot honor are rejected at parse
// time by the httpapi layer — a request that reaches an adapter is already
// capability-checked (设计: 不支持的工具/模态明确报错，不能静默丢字段).
type ChatRequest struct {
	// Model is the resolved PUBLIC model id (routing rewrites it to the
	// upstream model name at dispatch).
	Model    string
	Messages []model.ChatMessage
	Stream   bool
	// MaxTokens is the client-declared output cap; the admission gate
	// forces min(client, model hard limit) — see quota.EffectiveOutputCap.
	MaxTokens *int64
	// Tools are opaque OpenAI tool schemas, relayed verbatim.
	Tools []json.RawMessage
	// ToolChoice is relayed verbatim when present.
	ToolChoice json.RawMessage
	// ThinkingEnabled toggles reasoning mode (OpenAI thinking / Anthropic
	// extended thinking) on models that declare SupportsReasoning.
	ThinkingEnabled *bool
	// ThinkingBudget is the client-declared Anthropic thinking budget
	// (budget_tokens). Nil means the adapter's default. Only the anthropic
	// adapter consumes it; other adapters ignore it (their protocols have
	// no budget concept — documented in the capability matrix).
	ThinkingBudget *int64
	// Passthrough carries the allowlisted sampling parameters
	// (temperature, top_p, stop, presence_penalty, frequency_penalty,
	// seed). Each adapter maps what its protocol supports and rejects the
	// rest instead of silently dropping them.
	Passthrough map[string]any
}

// Call is one upstream dispatch: the routed deployment, the resolved secret
// material, the forced output cap, and the tracing id.
type Call struct {
	Deployment *domain.Deployment
	// Secret is the decrypted upstream credential (never logged).
	Secret []byte
	// RequestID is the gateway's logical request id, propagated as the
	// X-Request-Id tracing boundary (operator headers can never override
	// it — domain.ValidateCustomHeaders).
	RequestID string
	Request   *ChatRequest
	// OutputCap is the forced output ceiling forwarded upstream (设计 §7.2:
	// 不允许无限输出).
	OutputCap int64
	// ExtraHeaders come from the deployment's extension config; they were
	// blacklist-validated at write time and are re-validated at dispatch.
	ExtraHeaders map[string]string
}

// Adapter normalizes one upstream wire protocol.
type Adapter interface {
	Protocol() domain.Protocol
	Capabilities() domain.AdapterCapabilities
	// Inclusion declares which buckets the upstream's raw usage totals
	// already contain, so metering and accounting share ONE overlap rule
	// (设计 §7.1: 适配器负责规范化重叠语义).
	Inclusion() accounting.Inclusion
	// BuildPayload renders the upstream request body. OutputCap is always
	// forwarded as the protocol's hard output limit.
	BuildPayload(call *Call) ([]byte, error)
	// AuthHeaders turns the decrypted secret into the protocol's auth
	// headers (Authorization: Bearer vs x-api-key).
	AuthHeaders(secret []byte) map[string]string
	// WrapStream converts an upstream streaming 200 body into an
	// OpenAI-shaped SSE byte stream plus its incremental usage tap.
	// Anthropic streams are translated; OpenAI streams pass through.
	WrapStream(body io.ReadCloser) *Stream
	// DecodeNonStream parses a non-streaming 200 body: the OpenAI-shaped
	// chat.completion payload for the client, the normalized usage, and
	// whether the upstream reported usage at all.
	DecodeNonStream(body []byte) (*NonStreamResult, error)
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// DispatchError describes a failed upstream attempt. A non-zero StatusCode
// is an upstream HTTP rejection (the request WAS received and refused — no
// consumption); a zero-status transport error splits into
// definitely-not-executed (DNS/connect-refused) and unknown-execution
// (timeout/reset after the request may have left the box).
type DispatchError struct {
	StatusCode int
	// Body is the capped upstream error payload (diagnostics only, never
	// relayed verbatim to clients).
	Body string
	// Err is the transport error when StatusCode == 0.
	Err error
	// ExecutedUnknown marks "the upstream may have executed": the
	// reservation must be held for reconciliation, never silently released
	// (设计 §7.2).
	ExecutedUnknown bool
}

func (e *DispatchError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("upstream status %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("upstream transport: %v", e.Err)
}

func (e *DispatchError) Unwrap() error { return e.Err }

// Retryable reports whether the failure matches the bounded-retry policy
// (设计 §7.2/任务书: 429/5xx/网络 且未开始客户端输出). Retryability says
// nothing about billing — every attempt is persisted and the customer is
// settled exactly once per request.
func (e *DispatchError) Retryable() bool {
	if e.StatusCode != 0 {
		return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
	}
	if e.Err == nil {
		return false
	}
	return !IsCanceled(e.Err) // caller-side cancellation is never retried
}

// IsCanceled reports whether err is a caller-side (client disconnect /
// caller deadline) cancellation, as opposed to an upstream-side failure.
func IsCanceled(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return false // a timeout is unknown-execution, not cancellation
	}
	return errors.Is(err, context.Canceled)
}

// classifyTransport splits a transport error into definitely-not-executed
// (DNS failure, connection refused — the request never reached a server)
// vs unknown-execution (timeout/reset after the request may have left).
func classifyTransport(err error) (executedUnknown bool) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false
	}
	return true
}

// upstreamErrorBodyCap bounds the error body kept for diagnostics.
const upstreamErrorBodyCap = 8 << 10

// nonStreamBodyCap bounds a buffered non-streaming response. 8 MiB covers
// the model hard-output ceilings while bounding memory per request.
const nonStreamBodyCap = 8 << 20

// EgressChecker re-validates the outbound target at dispatch time (Task 4
// SSRF policy: DNS-rebinding between config time and dial time must not
// smuggle a blocked target through).
type EgressChecker interface {
	ValidateURL(ctx context.Context, rawURL string) error
	ValidateRedirect(ctx context.Context, target string) error
}

// egressDialer is the dial-time half of the egress policy (评审轮1 I4): the
// checker that also validates every candidate IP AT CONNECTION TIME and
// dials only validated addresses. ValidateURL's resolution and the resolver
// answer at dial time can differ (DNS rebinding TOCTOU), so the policy must
// bind to the actual dialed IP. *credentials.EgressValidator implements it;
// test doubles that only implement EgressChecker keep the plain dialer.
type egressDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewHTTPClient builds the shared dispatch client. There is deliberately no
// client-level Timeout: stream lifetimes are bounded by the per-request
// context (deployment.RequestTimeout) and the transport keeps explicit
// dial / TLS / response-header deadlines so a silently-hung upstream fails
// in seconds instead of pinning a connection. Redirects re-validate against
// the egress policy (Task 4); when the egress checker also provides a
// policy-binding DialContext, every dialed IP is validated at connection
// time (I4 — covers the first dial AND post-redirect dials alike).
//
// 评审轮2 I1：跨 origin 重定向绝不跟随。Go 的 http.Client 跨主机跳转只剥
// Authorization/Cookie，x-api-key 与 operator 扩展头会被逐字拷给新主机，
// 307/308 还会重发完整请求体（客户 prompt）。返回 ErrUseLastResponse 让
// 3xx 响应体作为上游错误 surfaced（Dispatch 按非 200 处理）。
func NewHTTPClient(egress EgressChecker) *http.Client {
	dial := (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	if egress != nil {
		if ed, ok := egress.(egressDialer); ok {
			dial = ed.DialContext
		}
	}
	c := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("providers: too many redirects")
		}
		// 评审轮2 I1：任一跳转离开初始 origin 即拒绝跟随 —— 上游密钥与
		// operator 扩展头绝不能落进另一台主机（egress IP 策略对所有公网
		// 主机都放行，挡不住凭证外泄）。
		if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
			return http.ErrUseLastResponse
		}
		if egress != nil {
			return egress.ValidateRedirect(req.Context(), req.URL.String())
		}
		return nil
	}
	return c
}

// sameOrigin reports whether two URLs share scheme and host (重定向凭证边界).
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// Dispatch performs one upstream call. A nil *DispatchResult plus a
// *DispatchError means the attempt failed BEFORE any client output — the
// gateway decides retry vs release vs reconcile from the error's
// Retryable/ExecutedUnknown flags. A BuildPayload failure is a client/shape
// error (*domain.Error with CodeInvalidInput), not an upstream failure.
func Dispatch(ctx context.Context, client *http.Client, a Adapter, call *Call) (*DispatchResult, error) {
	payload, err := a.BuildPayload(call)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EndpointURL(call.Deployment), bytes.NewReader(payload))
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "providers: build upstream request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", call.RequestID)
	for k, v := range a.AuthHeaders(call.Secret) {
		req.Header.Set(k, v)
	}
	// Operator-configured headers: validated at write time (Task 4) and
	// re-validated at dispatch so a hand-edited row can never override the
	// auth/tracing boundary.
	if len(call.ExtraHeaders) > 0 {
		if err := domain.ValidateCustomHeaders(call.ExtraHeaders); err != nil {
			return nil, domain.WrapError(domain.CodeInternal,
				"providers: deployment headers violate the gateway boundary", err)
		}
		for k, v := range call.ExtraHeaders {
			req.Header.Set(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		if IsCanceled(err) {
			return nil, &DispatchError{Err: err} // caller gone — not retryable
		}
		return nil, &DispatchError{Err: err, ExecutedUnknown: classifyTransport(err)}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyCap))
		return nil, &DispatchError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	d := &DispatchResult{UpstreamRequestID: upstreamRequestID(resp)}
	if call.Request.Stream {
		d.Stream = a.WrapStream(resp.Body)
		return d, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, nonStreamBodyCap+1))
	if err != nil {
		return nil, &DispatchError{Err: err, ExecutedUnknown: true}
	}
	if len(body) > nonStreamBodyCap {
		return nil, &DispatchError{
			Err:             fmt.Errorf("providers: upstream response exceeds %d bytes", nonStreamBodyCap),
			ExecutedUnknown: true,
		}
	}
	ns, err := a.DecodeNonStream(body)
	if err != nil {
		// A 200 we cannot parse: consumption unknown, nothing relayed.
		return nil, &DispatchError{Err: err, ExecutedUnknown: true}
	}
	d.Payload = ns.Payload
	d.Usage = ns.Usage
	d.UsageRaw = ns.UsageRaw
	d.UsageReported = ns.UsageReported
	d.ContentBytes = ns.ContentBytes
	if d.UpstreamRequestID == "" {
		d.UpstreamRequestID = ns.UpstreamRequestID
	}
	return d, nil
}

// DispatchResult is one admitted upstream attempt's live result.
type DispatchResult struct {
	UpstreamRequestID string
	// Stream is set for streaming calls (OpenAI-shaped SSE + usage tap).
	Stream *Stream
	// Non-stream results:
	Payload       []byte
	Usage         domain.UsageBuckets
	UsageRaw      json.RawMessage
	UsageReported bool
	// ContentBytes is the visible answer size for the estimate path.
	ContentBytes int64
}

// NonStreamResult is the adapter's decode of a non-streaming response.
type NonStreamResult struct {
	Payload           []byte
	Usage             domain.UsageBuckets
	UsageRaw          json.RawMessage
	UsageReported     bool
	UpstreamRequestID string
	// ContentBytes is the visible answer size (content + reasoning across
	// choices) — the output side of the estimate when the upstream omits
	// usage (与流式 tap 的 ContentBytes 同口径).
	ContentBytes int64
}

// upstreamRequestID prefers the conventional response headers; adapters
// fall back to the payload id.
func upstreamRequestID(resp *http.Response) string {
	if id := resp.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	return resp.Header.Get("Request-Id")
}

// EndpointURL joins a deployment's base URL with its protocol path.
func EndpointURL(d *domain.Deployment) string {
	switch d.Protocol {
	case domain.ProtocolAnthropicMessage:
		return strings.TrimSuffix(d.BaseURL, "/") + "/v1/messages"
	default:
		return strings.TrimSuffix(d.BaseURL, "/") + "/chat/completions"
	}
}
