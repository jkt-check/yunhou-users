// chat_facade.go — Kaya /chat facade onto the inference gateway (Task 8).
//
// The legacy /chat contract is preserved verbatim: JWT-authenticated,
// OpenAI-shaped SSE relay, no model field required (the configured default
// model applies), tools/thinking toggles relayed, and the ErrChat* sentinel
// error surface (the handler's {code,data,message} envelope + status
// mapping is untouched). Whether /chat runs through this facade at all is
// the migration switch INFERENCE_KAYA_CHAT_GATEWAY (default off = legacy
// passthrough); see cmd/server.
//
// The facade converts the server-verified Kaya JWT identity into the
// gateway's uniform principal (kind=kaya_jwt) via access.Resolver.
// ResolveUserSession — the gateway never branches on the outer credential.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/model"
)

// ChatGatewayFacade serves the legacy /chat shape from the inference
// gateway. It satisfies the chat handler's streamer interface.
type ChatGatewayFacade struct {
	gw           *gateway.Service
	resolver     *access.Resolver
	defaultModel string
}

// NewChatGatewayFacade builds the facade. defaultModel is the public model
// id applied when the client sends no model (旧无 model 默认); an empty
// default makes every call fail as no-access (misconfiguration is loud, not
// silently routed).
func NewChatGatewayFacade(gw *gateway.Service, resolver *access.Resolver, defaultModel string) *ChatGatewayFacade {
	return &ChatGatewayFacade{gw: gw, resolver: resolver, defaultModel: defaultModel}
}

// StreamChat implements the /chat service surface. The request always
// streams (SSE), exactly like the legacy proxy.
func (f *ChatGatewayFacade) StreamChat(ctx context.Context, userID, appID string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error) {
	return f.streamChatModel(ctx, userID, "", messages, tools, thinkingEnabled)
}

// StreamChatWithModel is the model-aware extension the chat handler prefers
// when available: it honors the optional ChatRequest.Model field (旧客户端
// 不带 model → 默认模型; the legacy proxy ignores the field entirely).
func (f *ChatGatewayFacade) StreamChatWithModel(ctx context.Context, userID, modelOverride string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error) {
	return f.streamChatModel(ctx, userID, modelOverride, messages, tools, thinkingEnabled)
}

func (f *ChatGatewayFacade) streamChatModel(ctx context.Context, userID, modelOverride string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error) {
	// Kaya JWT → unified principal (kind=kaya_jwt). A user without a model
	// billing account maps onto the legacy "no access" outcome (403),
	// never an implicit account creation on a read path.
	p, err := f.resolver.ResolveUserSession(ctx, userID)
	if err != nil {
		return nil, ErrChatNoAccess
	}
	modelID := modelOverride
	if modelID == "" {
		modelID = f.defaultModel
	}
	outcome, err := f.gw.ChatCompletions(ctx, p, nil, domain.ProtocolKayaChat, &providers.ChatRequest{
		Model:           modelID,
		Messages:        messages,
		Stream:          true,
		Tools:           tools,
		ThinkingEnabled: thinkingEnabled,
	})
	if err != nil {
		return nil, mapGatewayError(err)
	}

	body := &kayaFacadeBody{stream: outcome.Stream}
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}, nil
}

// mapGatewayError projects the unified internal error codes onto the legacy
// ErrChat* sentinels, keeping the /chat error shape (envelope + statuses)
// byte-compatible.
func mapGatewayError(err error) error {
	var qe *domain.QuotaExceededError
	if errors.As(err, &qe) {
		return ErrChatRateLimited // 额度不足 → 429 (设计 §9.1)
	}
	switch domain.CodeOf(err) {
	case domain.CodeModelNotAllowed, domain.CodeInvalidKey, domain.CodeNotFound, domain.CodeUnpricedCapability:
		// 无权益/模型未授权/默认模型未上架 → 旧的"无访问权限"语义。
		return ErrChatNoAccess
	case domain.CodeQuotaExceeded, domain.CodeRateLimited, domain.CodeInsufficientCapacity:
		return ErrChatRateLimited
	case domain.CodeInvalidInput:
		return ErrChatUpstreamRejected
	default:
		return ErrChatUpstreamError
	}
}

// kayaFacadeBody adapts the gateway's stream body to the *http.Response the
// legacy handler relays. Two contract points:
//
//   - an EOF WITHOUT a terminal [DONE] surfaces as ErrUnexpectedEOF, so the
//     handler injects its upstream-broke error event (kaya renders a missing
//     [DONE] as failure, never as a completed answer);
//   - Close finalizes settlement with the end state the relay observed
//     (client disconnect keeps the usage already read — 设计 §7.2).
type kayaFacadeBody struct {
	stream *gateway.StreamBody
	sawEOF bool
	broke  bool
}

func (b *kayaFacadeBody) Read(p []byte) (int, error) {
	n, err := b.stream.Read(p)
	if err == io.EOF {
		if b.stream.Terminal() {
			b.sawEOF = true
			return n, err
		}
		b.broke = true
		if n > 0 {
			return n, nil // deliver the tail bytes; the next read reports the break
		}
		return 0, io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		b.broke = true
	}
	return n, err
}

func (b *kayaFacadeBody) Close() error {
	end := gateway.EndClientGone
	switch {
	case b.sawEOF:
		end = gateway.EndCompleted
	case b.broke:
		end = gateway.EndUpstreamBroke
	}
	// Finish before Close: Close's own finalize is a once-guarded no-op
	// afterwards.
	_ = b.stream.Finish(end)
	return b.stream.Close()
}
