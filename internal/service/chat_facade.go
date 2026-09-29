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
	"sync/atomic"
	"time"

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
	snapshots    gateway.SnapshotSource
	defaultModel string

	// lastNotReadyLog 节流「服务未就绪」ERROR 日志（unix 秒，≤1 条/30s），
	// 避免目录故障期每个 /chat 请求刷一行。
	lastNotReadyLog atomic.Int64
}

// NewChatGatewayFacade builds the facade. defaultModel is the public model
// id applied when the client sends no model (旧无 model 默认); a default
// missing from the published catalog makes every call fail as not-ready
// (503, R7-N1 — misconfiguration is loud, not silently routed). cat backs
// AllowedModels' listing (GET /chat/models contract); snapshots is the
// readiness-gate/snapshot pin source — the shared SnapshotCache, so cold
// start carries ErrNoVerifiedSnapshot and transient refresh failures keep
// serving the last verified snapshot (评审修复 C1/I1：不得绕开 cache 直接
// 打 store，否则哨兵不可达且每请求全量 SELECT+ParseSnapshot)。
func NewChatGatewayFacade(gw *gateway.Service, resolver *access.Resolver, cat *catalog.Service, snapshots gateway.SnapshotSource, defaultModel string) *ChatGatewayFacade {
	return &ChatGatewayFacade{gw: gw, resolver: resolver, catalog: cat, snapshots: snapshots, defaultModel: defaultModel}
}

// StreamChat implements the /chat service surface. The request always
// streams (SSE), exactly like the multi-model ChatService path. logicalModel
// is the optional ChatRequest.Model (旧客户端不带 → defaultModel). Per the
// ChatStreamer contract the resolved route rides along on error too, so the
// handler's audit line carries the effective model (modelID == 客户端原值
// when the client supplied one — that semantic is unchanged).
func (f *ChatGatewayFacade) StreamChat(ctx context.Context, userID, appID, logicalModel string, messages []model.ChatMessage, tools []json.RawMessage, thinkingEnabled *bool) (*http.Response, *ChatRoute, error) {
	modelID := logicalModel
	if modelID == "" {
		modelID = f.defaultModel
	}
	route := &ChatRoute{LogicalModel: modelID, Provider: "inference", UpstreamModel: modelID}
	resp, err := f.streamChatModel(ctx, userID, logicalModel, messages, tools, thinkingEnabled)
	if err != nil {
		return nil, route, err
	}
	return resp, route, nil
}

// RecordUsage is a no-op for the facade: the inference gateway settles
// metered usage internally (Task 9 transactional settlement); writing
// llm_usage_events here would double-meter.
func (f *ChatGatewayFacade) RecordUsage(ctx context.Context, userID, appID string, route *ChatRoute, status string, inputTokens, outputTokens int) {
}

