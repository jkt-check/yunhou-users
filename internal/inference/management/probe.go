// probe.go — deployment 级上游连通性探测（R7-N7："base_url 缺 /v1 → 上线后
// 404" 事故类的永久防线）。on-demand、只读：加载 deployment → 解出其
// provider 的可用凭据 → 按**协议**设置鉴权/版本头 → GET {base_url}/models
// → {ok, status, latency_ms, error_summary, …}。绝不写库；error_summary
// 永不携带 secret 字节（传输错误串经 secret 替换脱敏，上游响应体只丢弃、
// 永不进摘要）。
//
// 与 workers/upstream_health.go 的分层差异：那里是账号级定时轮询（connector
// 客户端 + 冷却/恢复状态机，会写账号状态）；这里是 deployment 级 on-demand
// 预发布探测，只返回报告、不写任何状态。两者不共享状态机，仅共享 egress
// 防线与 vault 解密面。
//
// T9 的诚实性契约（三件事必须分开，绝不互相冒充）：
//
//	credential_state —— 凭据**材料**测试：材料能不能解开（decrypted /
//	                    missing / unusable）；
//	probe_method/outcome —— **指定**的连通性探测做了什么、结论是什么
//	                    （connectivity+verified / failed / unsupported /
//	                    unconfirmed）；
//	inference_verified —— 恒为 false：本探测**从不**发起真实推理（默认不
//	                    触发付费推理），因此 ok:true 只表示「这条部署的上游
//	                    连通」，绝不表示「模型答过」。
//
// 供应商不提供 /models 时结论是 unsupported（此探测方式不支持/无法确认），
// 不是「模型故障」，也绝不是「推理成功」。

package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/providers"
)

// ProbeStore is the read surface the probe needs; satisfied by
// inference/postgres.Store.
type ProbeStore interface {
	GetDeployment(ctx context.Context, id string) (*domain.Deployment, error)
	ListCredentials(ctx context.Context, providerID string, limit int) ([]domain.Credential, error)
	// ListActiveUpstreamAccounts 是该 provider 的上游账号清单。发布预演
	// 用它判定「这条部署没有可用账号」——一个**可判定**的事实，而不是
	// 猜测；清单读不到时预演说「未知」，绝不谎称无账号。
	ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error)
}

// BearerResolver 是 credentials.Service.ResolveBearerToken 的最小面。
// management 不能直接 import credentials（credentials 已依赖本包的审计面，
// 反向即循环依赖），故以接口注入；生产直接传 *credentials.Service。
// pinGeneration 钉住代次（设计 §8：解析与读取之间发生轮换即冲突失败）。
//
// T1 口径：ResolveBearerToken 与热路径的 ResolveOutboundAuth **共用**同一个
// decodeOAuthAccessToken —— oauth 凭据解出来的只有 access token，密封闭包
// 与 refresh token 永远不出边界；不可用的 bundle 两处同样被拒。它同时把
// 完整的 *domain.Credential 交回，探测面据此知道该凭据的 auth_type（协议
// 决定用哪个鉴权槽）。
type BearerResolver interface {
	ResolveBearerToken(ctx context.Context, id string, pinGeneration *int64) (string, *domain.Credential, error)
}

// DeploymentProber is the per-deployment probe consumed by the publish
// dry-run report; *ProbeService implements it, tests inject fakes.
type DeploymentProber interface {
	ProbeDeployment(ctx context.Context, id string) (*ProbeResult, error)
}

// AccountInventory 是发布预演读的账号清单面。*ProbeService 通过它的 store
// 实现它；预演用类型断言取它 —— 取不到就**跳过**「无账号」判定并在报告里
// 说明清单未知（绝不猜）。
type AccountInventory interface {
	ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error)
}

// ProbeOutcome 是一次探测的诚实结论。
const (
	// ProbeOutcomeVerified: 供应商声明的探测端点返回 2xx —— 连通性已确认。
	// 这**不是**推理成功（inference_verified 恒为 false）。
	ProbeOutcomeVerified = "verified"
	// ProbeOutcomeFailed: 端点存在并作答了，但不是成功（典型：401/403/500）。
	ProbeOutcomeFailed = "failed"
	// ProbeOutcomeUnsupported: 供应商在该 base_url 上**不提供**模型列表端点
	// （404/405/501）。这一探测方式无法确认任何事 —— 它**不是**模型故障的
	// 证据（base_url 缺一段路径也会得到同样的 404）。
	ProbeOutcomeUnsupported = "unsupported"
	// ProbeOutcomeUnconfirmed: 没有得到任何答复（传输错误、超时、egress
	// 拒绝、凭据不可用、探测根本没跑）。
	ProbeOutcomeUnconfirmed = "unconfirmed"
)

