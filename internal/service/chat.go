package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yunhou/users/internal/llm"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// chatUpstreamTimeout bounds one chat request end-to-end (headers + SSE
// stream). R6: 5m → 15m — a near-1M-token prompt spends minutes in upstream
// prefill before the first token, and the inference catalog now allows
// output caps up to 384K tokens; 15m covers prefill + a long (though not
// worst-case) generation. Still short enough that a hung upstream can't pin
// a connection forever. The context is bound to the upstream connection, so
// the client disconnecting (gin request ctx cancel) also tears the stream
// down at the transport level.
const chatUpstreamTimeout = 15 * time.Minute

// chatAccessTimeout bounds the pre-upstream phase (subscription + plan DB
// reads). /chat skips the global 20s timeoutMiddleware so the SSE stream can
// run long — but that exemption also leaves these two DB calls without a
// server-side deadline, so a hung Postgres would pin the connection until
// the client gives up.
const chatAccessTimeout = 10 * time.Second

// chatUpstreamErrorBodyCap limits how much of an upstream error body we
// read before discarding — error payloads can be huge and are only used for
// logging.
const chatUpstreamErrorBodyCap = 8 << 10

// chatUpstreamMessageCap bounds the upstream error message surfaced to
// clients (data.upstream_message) and the server-side log via Error(). The
// audit log records only upstream_status/upstream_code — not the message
// (upstream text can echo request content).
const chatUpstreamMessageCap = 300

// defaultChatMaxOutputTokens 是输出 token 硬上限默认值（评审安全补丁：无
// max_tokens 上限时恶意订阅者可开超长生成流造成不受控上游成本，参照
// inference 网关设计 §7.2「不允许无限输出」）。客户端请求（model.ChatRequest）
// 不携带 max_tokens，故该上限直接写入上游 payload，不存在客户端值封顶问题。
// 仅作用于 Anthropic 协议路径（协议必传 max_tokens）；OpenAI 协议路径按
// DualBackend 契约不携带该字段（见 catalog.go：MaxTokens 仅发 Anthropic
// provider），其滥用面由并发流上限与上游超时收敛。
const defaultChatMaxOutputTokens = 8192

// defaultChatMaxStreamsPerUser 是单用户并发流式请求上限默认值（评审安全
// 补丁：legacy /chat 仅有 router 层按 IP 限流，单用户可并发开大量流式请求
// 放大上游成本）。
const defaultChatMaxStreamsPerUser = 8

// ChatRoute describes where one chat request was actually sent. The handler
// uses it for usage metering and the audit log; it is nil on error.
type ChatRoute struct {
	LogicalModel  string // catalog id the client picked (or the default)
	Provider      string
	Protocol      string // llm.ProtocolOpenAI | llm.ProtocolAnthropic
	UpstreamModel string
	InputPerMtok  float64
	OutputPerMtok float64
}

// ChatModelInfo is the public view of one catalog model, returned by
// AllowedModels and served at GET /chat/models for kaya's model picker.
type ChatModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	Default     bool   `json:"default"`
}

// ChatStreamer is the /chat service surface: the multi-model ChatService
// and the inference-gateway facade (ChatGatewayFacade, Task 8) both
// satisfy it. Exported so the inference httpapi can wire the facade
// without importing the handler.
type ChatStreamer interface {
	// StreamChat opens the upstream stream. On error it still returns the
	// resolved route when model resolution succeeded (resp == nil), so the
	// handler's audit line can attribute the failure to the effective
	// model; a nil route means resolution never happened (unknown model)
	// and the handler falls back to the raw client value.
	StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error)
	// RecordUsage meters one completed upstream call; it never fails the
	// request. The facade's implementation is a no-op (the inference
	// gateway settles metered usage internally — double-writing
	// llm_usage_events would duplicate metering).
	RecordUsage(ctx context.Context, userID, appID string, route *ChatRoute, status string, inputTokens, outputTokens int)
	// AllowedModels backs GET /chat/models.
	AllowedModels(ctx context.Context, userID, appID string) ([]ChatModelInfo, error)
}

