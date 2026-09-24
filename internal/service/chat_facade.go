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
	"log"
	"net/http"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/model"
)

// ChatGatewayFacade serves the legacy /chat shape from the inference
// gateway. It satisfies the chat handler's ChatStreamer interface.
type ChatGatewayFacade struct {
	gw           *gateway.Service
	resolver     *access.Resolver
	catalog      *catalog.Service
	defaultModel string
}

// NewChatGatewayFacade builds the facade. defaultModel is the public model
// id applied when the client sends no model (旧无 model 默认); an empty
// default makes every call fail as no-access (misconfiguration is loud, not
// silently routed). cat backs AllowedModels (GET /chat/models contract).
func NewChatGatewayFacade(gw *gateway.Service, resolver *access.Resolver, cat *catalog.Service, defaultModel string) *ChatGatewayFacade {
	return &ChatGatewayFacade{gw: gw, resolver: resolver, catalog: cat, defaultModel: defaultModel}
}

// StreamChat implements the /chat service surface. The request always
// streams (SSE), exactly like the multi-model ChatService path. logicalModel
// is the optional ChatRequest.Model (旧客户端不带 → defaultModel).
func (f *ChatGatewayFacade) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error) {
	resp, err := f.streamChatModel(ctx, userID, logicalModel, messages, tools, thinkingEnabled)
	if err != nil {
		return nil, nil, err
	}
	modelID := logicalModel
	if modelID == "" {
		modelID = f.defaultModel
	}
	return resp, &ChatRoute{LogicalModel: modelID, Provider: "inference", UpstreamModel: modelID}, nil
}

// RecordUsage is a no-op for the facade: the inference gateway settles
// metered usage internally (Task 9 transactional settlement); writing
// llm_usage_events here would double-meter.
func (f *ChatGatewayFacade) RecordUsage(ctx context.Context, userID, appID string, route *ChatRoute, status string, inputTokens, outputTokens int) {
}

// AllowedModels backs GET /chat/models from the inference catalog — same
// judgment as /v1/models (published ∩ entitled); a user without a model
// billing account gets an empty list, not an error (picker UX).
func (f *ChatGatewayFacade) AllowedModels(ctx context.Context, userID, appID string) ([]ChatModelInfo, error) {
	p, err := f.resolver.ResolveUserSession(ctx, userID)
	if err != nil {
		// 评审批次7 Minor-5：仅「无计费账户」哨兵是正常态（picker 显示空
		// 列表）；其余错误（瞬时 DB 故障等）记录日志并透传——吞掉会把
		// 故障伪装成「该用户无可用模型」。
		if domain.CodeOf(err) == domain.CodeNotFound {
			return []ChatModelInfo{}, nil
		}
		log.Printf("chat facade: resolve user session for models (user=%s): %v", userID, err)
		return nil, err
	}
	allowed, err := f.resolver.AuthorizedModelIDs(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	allowSet := make(map[string]bool, len(allowed))
	for _, id := range allowed {
		allowSet[id] = true
	}
	models, err := f.catalog.ListPublishedModels(ctx,
		func(_ context.Context, modelID string) (bool, error) { return allowSet[modelID], nil })
	if err != nil {
		return nil, err
	}
	snap, err := f.catalog.LoadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ChatModelInfo, 0, len(models))
	for _, m := range models {
		provider := ""
		if deps := snap.ActiveDeployments(m.ID); len(deps) > 0 {
			if pv, ok := snap.Providers[deps[0].ProviderID]; ok {
				provider = pv.Code
			}
		}
		out = append(out, ChatModelInfo{
			ID: m.ID, DisplayName: m.DisplayName, Provider: provider,
			Default: m.ID == f.defaultModel,
		})
	}
	return out, nil
}

func (f *ChatGatewayFacade) streamChatModel(ctx context.Context, userID, modelOverride string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, error) {
	// Kaya JWT → unified principal (kind=kaya_jwt). A user without a model
	// billing account maps onto the legacy "no access" outcome (403),
	// never an implicit account creation on a read path. 其余错误（瞬时
	// DB 故障等）走 mapGatewayError 透传为 500 类，不得伪装成 403。
	p, err := f.resolver.ResolveUserSession(ctx, userID)
	if err != nil {
		return nil, mapGatewayError(err)
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
	case domain.CodeModelNotAllowed, domain.CodeInvalidKey, domain.CodeNotFound:
		// 无权益/模型未授权/默认模型未上架 → 旧的"无访问权限"语义。
		return ErrChatNoAccess
	case domain.CodeQuotaExceeded, domain.CodeRateLimited, domain.CodeInsufficientCapacity:
		return ErrChatRateLimited
	case domain.CodeInvalidInput:
		return ErrChatUpstreamRejected
	default:
		// CodeUnpricedCapability（运营定价配置错误）等也落这里——那是
		// 服务端配置缺陷，呈现为 500 类而不是 403「无权限」（评审批次
		// 7 Minor-4：与真实权益拒绝区分开，运营才能定位）。
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