// ProbeCredentialState 是凭据**材料**测试的结论。
const (
	// ProbeCredentialDecrypted: 材料解开且可用。
	ProbeCredentialDecrypted = "decrypted"
	// ProbeCredentialMissing: 该 provider 没有可用凭据（无 active/rotating）。
	ProbeCredentialMissing = "missing"
	// ProbeCredentialUnusable: 有凭据但材料不可用（解密失败 / bundle 不可用）。
	ProbeCredentialUnusable = "unusable"
)

// ProbeMethod 说明探测实际做了什么。
const (
	// ProbeMethodConnectivity: 真的向供应商的探测端点发了请求。
	ProbeMethodConnectivity = "connectivity"
	// ProbeMethodNone: 没有发任何请求（探测没能跑起来）。
	ProbeMethodNone = "none"
)

// ProbeResult 是一次探测的 advisory 结论。Status=0 表示未拿到任何 HTTP
// 响应（DNS 失败/拒绝/超时）；ErrorSummary 永不包含 secret 字节。
//
// T9 在**保留**既有字段语义的前提下**追加**了描述字段：凭据材料测试
// （CredentialState）、探测方法与结论（ProbeMethod / ProbeOutcome /
// ProbeSupported）、本次未做真实推理（InferenceVerified）与一句人话
// （Description）。
type ProbeResult struct {
	DeploymentID string `json:"deployment_id"`
	OK           bool   `json:"ok"`
	Status       int    `json:"status"`
	LatencyMS    int64  `json:"latency_ms"`
	ErrorSummary string `json:"error_summary,omitempty"`

	// CredentialState 是凭据材料测试的结论（decrypted / missing /
	// unusable）——它与「探测成功」是两件事。
	CredentialState string `json:"credential_state"`
	// ProbeMethod 说明探测做了什么（connectivity / none）。
	ProbeMethod string `json:"probe_method"`
	// ProbeOutcome 是诚实的结论（verified / failed / unsupported /
	// unconfirmed）。供应商不提供模型列表端点时是 unsupported。
	ProbeOutcome string `json:"probe_outcome"`
	// ProbeSupported 报告探测是否**确认**了供应商提供该端点。false 表示
	// 「没确认」——既包括供应商不提供，也包括探测没有得到答复（看
	// ProbeOutcome 区分）。它**不是**「模型不健康」的断言。
	ProbeSupported bool `json:"probe_supported"`
	// InferenceVerified 恒为 false：本探测从不发起真实推理（默认不触发
	// 付费推理）。显式发布出来，免得有人把 ok:true 读成「模型答过」。
	InferenceVerified bool `json:"inference_verified"`
	// Description 是给运营看的一句话结论。
	Description string `json:"description,omitempty"`
}

// probeBodyCap bounds the discarded /models response body.
const probeBodyCap = 8 << 10

// probeSummaryCap bounds error_summary length.
const probeSummaryCap = 300

// DefaultProbeTimeout 是单次探测的默认上限（brief: 10s）。
const DefaultProbeTimeout = 10 * time.Second

// probePath is the vendor-side endpoint the connectivity probe reads. It is
// a READ of the vendor's model list — never an inference, so it never costs
// money.
const probePath = "/models"

// ProbeService runs on-demand, read-only upstream connectivity probes.
type ProbeService struct {
	store       ProbeStore
	resolver    BearerResolver
	validateURL func(ctx context.Context, rawURL string) error
	client      *http.Client
	// Timeout bounds one probe attempt; <=0 falls back to DefaultProbeTimeout.
	Timeout time.Duration
}

// NewProbeService builds the probe service. validateURL is the egress/SSRF
// policy — nil fails closed on any non-empty base_url（与 CatalogManager 写
// 路径同口径）; client MUST be providers.NewHTTPClient(egress) in production
// so every dialed IP is validated at connection time（DNS rebinding 兜底）。
// nil client 是装配错误：探测时 fail-closed 报 CodeInternal，绝不回落
// http.DefaultClient（它跟随重定向、无 egress 重校验、无 dial 时 IP 绑定
// ——SSRF 边界不允许静默降级，评审修复 R7-N7 轮1）。
func NewProbeService(store ProbeStore, resolver BearerResolver, validateURL func(context.Context, string) error, client *http.Client) *ProbeService {
	return &ProbeService{store: store, resolver: resolver, validateURL: validateURL, client: client, Timeout: DefaultProbeTimeout}
}