// ChatService routes POST /chat to the upstream LLM provider selected by the
// request's logical model id. All provider API keys live server-side (the
// llm.Catalog); consumer apps (kaya etc.) never see them — they authenticate
// with a user JWT and the server checks subscription-based access before
// spending upstream tokens. Downstream of this service everything speaks
// OpenAI chat.completion.chunk SSE; Anthropic-protocol providers are
// translated at the boundary (llm.TranslateAnthropicStream).
type ChatService struct {
	catalog    *llm.Catalog // nil = chat disabled (ErrChatNotEnabled)
	pools      map[string]*llm.KeyPool
	subRepo    repo.SubscriptionRepo
	planRepo   repo.PlanRepo
	usageRepo  repo.LLMUsageRepo // nil = metering disabled
	httpClient *http.Client
	// maxOutputTokens / maxStreamsPerUser：两项安全补丁阈值，<=0 时回落默认
	// 值（见 outputTokenCap / streamSlotLimit），保证零值构造也可用。后续由
	// 主装配线通过 setter 接入配置（internal/config 不在本补丁范围内）。
	maxOutputTokens   int
	maxStreamsPerUser int
	slotsMu           sync.Mutex
	slots             map[string]int // userID → 进行中的流式请求数
}

// NewChatService builds the multi-model chat router. catalog nil → every
// call returns ErrChatNotEnabled (handler maps to 404), mirroring the
// empty-webhook-secret convention for disabled channels.
func NewChatService(catalog *llm.Catalog, subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo, usageRepo repo.LLMUsageRepo) *ChatService {
	s := &ChatService{
		catalog:   catalog,
		pools:     map[string]*llm.KeyPool{},
		subRepo:   subRepo,
		planRepo:  planRepo,
		usageRepo: usageRepo,
		// No client-level Timeout: the SSE stream length is bounded by ctx
		// (chatUpstreamTimeout). But the transport gets explicit dial and
		// response-header deadlines so a silently-hung upstream fails in
		// seconds instead of pinning the connection until the 15m ctx fires.
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
			MaxIdleConns:      100,
			// 与 providers/adapter.go 同款：上游集中在少数 host，默认
			// MaxIdleConnsPerHost=2 会让 keep-alive 形同虚设（每请求重做
			// TCP+TLS 握手），抬高首 token 延迟。
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
		slots: map[string]int{},
	}
	// 评审安全补丁（凭证边界）：跨 origin 重定向绝不跟随。Go 的 http.Client
	// 跨主机跳转只剥 Authorization/Cookie，x-api-key 与 Provider.Headers 里
	// 的运营商自定义鉴权头会被逐字拷给新主机（307/308 还会重发完整请求
	// 体）。返回 ErrUseLastResponse 让 3xx 响应按上游错误处理（StreamChat
	// 的非 200 分支）。与 internal/inference/providers/adapter.go 的
	// NewHTTPClient 策略一致。
	s.httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("chat: too many redirects")
		}
		if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	if catalog != nil {
		for name, p := range catalog.Providers {
			s.pools[name] = llm.NewKeyPool(p.APIKeys)
		}
	}
	return s
}

// SetHTTPClient overrides the HTTP client. Tests inject httptest servers here.
func (s *ChatService) SetHTTPClient(c *http.Client) {
	s.httpClient = c
}

// SetMaxOutputTokens 设置输出 token 硬上限（n<=0 时忽略，保持当前值）。
// 仅作用于 Anthropic 协议路径（协议必传 max_tokens）；OpenAI 协议路径按
// DualBackend 契约不携带该字段。后续由主装配线从配置接入。
func (s *ChatService) SetMaxOutputTokens(n int) {
	if n > 0 {
		s.maxOutputTokens = n
	}
}

// SetMaxStreamsPerUser 设置单用户并发流式请求上限（n<=0 时忽略，保持当前值）。
func (s *ChatService) SetMaxStreamsPerUser(n int) {
	if n > 0 {
		s.maxStreamsPerUser = n
	}
}

// outputTokenCap 返回生效的输出 token 硬上限（未配置/零值回落默认值）。
func (s *ChatService) outputTokenCap() int {
	if s.maxOutputTokens > 0 {
		return s.maxOutputTokens
	}
	return defaultChatMaxOutputTokens
}

