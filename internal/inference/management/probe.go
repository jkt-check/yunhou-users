// probe.go — deployment 级上游连通性探测（R7-N7："base_url 缺 /v1 → 上线后
// 404" 事故类的永久防线）。on-demand、只读：加载 deployment → 解出其
// provider 的可用凭据 → GET {base_url}/models（Authorization: Bearer，默认
// 10s 上限）→ {ok, status, latency_ms, error_summary}。绝不写库；
// error_summary 永不携带 secret 字节（传输错误串经 secret 替换脱敏，上游
// 响应体只丢弃、永不进摘要）。
//
// 与 workers/upstream_health.go 的分层差异：那里是账号级定时轮询（connector
// 客户端 + 冷却/恢复状态机，会写账号状态）；这里是 deployment 级 on-demand
// 预发布探测，只返回报告、不写任何状态。两者不共享状态机，仅共享 egress
// 防线与 vault 解密面。

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
)

// ProbeStore is the read surface the probe needs; satisfied by
// inference/postgres.Store.
type ProbeStore interface {
	GetDeployment(ctx context.Context, id string) (*domain.Deployment, error)
	ListCredentials(ctx context.Context, providerID string, limit int) ([]domain.Credential, error)
}

// BearerResolver 是 credentials.Service.ResolveBearerToken 的最小面。
// management 不能直接 import credentials（credentials 已依赖本包的审计面，
// 反向即循环依赖），故以接口注入；生产直接传 *credentials.Service。
// pinGeneration 钉住代次（设计 §8：解析与读取之间发生轮换即冲突失败）。
type BearerResolver interface {
	ResolveBearerToken(ctx context.Context, id string, pinGeneration *int64) (string, *domain.Credential, error)
}

// DeploymentProber is the per-deployment probe consumed by the publish
// dry-run report; *ProbeService implements it, tests inject fakes.
type DeploymentProber interface {
	ProbeDeployment(ctx context.Context, id string) (*ProbeResult, error)
}

// ProbeResult 是一次探测的 advisory 结论。Status=0 表示未拿到任何 HTTP
// 响应（DNS 失败/拒绝/超时）；ErrorSummary 永不包含 secret 字节。
type ProbeResult struct {
	DeploymentID string `json:"deployment_id"`
	OK           bool   `json:"ok"`
	Status       int    `json:"status"`
	LatencyMS    int64  `json:"latency_ms"`
	ErrorSummary string `json:"error_summary,omitempty"`
}

// probeBodyCap bounds the discarded /models response body.
const probeBodyCap = 8 << 10

// probeSummaryCap bounds error_summary length.
const probeSummaryCap = 300

// DefaultProbeTimeout 是单次探测的默认上限（brief: 10s）。
const DefaultProbeTimeout = 10 * time.Second

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
// 路径同口径）; client should be providers.NewHTTPClient(egress) in
// production so every dialed IP is validated at connection time（DNS
// rebinding 兜底）, nil falls back to http.DefaultClient.
func NewProbeService(store ProbeStore, resolver BearerResolver, validateURL func(context.Context, string) error, client *http.Client) *ProbeService {
	return &ProbeService{store: store, resolver: resolver, validateURL: validateURL, client: client, Timeout: DefaultProbeTimeout}
}

func (s *ProbeService) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return http.DefaultClient
}

func (s *ProbeService) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultProbeTimeout
}

// ProbeDeployment probes one deployment's upstream. Lookup failures
// (unknown deployment) are hard errors; every probe-level failure is
// reported in-band as {ok:false} — the endpoint itself stays 200.
func (s *ProbeService) ProbeDeployment(ctx context.Context, deploymentID string) (*ProbeResult, error) {
	res := &ProbeResult{DeploymentID: deploymentID}
	dep, err := s.store.GetDeployment(ctx, deploymentID)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(strings.TrimSpace(dep.BaseURL), "/")
	if base == "" {
		res.ErrorSummary = "deployment has no base_url"
		return res, nil
	}
	// SSRF 防线：出站前过 egress 策略（与写路径同一 validator；生产 client
	// 另有 dial 时 IP 校验兜底 DNS rebinding）。拒绝即绝不出站。
	if s.validateURL == nil {
		res.ErrorSummary = "egress validation not configured"
		return res, nil
	}
	if err := s.validateURL(ctx, base); err != nil {
		res.ErrorSummary = capProbeSummary("base_url rejected by egress policy: " + err.Error())
		return res, nil
	}
	token, err := s.resolveProviderToken(ctx, dep.ProviderID)
	if err != nil {
		res.ErrorSummary = safeProbeErrorSummary("credential resolution failed", err)
		return res, nil
	}

	pctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		res.ErrorSummary = capProbeSummary("build probe request: " + err.Error())
		return res, nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	resp, err := s.httpClient().Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.ErrorSummary = capProbeSummary(redactProbeSecret(s.transportErrorSummary(pctx, err), token))
		return res, nil
	}
	defer resp.Body.Close()
	// 响应体只丢弃不引用：上游错误体可能回显请求头，绝不能让 secret 经
	// error_summary 外泄。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyCap))
	res.Status = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.OK = true
		return res, nil
	}
	// 非 2xx（典型事故形状：base_url 缺 /v1 → 404）如实上报真实状态码。
	res.ErrorSummary = strings.TrimSpace(fmt.Sprintf("upstream returned HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	return res, nil
}

// resolveProviderToken picks the provider's usable credential（active 优先、
// rotating 回退——两者都是 ResolveSecret 的可派发状态）并以代次钉扎解出
// bearer token。
func (s *ProbeService) resolveProviderToken(ctx context.Context, providerID string) (string, error) {
	creds, err := s.store.ListCredentials(ctx, providerID, 100)
	if err != nil {
		return "", err
	}
	var rotating *domain.Credential
	for i := range creds {
		c := &creds[i]
		switch c.Status {
		case "active":
			pin := c.Generation
			token, _, err := s.resolver.ResolveBearerToken(ctx, c.ID, &pin)
			return token, err
		case "rotating":
			if rotating == nil {
				rotating = c
			}
		}
	}
	if rotating != nil {
		pin := rotating.Generation
		token, _, err := s.resolver.ResolveBearerToken(ctx, rotating.ID, &pin)
		return token, err
	}
	return "", domain.NewError(domain.CodeNotFound, "no usable credential for provider "+providerID)
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