func (s *ProbeService) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultProbeTimeout
}

// ListActiveUpstreamAccounts implements AccountInventory: the provider's
// account inventory, read through the same store the probe uses.
func (s *ProbeService) ListActiveUpstreamAccounts(ctx context.Context, providerID string) ([]domain.UpstreamAccount, error) {
	return s.store.ListActiveUpstreamAccounts(ctx, providerID)
}

// ProbeDeployment probes one deployment's upstream. Lookup failures
// (unknown deployment) are hard errors; every probe-level failure is
// reported in-band as {ok:false} — the endpoint itself stays 200.
func (s *ProbeService) ProbeDeployment(ctx context.Context, deploymentID string) (*ProbeResult, error) {
	res := &ProbeResult{
		DeploymentID:      deploymentID,
		CredentialState:   ProbeCredentialMissing,
		ProbeMethod:       ProbeMethodNone,
		ProbeOutcome:      ProbeOutcomeUnconfirmed,
		InferenceVerified: false,
	}
	dep, err := s.store.GetDeployment(ctx, deploymentID)
	if err != nil {
		return nil, err
	}
	// SSRF 边界 fail-closed：client 未装配 = 配置错误，硬失败且绝不出站。
	if s.client == nil {
		return nil, domain.NewError(domain.CodeInternal, "probe http client not configured")
	}
	// 1. 凭据材料测试（T1 的出站材料口径）：材料解不开就**没有**探测可做，
	//    结论是 unconfirmed（绝不是「上游模型故障」）。
	token, cred, err := s.resolveProviderToken(ctx, dep.ProviderID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			res.CredentialState = ProbeCredentialMissing
			res.ErrorSummary = capProbeSummary("no usable credential for provider " + dep.ProviderID)
			res.Description = "the provider has no usable credential — no connectivity probe was performed"
		} else {
			res.CredentialState = ProbeCredentialUnusable
			res.ErrorSummary = safeProbeErrorSummary("credential resolution failed", err)
			res.Description = "the credential material could not be decrypted — no connectivity probe was performed"
		}
		return res, nil
	}
	res.CredentialState = ProbeCredentialDecrypted

	base := strings.TrimSuffix(strings.TrimSpace(dep.BaseURL), "/")
	if base == "" {
		res.ErrorSummary = "deployment has no base_url"
		res.Description = "the deployment has no base_url — no connectivity probe was performed"
		return res, nil
	}
	// SSRF 防线：出站前过 egress 策略（与写路径同一 validator；生产 client
	// 另有 dial 时 IP 校验兜底 DNS rebinding）。拒绝即绝不出站。
	if s.validateURL == nil {
		res.ErrorSummary = "egress validation not configured"
		res.Description = "the egress policy is not configured — no connectivity probe was performed"
		return res, nil
	}
	if err := s.validateURL(ctx, base); err != nil {
		res.ErrorSummary = capProbeSummary("base_url rejected by egress policy: " + err.Error())
		res.Description = "the base_url was rejected by the egress policy — no connectivity probe was performed"
		return res, nil
	}
	// 2. 按协议设置鉴权/版本头 —— 与派发路径**同一个适配器**的同一份
	//    鉴权规则（T1）：anthropic_messages 走 x-api-key + anthropic-version；
	//    OpenAI 兼容协议走 Authorization: Bearer。该适配器对
	//    anthropic_messages + oauth 的**明确拒绝**在这里同样成立：
	//    出站前拒绝，绝不把 OAuth 令牌塞进 API-key 槽。
	headers, herr := s.probeHeaders(dep, token, cred)
	if herr != nil {
		res.ErrorSummary = capProbeSummary(safeProbeErrorSummary("probe refused before dispatch", herr))
		res.Description = "the credential is not usable with this deployment's protocol — no request was sent"
		return res, nil
	}

	pctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+probePath, nil)
	if err != nil {
		res.ErrorSummary = capProbeSummary("build probe request: " + err.Error())
		res.Description = "the probe request could not be built — no request was sent"
		return res, nil
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res.ProbeMethod = ProbeMethodConnectivity
	start := time.Now()
	resp, err := s.client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.ErrorSummary = capProbeSummary(redactProbeSecret(s.transportErrorSummary(pctx, err), token))
		res.Description = "the connectivity probe got no answer — connectivity could not be confirmed"
		return res, nil
	}
	defer resp.Body.Close()
	// 响应体只丢弃不引用：上游错误体可能回显请求头，绝不能让 secret 经
	// error_summary 外泄。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyCap))
	res.Status = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.OK = true
		res.ProbeOutcome = ProbeOutcomeVerified
		res.ProbeSupported = true
		res.Description = "the connectivity probe succeeded — this is NOT an inference (inference_verified=false)"
	case resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusMethodNotAllowed ||
		resp.StatusCode == http.StatusNotImplemented:
		// 供应商在该 base_url 上不提供模型列表端点 —— **不是**模型故障。
		res.ProbeOutcome = ProbeOutcomeUnsupported
		res.ProbeSupported = false
		res.ErrorSummary = fmt.Sprintf("upstream returned HTTP %d %s — the vendor does not expose a model-list endpoint at this base_url; this probe method cannot confirm connectivity (not evidence of an unhealthy model; a base_url missing a path segment yields the same 404)",
			resp.StatusCode, http.StatusText(resp.StatusCode))
		res.Description = "the vendor does not expose a model-list endpoint here — this probe method cannot confirm connectivity; this is NOT a model fault"
	default:
		// 端点存在并作答了，但不是成功（401/403/5xx …）。
		res.ProbeOutcome = ProbeOutcomeFailed
		res.ProbeSupported = true
		res.ErrorSummary = strings.TrimSpace(fmt.Sprintf("upstream returned HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
		res.Description = "the upstream answered the connectivity probe with a failure status"
	}
	return res, nil
}

