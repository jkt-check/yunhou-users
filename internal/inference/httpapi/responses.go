// responses.go — POST /v1/responses（Task 13，OpenAI Responses 客户端面）。
//
// 原生 OpenAI Responses 形状与 SSE 事件序列；错误沿用 /v1 统一原生形状
// （{"error":{message,type,code}}，与 chat 面一致）。闸门与 Chat 完全一致：
// principal → 预占 → 转发 → 结算，本文件只做协议翻译、会话链持久化与
// 原生错误映射。
//
// 会话链（previous_response_id）语义：
//   - 成功响应（store 缺省 true）落 inference_response_chains：截至本轮的
//     完整规范 items transcript + 账户归属 + 实际服务账号；
//   - 引用链的后续请求：transcript 回放（既有上下文 + 新输入），并经
//     routing.SessionBinding 钉住同一上游账号；账号失效走显式 Migrate
//     （网关内），绝不静默换号续接；
//   - 跨客户引用、过期链、store:false 的响应：一律 404（不泄漏存在性）；
//   - transcript 超过 maxResponsesTranscriptBytes 的请求正常服务但不落链
//     （引用它得到明确的 404，不静默丢上下文）。

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/model"
)

// ResponseChainStore is the chain persistence surface; satisfied by
// inference/postgres.Store.
type ResponseChainStore interface {
	InsertResponseChain(ctx context.Context, c *domain.ResponseChain) error
	GetResponseChainForAccount(ctx context.Context, id, billingAccountID string, now time.Time) (*domain.ResponseChain, error)
}

// responsesChainTTL bounds chain-row retention; aligned with the session
// binding TTL so a chain never outlives its pin by far.
const responsesChainTTL = 24 * time.Hour

// maxResponsesTranscriptBytes caps the persisted transcript (abuse surface:
// each chained request re-sends the replay upstream).
const maxResponsesTranscriptBytes = 512 << 10

// ResponsesHandler serves POST /v1/responses.
type ResponsesHandler struct {
	Gateway *gateway.Service
	Chains  ResponseChainStore
	clock   domain.Clock
}

// NewResponsesHandler builds the handler. chains may be nil ONLY when the
// surface must reject previous_response_id entirely (fail closed) — in
// production it is always the inference store.
func NewResponsesHandler(gw *gateway.Service, chains ResponseChainStore, clock domain.Clock) *ResponsesHandler {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &ResponsesHandler{Gateway: gw, Chains: chains, clock: clock}
}