// streamSlotLimit 返回生效的单用户并发流上限（未配置/零值回落默认值）。
func (s *ChatService) streamSlotLimit() int {
	if s.maxStreamsPerUser > 0 {
		return s.maxStreamsPerUser
	}
	return defaultChatMaxStreamsPerUser
}

// acquireStreamSlot 占用 userID 的一个并发流名额；已达上限返回 ok=false。
// 返回的 release 幂等（sync.Once），调用方可在多个退出路径安全重复调用。
func (s *ChatService) acquireStreamSlot(userID string) (release func(), ok bool) {
	s.slotsMu.Lock()
	if s.slots == nil {
		s.slots = map[string]int{}
	}
	if s.slots[userID] >= s.streamSlotLimit() {
		s.slotsMu.Unlock()
		return nil, false
	}
	s.slots[userID]++
	s.slotsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.slotsMu.Lock()
			if n := s.slots[userID] - 1; n > 0 {
				s.slots[userID] = n
			} else {
				delete(s.slots, userID)
			}
			s.slotsMu.Unlock()
		})
	}, true
}

// sameOrigin 判定两个 URL 是否同 scheme+host（重定向凭证边界）。
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// ErrChatConcurrencyLimited：单用户并发流式请求超过上限（评审安全补丁）。
// ChatConcurrencyLimitError 同时 unwrap 到 ErrChatRateLimited —— handler
// 的 chatErrorMapping 已为它加了独立 case（置于 ErrChatRateLimited 之前，
// handler/chat.go），客户端拿到准确文案的 429。
var ErrChatConcurrencyLimited = errors.New("too many concurrent chat streams")

// ChatConcurrencyLimitError 携带触发拒绝时的上限值，Error() 面向服务端
// 日志；Unwrap 同时匹配 ErrChatConcurrencyLimited 与 ErrChatRateLimited
// （Go 1.20+ 多 unwrap），兼容 handler 现有 429 映射。
type ChatConcurrencyLimitError struct {
	Limit int
}

func (e *ChatConcurrencyLimitError) Error() string {
	return fmt.Sprintf("%s (limit %d)", ErrChatConcurrencyLimited, e.Limit)
}

func (e *ChatConcurrencyLimitError) Unwrap() []error {
	return []error{ErrChatConcurrencyLimited, ErrChatRateLimited}
}