// AllowedModels backs GET /chat/models from the inference catalog — same
// judgment as /v1/models (published ∩ entitled); N4a: a user without a model
// billing account gets ErrChatNoAccess (403, 对齐 legacy accessPlan)，不再
// 返回空列表。注意：生产上开关打开时 GET /chat/models 由 router 挂载到
// KayaModelsHandler，本方法返回的原始哨兵透传是 dead path（仅 legacy
// handler 路径或测试触达）。
func (f *ChatGatewayFacade) AllowedModels(ctx context.Context, userID, appID string) ([]ChatModelInfo, error) {
	p, err := f.resolver.ResolveUserSession(ctx, userID)
	if err != nil {
		// N4a（推翻评审批次7 Minor-5 的空列表设计）：「无计费账户」哨兵 =
		// 无有效订阅 → ErrChatNoAccess（403 对齐 legacy）；其余错误（瞬时
		// DB 故障等）记录日志并透传——吞掉会把故障伪装成权限语义。
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, ErrChatNoAccess
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
	snap, err := f.snapshots.Current(ctx)
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
	// N1/N2 就绪闸门：目录不可用（冷启动）或默认模型不在已发布目录 → 503，
	// 进程不死、不回退 legacy。先于 ResolveUserSession，使「服务未就绪」
	// 与「用户无权限」严格分层。走共享 SnapshotCache：冷启动携带
	// ErrNoVerifiedSnapshot 哨兵，瞬时刷新失败由 cache 兜底旧快照。
	snap, err := f.snapshots.Current(ctx)
	if err != nil {
		f.logNotReadyErr(err) // 结构化 ERROR，30s 节流（评审修复 I2：冷启动不得静默）
		return nil, mapGatewayError(err) // ErrNoVerifiedSnapshot → ErrChatNotReady
	}
	if _, ok := snap.Model(f.defaultModel); !ok {
		f.logNotReady(snap) // 结构化 ERROR，30s 节流，含 model id + 快照 revision
		return nil, ErrChatNotReady
	}
	// Kaya JWT → unified principal (kind=kaya_jwt). A user without a model
	// billing account maps onto the legacy "no access" outcome (403),
	// never an implicit account creation on a read path. 其余错误（瞬时
	// DB 故障等）走 mapGatewayError 透传为 500 类，不得伪装成 403。
	p, err := f.resolver.ResolveUserSession(ctx, userID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, ErrChatNoAccess // 无计费账户 = 无访问权限（403）
		}
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

// logNotReady emits the not-ready ERROR at most once per 30s (目录故障期
// 每个 /chat 请求都会走到这里，不节流会刷爆日志）。结构化 key=value，
// 含默认模型 id 与当前快照 revision，便于运营定位「模型未上架」。
func (f *ChatGatewayFacade) logNotReady(snap *catalog.Snapshot) {
	if !f.notReadyLogDue() {
		return
	}
	log.Printf("ERROR chat facade not ready: default_model=%q revision_id=%d revision=%d",
		f.defaultModel, snap.RevisionID, snap.Revision)
}

// logNotReadyErr 是冷启动/快照不可用路径的同款节流 ERROR（评审修复 I2：
// facade 不再绕过 SnapshotCache 打 store，但该路径仍需运营可见）。
func (f *ChatGatewayFacade) logNotReadyErr(err error) {
	if !f.notReadyLogDue() {
		return
	}
	log.Printf("ERROR chat facade not ready: default_model=%q err=%v", f.defaultModel, err)
}

// notReadyLogDue 30s 节流闸（CAS 抢占，并发下只放行一条）。
func (f *ChatGatewayFacade) notReadyLogDue() bool {
	now := time.Now().Unix()
	last := f.lastNotReadyLog.Load()
	return now-last >= 30 && f.lastNotReadyLog.CompareAndSwap(last, now)
}

// mapGatewayError projects the unified internal error codes onto the legacy
// ErrChat* sentinels, keeping the /chat error shape (envelope + statuses)
// byte-compatible.
func mapGatewayError(err error) error {
	var qe *domain.QuotaExceededError
	if errors.As(err, &qe) {
		return ErrChatRateLimited // 额度不足 → 429 (设计 §9.1)
	}
	if errors.Is(err, catalog.ErrNoVerifiedSnapshot) {
		return ErrChatNotReady // 目录冷启动无快照 → 503 (R7-N1)
	}
	// R7-N4 契约差异修复：网关把上游 HTTP 拒绝包成 CodeUpstreamUnavailable
	// （failover 语义），若只按 code 映射会丢掉 legacy 的结构化分类——kaya
	// 依赖 data.upstream_code 区分「改写可重试」（context_length_exceeded）
	// 与余额/内容政策拒绝，429 则触发客户端退避。错误链中的
	// *providers.DispatchError 携真实状态码与上游错误体，用与 legacy
	// 完全相同的 classifyUpstreamRejection 规则重建（同一函数，同一
	// 脱敏/截断口径），保证两种模式 envelope 逐字节一致：
	//   - 上游 4xx（≠429，重试耗尽或不可重试）→ ChatUpstreamRejection
	//     （handler 映射 502 + data{upstream_status,code,message}）；
	//   - 上游 429（候选全部限流）→ ErrChatRateLimited（429），对齐 legacy
	//     重试耗尽后的 429 语义。
	var de *providers.DispatchError
	if errors.As(err, &de) && de.StatusCode != 0 {
		if de.StatusCode == http.StatusTooManyRequests {
			return ErrChatRateLimited
		}
		if de.StatusCode >= 400 && de.StatusCode < 500 {
			return classifyUpstreamRejection(de.StatusCode, []byte(de.Body))
		}
	}
	switch domain.CodeOf(err) {
	case domain.CodeNotFound:
		// N4b：目录查无此模型 id → 400 未知模型（picker 回退默认）。
		// 无计费账户的 CodeNotFound 已在 resolver 分支拦截，不到这里。
		return ErrChatUnknownModel
	case domain.CodeModelNotAllowed:
		return ErrChatModelNotAllowed // 模型存在但无权益 → 403（对齐 legacy 文案）
	case domain.CodeInvalidKey:
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