// probeHeaders builds the outbound headers of one probe request from the
// deployment's PROTOCOL, using the same adapter the dispatcher uses (T1:
// one auth decision, one place). The credential's auth type selects the
// slot: an api_key/service credential goes to the protocol's key slot, an
// oauth credential to the bearer slot — and anthropic_messages refuses
// oauth outright (its documented support set contains no OAuth vendor).
func (s *ProbeService) probeHeaders(dep *domain.Deployment, token string, cred *domain.Credential) (map[string]string, error) {
	adapter, ok := providers.AdapterForProtocol(dep.Protocol)
	if !ok {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"probe: no adapter is registered for protocol "+string(dep.Protocol))
	}
	authType := ""
	if cred != nil {
		authType = cred.AuthType
	}
	headers, err := adapter.AuthHeaders(providers.Auth{Type: authType, Token: strings.TrimSpace(token)})
	if err != nil {
		return nil, err
	}
	return headers, nil
}

// resolveProviderToken picks the provider's usable credential（active 优先、
// rotating 回退——两者都是可派发状态）并以代次钉扎解出
// 出站 token（oauth 凭据解出的是 access token，密封闭包不出边界）。
func (s *ProbeService) resolveProviderToken(ctx context.Context, providerID string) (string, *domain.Credential, error) {
	creds, err := s.store.ListCredentials(ctx, providerID, 100)
	if err != nil {
		return "", nil, err
	}
	var rotating *domain.Credential
	for i := range creds {
		c := &creds[i]
		switch c.Status {
		case "active":
			pin := c.Generation
			token, cred, err := s.resolver.ResolveBearerToken(ctx, c.ID, &pin)
			return token, cred, err
		case "rotating":
			if rotating == nil {
				rotating = c
			}
		}
	}
	if rotating != nil {
		pin := rotating.Generation
		token, cred, err := s.resolver.ResolveBearerToken(ctx, rotating.ID, &pin)
		return token, cred, err
	}
	return "", nil, domain.NewError(domain.CodeNotFound, "no usable credential for provider "+providerID)
}

// transportErrorSummary 把传输错误归类为 operator 可读摘要（不含 secret；
// 调用方再做一次 secret 替换兜底）。
func (s *ProbeService) transportErrorSummary(pctx context.Context, err error) string {
	if pctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("probe timed out after %s", s.timeout())
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns resolution failed"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	return "transport error: " + err.Error()
}

// safeProbeErrorSummary 只透传 domain.Error 的 Message（operator-safe，无
// cause 链）；非 domain 错误一律折叠为固定前缀——解密/内部失败细节永不外泄。
func safeProbeErrorSummary(prefix string, err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return prefix + ": " + de.Message
	}
	return prefix
}

// redactProbeSecret 兜底脱敏：任何路径下 secret 字节都不得出现在摘要里。
func redactProbeSecret(msg, secret string) string {
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	return msg
}

func capProbeSummary(msg string) string {
	if len(msg) > probeSummaryCap {
		return msg[:probeSummaryCap]
	}
	return msg
}