// StreamChat resolves the logical model, checks the caller's subscription
// access (including the plan's model allowlist), then opens a streaming
// request upstream. On success the returned *http.Response carries an
// OpenAI-shaped SSE stream (Anthropic upstreams are translated) and the
// caller owns closing Body. The response body is bound to ctx: cancelling
// ctx (client disconnect) closes the upstream connection and fails the read.
//
// On error after model resolution the resolved route is still returned
// (resp == nil) so the handler's audit line can attribute the failure to
// the effective model; a nil route means resolution never happened
// (unknown model) and the handler falls back to the raw client value.
//
// The access decision mirrors resolvePlanForTokenIssuanceWithPlan: an active
// subscription whose plan is active, whose apps include appID, and whose
// chat_models (when non-NULL) include the resolved model.
func (s *ChatService) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error) {
	if s.catalog == nil {
		return nil, nil, ErrChatNotEnabled
	}
	// 按用户并发流上限（评审安全补丁）：进入即占名额，成功路径把名额转交
	// 给响应 body 的生命周期（见下方成功分支），所有错误路径由 defer 释放。
	release, ok := s.acquireStreamSlot(userID)
	if !ok {
		return nil, nil, &ChatConcurrencyLimitError{Limit: s.streamSlotLimit()}
	}
	slotHeld := true
	defer func() {
		if slotHeld {
			release()
		}
	}()
	resolvedID, m, ok := s.catalog.Resolve(logicalModel)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrChatUnknownModel, logicalModel)
	}
	provider := s.catalog.Providers[m.Provider]
	route := &ChatRoute{
		LogicalModel:  resolvedID,
		Provider:      m.Provider,
		Protocol:      provider.Protocol,
		UpstreamModel: m.UpstreamModel,
		InputPerMtok:  m.InputPerMtok,
		OutputPerMtok: m.OutputPerMtok,
	}

	// Bound the gate separately from the stream: /chat is exempt from the
	// global request timeout (see chatAccessTimeout), so without this a hung
	// DB would hold the request open indefinitely.
	accessCtx, accessCancel := context.WithTimeout(ctx, chatAccessTimeout)
	accessErr := s.checkAccess(accessCtx, userID, appID, resolvedID, time.Now())
	accessCancel()
	if accessErr != nil {
		return nil, route, accessErr
	}

	var body []byte
	var err error
	// 评审安全补丁（无 max_tokens 上限）：客户端请求不携带 max_tokens
	// （model.ChatRequest 无此字段）。Anthropic 协议必传 max_tokens，故该
	// 路径写入硬上限；OpenAI 协议路径按 DualBackend 契约（e2e
	// chat_gateway_test 钉死：legacy upstream payload 不得携带
	// max_tokens）与 catalog 设计（MaxTokens 仅发 Anthropic provider）
	// 不注入该字段，滥用面由并发流上限 + 上游超时 + 计量收费收敛。
	outCap := s.outputTokenCap()
	switch provider.Protocol {
	case llm.ProtocolAnthropic:
		// Anthropic API 要求必传 max_tokens：目录里的 operator 配置值
		// （m.MaxTokens）被 honored，但以硬上限封顶；缺省或超上限一律用
		// 硬上限。
		maxTok := m.MaxTokens
		if maxTok <= 0 || maxTok > outCap {
			maxTok = outCap
		}
		body, err = llm.BuildAnthropicPayload(m.UpstreamModel, maxTok, outCap, messages, tools, thinkingEnabled)
		if err != nil {
			// Anthropic translation only fails on un-encodable client input:
			// the shape guards (history legal per chat validation, forbidden
			// by Anthropic protocol), an unsupported role, or an undecodable
			// tool — the final marshal cannot fail. All of it is a client
			// shape problem, not a server fault: mark it for a 400 mapping.
			return nil, route, fmt.Errorf("encode chat request: %w: %v", ErrChatRequestShape, err)
		}
	default:
		body, err = llm.BuildOpenAIPayload(m.UpstreamModel, messages, tools, thinkingEnabled)
		if err != nil {
			return nil, route, fmt.Errorf("encode chat request: %w", err)
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, chatUpstreamTimeout)
	// NOTE: no defer cancel() here. The context is deliberately bound to the
	// response body's lifetime instead: the transport's readLoop watches
	// reqCtx.Done() and tears down the upstream connection on cancel, so
	// cancelling at function return would cut the SSE stream before the
	// caller (handler) has read anything. cancel fires via the
	// cancelOnCloseBody wrapper on Close, or via the timeout timer if the
	// body is never closed.

	// Retry budget: with multiple keys, one 429/5xx retries on another key
	// (the rejected key is cooled). A single-key provider never retries.
	pool := s.pools[m.Provider]
	attempts := 1
	if pool.Len() > 1 {
		attempts = 2
	}

	for attempt := 0; attempt < attempts; attempt++ {
		keyIdx, key := pool.Acquire(time.Now())
		resp, err := s.doUpstream(reqCtx, provider, key, body)
		if err != nil {
			cancel()
			return nil, route, fmt.Errorf("%w: %v", ErrChatUpstreamError, err)
		}
		if resp.StatusCode == http.StatusOK {
			if provider.Protocol == llm.ProtocolAnthropic {
				resp.Body = llm.TranslateAnthropicStream(resp.Body)
			}
			// 名额随 body 生命周期释放：cancelOnCloseBody 在 Close 时同时
			// cancel ctx 并释放 slot（与现有包装模式一致）；watcher 兜底
			// body 未关闭但 ctx 已取消的路径（客户端断连 / 15m 超时），
			// release 幂等，两条路径同时触发也安全。
			slotHeld = false
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: func() {
				cancel()
				release()
			}}
			go func() {
				<-reqCtx.Done()
				release()
			}()
			return resp, route, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, chatUpstreamErrorBodyCap))
		resp.Body.Close()
		// 429/5xx may be a per-key quota or a transient provider issue: cool
		// the key and, if another key exists, retry once transparently.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			pool.Cool(keyIdx, time.Now().Add(llm.KeyCooldown))
			if attempt+1 < attempts {
				continue
			}
		}
		cancel()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, route, fmt.Errorf("%w (status %d): %s", ErrChatRateLimited, resp.StatusCode, errBody)
		}
		// Upstream 4xx (other than 429) rejects the request itself — a
		// permanent error that retrying the same bytes will never fix. The
		// structured rejection lets the client distinguish a
		// retryable-by-rewrite cause (context length) from billing and
		// content-policy ones.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, route, classifyUpstreamRejection(resp.StatusCode, errBody)
		}
		return nil, route, fmt.Errorf("%w (status %d): %s", ErrChatUpstreamError, resp.StatusCode, errBody)
	}
	// Unreachable: the loop's last attempt always returns. Kept so the
	// compiler sees a terminating statement.
	cancel()
	return nil, route, ErrChatUpstreamError
}

