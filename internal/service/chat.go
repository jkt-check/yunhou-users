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
	"slices"
	"strings"
	"time"

	"github.com/yunhou/users/internal/llm"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// chatUpstreamTimeout bounds one chat request end-to-end (headers + SSE
// stream). Long enough for a full streamed answer; short enough that a hung
// upstream can't pin a connection forever. The context is bound to the
// upstream connection, so the client disconnecting (gin request ctx cancel)
// also tears the stream down at the transport level.
const chatUpstreamTimeout = 5 * time.Minute

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
		// seconds instead of pinning the connection until the 5m ctx fires.
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
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

// StreamChat resolves the logical model, checks the caller's subscription
// access (including the plan's model allowlist), then opens a streaming
// request upstream. On success the returned *http.Response carries an
// OpenAI-shaped SSE stream (Anthropic upstreams are translated) and the
// caller owns closing Body. The response body is bound to ctx: cancelling
// ctx (client disconnect) closes the upstream connection and fails the read.
//
// The access decision mirrors resolvePlanForTokenIssuanceWithPlan: an active
// subscription whose plan is active, whose apps include appID, and whose
// chat_models (when non-NULL) include the resolved model.
func (s *ChatService) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error) {
	if s.catalog == nil {
		return nil, nil, ErrChatNotEnabled
	}
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
		return nil, nil, accessErr
	}

	var body []byte
	var err error
	switch provider.Protocol {
	case llm.ProtocolAnthropic:
		body, err = llm.BuildAnthropicPayload(m.UpstreamModel, m.MaxTokens, messages, tools, thinkingEnabled)
		if err != nil {
			// Anthropic translation only fails on un-encodable client input:
			// the shape guards (history legal per chat validation, forbidden
			// by Anthropic protocol), an unsupported role, or an undecodable
			// tool — the final marshal cannot fail. All of it is a client
			// shape problem, not a server fault: mark it for a 400 mapping.
			return nil, nil, fmt.Errorf("encode chat request: %w: %v", ErrChatRequestShape, err)
		}
	default:
		body, err = llm.BuildOpenAIPayload(m.UpstreamModel, messages, tools, thinkingEnabled)
		if err != nil {
			return nil, nil, fmt.Errorf("encode chat request: %w", err)
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
			return nil, nil, fmt.Errorf("%w: %v", ErrChatUpstreamError, err)
		}
		if resp.StatusCode == http.StatusOK {
			if provider.Protocol == llm.ProtocolAnthropic {
				resp.Body = llm.TranslateAnthropicStream(resp.Body)
			}
			// Bind cancel to the body's lifetime (see NOTE above).
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
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
			return nil, nil, fmt.Errorf("%w (status %d): %s", ErrChatRateLimited, resp.StatusCode, errBody)
		}
		// Upstream 4xx (other than 429) rejects the request itself — a
		// permanent error that retrying will never fix.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, nil, fmt.Errorf("%w (status %d): %s", ErrChatUpstreamRejected, resp.StatusCode, errBody)
		}
		return nil, nil, fmt.Errorf("%w (status %d): %s", ErrChatUpstreamError, resp.StatusCode, errBody)
	}
	// Unreachable: the loop's last attempt always returns. Kept so the
	// compiler sees a terminating statement.
	cancel()
	return nil, nil, ErrChatUpstreamError
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