// Create handles POST /v1/responses.
func (h *ResponsesHandler) Create(c *gin.Context) {
	p := CallerPrincipalOf(c)
	if p == nil {
		v1Error(c, http.StatusUnauthorized, "authentication_error", "invalid_api_key",
			"missing caller principal (route mounted without auth)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, v1ChatMaxBodyBytes))
	if err != nil {
		v1Error(c, http.StatusBadRequest, "invalid_request_error", "invalid_input",
			"request body too large or unreadable")
		return
	}
	in, err := providers.ParseResponsesRequest(body)
	if err != nil {
		writeV1DomainError(c, err)
		return
	}
	req := in.Request

	// 会话链解析:引用行必须属于同一客户（跨客户与不存在/过期无差别 404,
	// 不泄漏存在性 —— 与 ownedKey 同模式)。
	var prevChain *domain.ResponseChain
	var sessionKey string
	if in.PreviousResponseID != "" {
		if h.Chains == nil {
			v1Error(c, http.StatusBadRequest, "invalid_request_error", "invalid_input",
				"previous_response_id: response chaining is not available on this deployment")
			return
		}
		prevChain, err = h.Chains.GetResponseChainForAccount(c.Request.Context(),
			in.PreviousResponseID, p.BillingAccountID, h.clock.Now())
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				v1Error(c, http.StatusNotFound, "invalid_request_error", "previous_response_not_found",
					"previous_response_id not found (expired, store:false, or not yours)")
				return
			}
			log.Printf("v1/responses: load chain %s: %v", in.PreviousResponseID, err)
			v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
			return
		}
		// transcript 回放:既有上下文(含此前各轮输入+输出) + 本轮新输入。
		var transcript []json.RawMessage
		if err := json.Unmarshal(prevChain.Transcript, &transcript); err != nil {
			log.Printf("v1/responses: corrupt chain transcript %s: %v", prevChain.ID, err)
			v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
			return
		}
		combined := append(transcript, in.InputItems...)
		replayed, err := providers.ResponsesReplayToChat(combined)
		if err != nil {
			writeV1DomainError(c, err)
			return
		}
		messages := replayed
		if in.Instructions != "" {
			messages = append([]model.ChatMessage{{Role: "system", Content: in.Instructions}}, replayed...)
		}
		req.Messages = messages
		// 回放输入重新参与预占估算(上下文变长,原估算只算了新输入)。
		sessionKey = responsesSessionKey(prevChain.ChainID)
	}

	var outcome *gateway.Outcome
	if sessionKey != "" {
		outcome, err = h.Gateway.ChatCompletionsSticky(c.Request.Context(), p, CallerKeyOf(c),
			domain.ProtocolOpenAIResponses, req, sessionKey)
	} else {
		outcome, err = h.Gateway.ChatCompletions(c.Request.Context(), p, CallerKeyOf(c),
			domain.ProtocolOpenAIResponses, req)
	}
	if err != nil {
		writeV1DomainError(c, err)
		return
	}

	responseID := providers.NewPublicResponseID()
	createdAt := h.clock.Now().Unix()

	// transcript 增量:链式请求的"此前 items"是父链 transcript ++ 本轮新输入。
	newItems := in.InputItems
	if prevChain != nil {
		var transcript []json.RawMessage
		if err := json.Unmarshal(prevChain.Transcript, &transcript); err == nil {
			newItems = append(transcript, in.InputItems...)
		}
	}

	if outcome.Stream == nil {
		payload, outputItems, err := providers.ResponsesFromCompletion(
			outcome.Payload, responseID, outcome.ModelID, createdAt, outcome.Inclusion.CacheReadInInput)
		if err != nil {
			log.Printf("v1/responses: translate completion %s: %v", outcome.RequestID, err)
			v1Error(c, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
			return
		}
		if in.Store {
			h.persistChain(responseID, prevChain, p, outcome, append(newItems, outputItems...))
		}
		c.Data(http.StatusOK, "application/json", payload)
		return
	}
	h.relayStream(c, outcome, in, responseID, createdAt, prevChain, newItems)
}

// relayStream relays the translated Responses SSE stream. response.completed
// / response.incomplete are the only clean ends; an upstream break has
// ALREADY been rendered by the translator as a native `event: error` frame.
func (h *ResponsesHandler) relayStream(c *gin.Context, outcome *gateway.Outcome, in *providers.ResponsesInput,
	responseID string, createdAt int64, prevChain *domain.ResponseChain, newItems []json.RawMessage) {
	stream := outcome.Stream
	defer stream.Close()

	src := noCloseBody{stream}
	translated, state := providers.TranslateOpenAIToResponsesStream(
		src, responseID, outcome.ModelID, createdAt, outcome.Inclusion.CacheReadInInput)
	defer translated.Close()

	c.Status(http.StatusOK)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")

	rc := http.NewResponseController(c.Writer)
	if err := rc.SetWriteDeadline(time.Now().Add(v1StreamWriteTimeout)); err != nil {
		log.Printf("v1/responses: set write deadline: %v", err)
	}

	buf := make([]byte, 32<<10)
	end := gateway.EndCompleted
	for {
		n, readErr := translated.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				end = gateway.EndClientGone
				break
			}
			c.Writer.Flush()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if !stream.Terminal() {
					end = gateway.EndUpstreamBroke
				}
			} else if c.Request.Context().Err() != nil {
				end = gateway.EndClientGone
			} else {
				end = gateway.EndUpstreamBroke
			}
			break
		}
	}
	if err := stream.Finish(end); err != nil {
		log.Printf("v1/responses: finish %s: %v", outcome.RequestID, err)
	}

	// 只有完整结束的轮次才可被后续 previous_response_id 引用(半截上下文
	// 不得成为会话状态)。结算独立于客户端连接,链持久化同理(detached)。
	if in.Store && state.Completed() {
		h.persistChain(responseID, prevChain, CallerPrincipalOf(c), outcome,
			append(newItems, state.FinalOutputItems()...))
	}
}

// persistChain writes the chain row (and, for a chain's first turn, binds
// the session to the serving account). Persistence failure degrades the
// chain (later references 404) but never fails the served response — it is
// logged loudly instead.
func (h *ResponsesHandler) persistChain(responseID string, prevChain *domain.ResponseChain,
	p *domain.Principal, outcome *gateway.Outcome, items []json.RawMessage) {
	raw, err := json.Marshal(items)
	if err != nil {
		log.Printf("v1/responses: marshal transcript %s: %v", responseID, err)
		return
	}
	if len(raw) > maxResponsesTranscriptBytes {
		log.Printf("v1/responses: transcript %d bytes exceeds cap; chain row NOT persisted (id=%s)",
			len(raw), responseID)
		return
	}
	chainID := responseID
	if prevChain != nil {
		chainID = prevChain.ChainID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	row := &domain.ResponseChain{
		ID: responseID, ChainID: chainID, BillingAccountID: p.BillingAccountID,
		RequestID: outcome.RequestID, ModelID: outcome.ModelID,
		UpstreamAccountID: outcome.AccountID,
		Transcript:        raw, ExpiresAt: h.clock.Now().Add(responsesChainTTL),
	}
	if err := h.Chains.InsertResponseChain(ctx, row); err != nil {
		log.Printf("ERROR v1/responses: persist chain %s: %v", responseID, err)
		return
	}
	if prevChain == nil {
		// 链首:绑定会话到本轮实际服务的账号(会话键与调度侧一致带前缀)。
		// 冲突 = 并发首链已绑(亲和性而非正确性),记日志即可。
		if err := h.Gateway.BindSession(ctx, responsesSessionKey(chainID), outcome.ModelID, outcome.AccountID); err != nil {
			log.Printf("v1/responses: bind session %s: %v", chainID, err)
		}
	}
}

// responsesSessionKey is the session-binding key of one chain (prefixed so
// the binding namespace can never collide with another protocol's keys).
func responsesSessionKey(chainID string) string { return "respchain:" + chainID }
