package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

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
// the client gives up. 10s matches the OAuth-provider timeout headroom used
// elsewhere.
const chatAccessTimeout = 10 * time.Second

// chatUpstreamErrorBodyCap limits how much of an upstream error body we
// read before discarding — error payloads can be huge and are only used for
// logging.
const chatUpstreamErrorBodyCap = 8 << 10

// ChatService proxies POST /chat to an OpenAI-compatible chat.completions
// endpoint (DeepSeek) with the server's own API key. Consumer apps (kaya
// etc.) never see the key — they authenticate with a user JWT and the
// server checks subscription-based access before spending upstream tokens.
type ChatService struct {
	apiKey     string // server-side DeepSeek API key; empty = chat disabled
	baseURL    string // OpenAI-compatible origin, e.g. https://api.deepseek.com
	model      string // upstream model name, e.g. deepseek-v4-flash
	subRepo    repo.SubscriptionRepo
	planRepo   repo.PlanRepo
	httpClient *http.Client
}

// NewChatService builds the chat proxy. apiKey empty → every call returns
// ErrChatNotEnabled (handler maps to 404), mirroring the empty-webhook-
// secret convention for disabled channels.
func NewChatService(apiKey, baseURL, model string, subRepo repo.SubscriptionRepo, planRepo repo.PlanRepo) *ChatService {
	return &ChatService{
		apiKey:     apiKey,
		baseURL:    baseURL,
		model:      model,
		subRepo:    subRepo,
		planRepo:   planRepo,
		httpClient: &http.Client{Timeout: 0}, // no client-level cap: SSE stream length is bounded by ctx
	}
}

// SetHTTPClient overrides the HTTP client. Tests inject httptest servers here.
func (s *ChatService) SetHTTPClient(c *http.Client) {
	s.httpClient = c
}

// SetBaseURL overrides the upstream origin. Tests point this at an httptest stub.
func (s *ChatService) SetBaseURL(u string) {
	s.baseURL = u
}

// StreamChat checks the caller's subscription access for appID, then opens a
// streaming chat.completions request upstream. On success the returned
// *http.Response carries the SSE stream (Content-Type: text/event-stream) and
// the caller owns closing Body. The response body is bound to ctx: cancelling
// ctx (client disconnect) closes the upstream connection and fails the read.
//
// The access decision mirrors resolvePlanForTokenIssuanceWithPlan: an active
// subscription whose plan is active and whose apps include appID. Errors are
// the ErrChat* sentinels (mapped by the handler) plus wrapped repo errors
// (500 at the handler).
//
// tools / thinkingEnabled are relayed verbatim into the upstream payload:
//   - tools (OpenAI-compatible tool schema list) → `tools` field, so the
//     upstream model can emit tool_calls the client executes locally
//     (kaya's run_shell / list_dir etc.). Bounds are validated by the
//     handler (model.ChatMaxTools / ChatMaxToolsBytes) before any spend.
//   - thinkingEnabled=true → `thinking: {"type": "enabled"}` (DeepSeek
//     reasoning mode). nil/false → omitted, upstream default behavior.
func (s *ChatService) StreamChat(ctx context.Context, userID, appID string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error) {
	if s.apiKey == "" {
		return nil, ErrChatNotEnabled
	}
	// Bound the gate separately from the stream: /chat is exempt from the
	// global request timeout (see chatAccessTimeout), so without this a hung
	// DB would hold the request open indefinitely. cancel fires when
	// checkAccess returns — it does NOT outlive this function.
	accessCtx, accessCancel := context.WithTimeout(ctx, chatAccessTimeout)
	accessErr := s.checkAccess(accessCtx, userID, appID, time.Now())
	accessCancel()
	if accessErr != nil {
		return nil, accessErr
	}

	reqCtx, cancel := context.WithTimeout(ctx, chatUpstreamTimeout)
	// NOTE: no defer cancel() here. The context is deliberately bound to the
	// response body's lifetime instead: the transport's readLoop watches
	// reqCtx.Done() and tears down the upstream connection on cancel, so
	// cancelling at function return would cut the SSE stream before the
	// caller (handler) has read anything. cancel fires via the
	// cancelOnCloseBody wrapper on Close, or via the timeout timer if the
	// body is never closed.

	payload := map[string]any{
		"model":    s.model,
		"messages": messages,
		"stream":   true,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if thinkingEnabled != nil && *thinkingEnabled {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("encode chat request: %w", err)
	}

	// TrimSuffix: an operator-set DEEPSEEK_BASE_URL with a trailing slash
	// would otherwise produce "...//chat/completions" and a confusing
	// upstream 404 instead of a config-time correction.
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimSuffix(s.baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrChatUpstreamError, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() {
			cancel()
			resp.Body.Close()
		}()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, chatUpstreamErrorBodyCap))
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w (status %d): %s", ErrChatRateLimited, resp.StatusCode, errBody)
		}
		return nil, fmt.Errorf("%w (status %d): %s", ErrChatUpstreamError, resp.StatusCode, errBody)
	}
	// Bind cancel to the body's lifetime instead of this function's return:
	// the transport's readLoop watches reqCtx.Done() and would tear down
	// the upstream connection the moment StreamChat returns (before the
	// caller has read a single SSE chunk). With the wrapper, cancel fires
	// when the caller closes the body (handler defer) or when the 5m
	// timeout expires — whichever comes first.
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
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

// checkAccess is the subscription gate: active subscription, not expired,
// plan active, plan.apps contains appID. Same decision matrix as the JWT
// issuance path (auth.go resolvePlanForTokenIssuanceWithPlan) so a token
// that carries has_access=true can always chat, and vice versa.
func (s *ChatService) checkAccess(ctx context.Context, userID, appID string, now time.Time) error {
	sub, err := s.subRepo.FindActiveByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrChatNoAccess
		}
		return fmt.Errorf("get subscription: %w", err)
	}
	if sub == nil {
		return ErrChatNoAccess
	}
	// NULL expires_at means "never expires" (pre-2026-07-27 rows); a
	// non-nil past expiry is an expired subscription.
	if sub.ExpiresAt != nil && sub.ExpiresAt.Before(now) {
		return ErrChatNoAccess
	}

	plan, err := s.planRepo.FindByID(ctx, sub.PlanID)
	if err != nil {
		return fmt.Errorf("get plan: %w", err)
	}
	if !plan.IsActive || !slices.Contains(plan.Apps, appID) {
		return ErrChatNoAccess
	}
	return nil
}