// doUpstream performs one HTTP call against the provider. Path and auth
// headers follow the protocol: OpenAI providers get POST
// {base_url}/chat/completions with Bearer auth; Anthropic providers get POST
// {base_url}/v1/messages with both Bearer and x-api-key (Kimi/GLM-style
// endpoints accept Bearer, stock Anthropic wants x-api-key) plus the
// required anthropic-version header. Provider.Headers are applied last so an
// operator can override any default.
func (s *ChatService) doUpstream(ctx context.Context, p llm.Provider, apiKey string, body []byte) (*http.Response, error) {
	path := "/chat/completions"
	if p.Protocol == llm.ProtocolAnthropic {
		path = "/v1/messages"
	}
	// TrimSuffix: an operator-set base_url with a trailing slash would
	// otherwise produce "...//chat/completions" and a confusing upstream 404.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(p.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if p.Protocol == llm.ProtocolAnthropic {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	return s.httpClient.Do(req)
}

// RecordUsage meters one completed upstream call. Metering must never break
// the chat path: insert errors are logged and swallowed. route nil (or a nil
// usageRepo) is a no-op. Cost identity: µ¥ = tokens × price_per_mtok.
func (s *ChatService) RecordUsage(ctx context.Context, userID, appID string, route *ChatRoute, status string, inputTokens, outputTokens int) {
	if s.usageRepo == nil || route == nil {
		return
	}
	ev := model.LLMUsageEvent{
		UserID:        userID,
		AppID:         appID,
		Model:         route.LogicalModel,
		Provider:      route.Provider,
		UpstreamModel: route.UpstreamModel,
		Status:        status,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		CostMicros:    int64(float64(inputTokens)*route.InputPerMtok + float64(outputTokens)*route.OutputPerMtok),
	}
	if err := s.usageRepo.InsertEvent(ctx, ev); err != nil {
		log.Printf("chat: record usage (user=%s model=%s): %v", userID, route.LogicalModel, err)
	}
}

// AllowedModels returns the catalog models the caller's plan may use, for
// GET /chat/models (kaya's model picker). The subscription gate is the same
// as StreamChat's, minus the per-model check.
func (s *ChatService) AllowedModels(ctx context.Context, userID, appID string) ([]ChatModelInfo, error) {
	if s.catalog == nil {
		return nil, ErrChatNotEnabled
	}
	accessCtx, accessCancel := context.WithTimeout(ctx, chatAccessTimeout)
	plan, err := s.accessPlan(accessCtx, userID, appID, time.Now())
	accessCancel()
	if err != nil {
		return nil, err
	}
	allowed := func(id string) bool {
		return len(plan.ChatModels) == 0 || slices.Contains(plan.ChatModels, id)
	}
	out := make([]ChatModelInfo, 0, len(s.catalog.Models))
	for _, id := range s.catalog.ModelIDs() {
		if !allowed(id) {
			continue
		}
		m := s.catalog.Models[id]
		name := m.DisplayName
		if name == "" {
			name = id
		}
		out = append(out, ChatModelInfo{ID: id, DisplayName: name, Provider: m.Provider, Default: id == s.catalog.DefaultModel})
	}
	return out, nil
}

// cancelOnCloseBody runs cancel exactly once, when Close is called. The
// context timeout still fires on its own if the body is never closed.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// accessPlan is the shared subscription gate: active subscription, not
// expired, plan active, plan.apps contains appID. It returns the plan so
// callers can apply model-allowlist checks on top.
func (s *ChatService) accessPlan(ctx context.Context, userID, appID string, now time.Time) (*model.Plan, error) {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrChatNoAccess
		}
		return nil, fmt.Errorf("get subscription: %w", err)
	}
	if sub == nil {
		return nil, ErrChatNoAccess
	}
	// NULL expires_at means "never expires" (pre-2026-07-27 rows); a
	// non-nil past expiry is an expired subscription.
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(now) {
		return nil, ErrChatNoAccess
	}

	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return nil, fmt.Errorf("get plan: %w", err)
	}
	if !plan.IsActive || !slices.Contains(plan.Apps, appID) {
		return nil, ErrChatNoAccess
	}
	return plan, nil
}

// checkAccess is the subscription gate plus the per-plan model allowlist
// (plans.chat_models; NULL/empty = unrestricted).
func (s *ChatService) checkAccess(ctx context.Context, userID, appID, logicalModel string, now time.Time) error {
	plan, err := s.accessPlan(ctx, userID, appID, now)
	if err != nil {
		return err
	}
	if len(plan.ChatModels) > 0 && !slices.Contains(plan.ChatModels, logicalModel) {
		return ErrChatModelNotAllowed
	}
	return nil
}

// classifyUpstreamRejection builds the structured detail for an upstream 4xx
// (≠429): real status, a normalized code, and the sanitized upstream message.
// Classification precedence: billing (402 or balance keywords) → context
// length → content policy → generic invalid_request.
func classifyUpstreamRejection(status int, body []byte) *ChatUpstreamRejection {
	msg, code := extractUpstreamError(body)
	rej := &ChatUpstreamRejection{
		Status:  status,
		Code:    UpstreamCodeInvalidRequest,
		Message: capUpstreamMessage(msg),
	}
	hay := strings.ToLower(code + " " + msg)
	switch {
	case status == http.StatusPaymentRequired ||
		strings.Contains(hay, "insufficient") ||
		strings.Contains(hay, "balance") ||
		strings.Contains(msg, "余额"):
		rej.Code = UpstreamCodeInsufficientBalance
	case strings.Contains(hay, "context_length_exceeded") ||
		strings.Contains(hay, "context length") ||
		strings.Contains(hay, "maximum context") ||
		strings.Contains(hay, "context window") ||
		strings.Contains(hay, "prompt is too long") ||
		strings.Contains(hay, "too many tokens"):
		rej.Code = UpstreamCodeContextLengthExceeded
	case strings.Contains(hay, "content_filter") ||
		strings.Contains(hay, "content filter") ||
		strings.Contains(hay, "content exists risk") ||
		strings.Contains(hay, "content management") ||
		strings.Contains(hay, "moderation") ||
		strings.Contains(hay, "sensitive"):
		rej.Code = UpstreamCodeContentFilter
	}
	return rej
}

// extractUpstreamError pulls message + code out of the common upstream error
// shapes: OpenAI/DeepSeek {"error":{"message","code"}} (also Anthropic's
// {"type":"error","error":{...}}), a bare {"error":"string"}, or — for
// non-JSON bodies and message-less error objects — the raw text.
func extractUpstreamError(body []byte) (message, code string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", ""
	}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &env); err == nil && len(env.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(env.Error, &obj); err == nil {
			if obj.Message == "" {
				// An error object without a message (e.g. only "type") —
				// surface the raw body rather than nothing.
				return string(trimmed), ""
			}
			return obj.Message, obj.Code
		}
		var s string
		if err := json.Unmarshal(env.Error, &s); err == nil {
			return s, ""
		}
	}
	return string(trimmed), ""
}

// capUpstreamMessage bounds the message at chatUpstreamMessageCap bytes,
// cutting on a UTF-8 boundary (the "…" marker may push the total a few bytes
// past the cap). Invalid UTF-8 (GBK error pages, stray gateway bytes) is
// replaced first — otherwise validation of the whole cut would discard the
// entire message.
func capUpstreamMessage(msg string) string {
	msg = strings.TrimSpace(strings.ToValidUTF8(msg, ""))
	if len(msg) <= chatUpstreamMessageCap {
		return msg
	}
	cut := msg[:chatUpstreamMessageCap]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
