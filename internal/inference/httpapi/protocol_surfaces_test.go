// protocol_surfaces_test.go — Task 13 协议面 handler 级测试：Messages /
// Responses 端点走完整鉴权链与网关（真实库 + 真实 httptest 上游），断言
// 协议原生形状、错误映射、x-api-key 凭据头、会话链持久化与跨客户隔离。
// skip 不算通过。

package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/gateway"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
	"github.com/yunhou/users/internal/inference/quota"
	"github.com/yunhou/users/internal/inference/routing"
)

// protoFixture mirrors v1Fixture but mounts the Task 13 surfaces and adds an
// Anthropic-protocol upstream deployment, so Messages/Responses can be
// exercised against BOTH upstream wire protocols.
type protoFixture struct {
	*v1Fixture
	respHandler *httpapi.ResponsesHandler
	gw          *gateway.Service
	routingSvc  *routing.Service
	resolver    *access.Resolver
	keySvc      *access.KeyService
	provID      string
	upAcctID    string
	lastBodies  chan []byte
}

func newProtoFixture(t *testing.T) *protoFixture {
	t.Helper()
	f := &protoFixture{v1Fixture: newV1Fixture(t), lastBodies: make(chan []byte, 8)}
	store := f.store
	f.resolver = access.NewResolver(store, nil)
	f.keySvc = access.NewKeyService(store, nil)

	// 让 glm-4.6 声明支持全部三个客户端协议(协议门:modelSpeaks)。
	if _, err := f.db.Exec(`UPDATE inference_models
		SET protocols = ARRAY['openai_chat','anthropic_messages','openai_responses']::text[]
		WHERE id = $1`, f.modelID); err != nil {
		t.Fatal(err)
	}

	// 记录上游请求体(供 transcript 回放断言)。
	f.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case f.lastBodies <- body:
		default:
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			for _, c := range []string{
				"data: {\"id\":\"chatcmpl-p1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n",
				"data: {\"id\":\"chatcmpl-p1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n",
				"data: {\"id\":\"chatcmpl-p1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n",
				"data: [DONE]\n\n",
			} {
				_, _ = io.WriteString(w, c)
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-p1ns","object":"chat.completion","created":1,"model":"glm-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	})

	var provID, acctID string
	if err := f.db.QueryRow(`SELECT provider_id, id FROM inference_upstream_accounts LIMIT 1`).Scan(&provID, &acctID); err != nil {
		t.Fatal(err)
	}
	f.upAcctID = acctID
	f.provID = provID

	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       providers.NewOpenAIChat(),
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	f.routingSvc = routing.NewService(store, adapters, nil)
	f.gw = gateway.NewService(staticSnap{f.protocolSnapshot()}, store,
		access.NewEntitlementResolver(store, nil), quota.NewService(store, nil),
		f.routingSvc, mustCredSvc(t, store), providers.NewHTTPClient(egress), egress, nil)
	f.gw.SetSessionBinder(routing.NewSessionBinder(store, nil))

	f.respHandler = httpapi.NewResponsesHandler(f.gw, store, nil)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(f.resolver))
	// 三个协议入口同挂一个引擎：T6 的协议矩阵需要逐入口对比同一组限制。
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(f.gw).Create)
	v1.POST("/messages", httpapi.NewMessagesHandler(f.gw).Create)
	v1.POST("/responses", f.respHandler.Create)
	f.engine = engine
	return f
}

// protocolSnapshot rebuilds the static snapshot with the model serving all
// three client protocols and the deployment map from newV1Fixture.
func (f *protoFixture) protocolSnapshot() *catalog.Snapshot {
	base := &catalog.Snapshot{
		Models: map[string]domain.Model{
			f.modelID: {
				ID: f.modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
				ContextTokens: 200000, MaxOutputTokens: 8192,
				Protocols: []domain.Protocol{
					domain.ProtocolOpenAIChat, domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses,
				},
				InputModalities: []string{"text"}, OutputModalities: []string{"text"},
				SupportsTools: true, SupportsReasoning: true,
			},
		},
		Providers:     map[string]domain.Provider{},
		Deployments:   map[string]domain.Deployment{},
		RoutesByModel: map[string][]domain.ModelRoute{},
	}
	ctx := context.Background()
	// The provider must be in the snapshot with its PUBLISHED status — a
	// published revision always carries every referenced provider, and a
	// disabled provider takes its deployments out of rotation. Load it
	// through the store's typed row mapper (domain.Provider has no db
	// tags, so a raw `SELECT *` into it would silently fail).
	if p, err := f.store.GetProvider(ctx, f.provID); err == nil {
		base.Providers[p.ID] = *p
	}
	deps, err := f.store.ListDeployments(ctx, domain.DeploymentFilter{})
	if err != nil {
		return base
	}
	for _, d := range deps {
		// 只路由 loopback 运行时部署 —— v1Fixture 还建了一个指向
		// upstream.example.com 的"永不拨号"发布占位部署,不能进调度。
		if !strings.Contains(d.BaseURL, f.upstream.URL) {
			continue
		}
		base.Deployments[d.ID] = d
		base.RoutesByModel[f.modelID] = append(base.RoutesByModel[f.modelID],
			domain.ModelRoute{ModelID: f.modelID, DeploymentID: d.ID, Weight: 1, Enabled: true})
	}
	return base
}

// addUser inserts a bare user row (API-key path never needs a JWT).
func (f *protoFixture) addUser(t *testing.T) string {
	t.Helper()
	userID := uuid.NewString()
	if _, err := f.db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return userID
}

func mustCredSvc(t *testing.T, store *postgres.Store) *credentials.Service {
	t.Helper()
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return credentials.NewService(vault, store, store)
}

// ---------------------------------------------------------------------------
// Messages 面
// ---------------------------------------------------------------------------

// 非流式原生形状 + 错误形状 + x-api-key 凭据头(Anthropic SDK 默认)。
func TestV1Messages_NonStreamNativeShapeAndXAPIKey(t *testing.T) {
	f := newProtoFixture(t)

	// x-api-key 凭据头(Anthropic 风格)必须可用。
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"glm-4.6","max_tokens":64,
	  "messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Api-Key", f.keyPlain)
	req.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var msg struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("anthropic client decode: %v (%s)", err, w.Body.String())
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.StopReason != "end_turn" {
		t.Errorf("msg = %+v", msg)
	}
	if msg.Model != "glm-4.6" {
		t.Errorf("model must be the PUBLIC id, got %q", msg.Model)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("id = %q, want msg_*", msg.ID)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Hello" {
		t.Errorf("content = %+v", msg.Content)
	}
	if msg.Usage.InputTokens != 7 || msg.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", msg.Usage)
	}

	// 无凭据 → Anthropic 原生错误形状(type:error 信封)。
	w = f.call(t, http.MethodPost, "/v1/messages", "", map[string]any{
		"model": "glm-4.6", "max_tokens": 1,
		"messages": []map[string]any{{"role": "user", "content": "x"}},
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", w.Code)
	}
	var errBody struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil ||
		errBody.Type != "error" || errBody.Error.Type != "authentication_error" {
		t.Errorf("error shape = %s", w.Body.String())
	}
}

// 流式:Anthropic SSE 事件序列 + message_stop 终止 + 已结算。
func TestV1Messages_StreamEvents(t *testing.T) {
	f := newProtoFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"glm-4.6","max_tokens":64,"stream":true,
	  "messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+f.keyPlain)
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var events []string
	var text strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "text_delta") {
			var ev struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				text.WriteString(ev.Delta.Text)
			}
		}
	}
	seq := strings.Join(events, ",")
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(seq, want) {
			t.Errorf("missing event %q in %s", want, seq)
		}
	}
	if !strings.HasSuffix(seq, "message_stop") {
		t.Errorf("last event must be message_stop: %s", seq)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	// 与 chat 面同一结算链:17 micros。
	var settled int64
	if err := f.db.QueryRow(`SELECT settled_micros FROM inference_requests ORDER BY created_at DESC LIMIT 1`).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != 17 {
		t.Errorf("settled = %d, want 17", settled)
	}
}

// 能力拒绝走 Anthropic 原生 400(多模态块),且零上游/账本痕迹。
func TestV1Messages_ExplicitRejection(t *testing.T) {
	f := newProtoFixture(t)
	w := f.call(t, http.MethodPost, "/v1/messages", f.keyPlain, map[string]any{
		"model": "glm-4.6", "max_tokens": 64,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":   "image",
				"source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"},
			}},
		}},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var errBody struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil ||
		errBody.Type != "error" || errBody.Error.Type != "invalid_request_error" ||
		!strings.Contains(errBody.Error.Message, "image") {
		t.Errorf("error = %s", w.Body.String())
	}
	var cnt int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_requests`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("requests = %d, want 0 (rejection precedes admission)", cnt)
	}
}

// ---------------------------------------------------------------------------
// Responses 面
// ---------------------------------------------------------------------------

// 非流式原生形状 + 会话链落库 + 引用回放(transcript 到达上游)+ 粘性绑定。
func TestV1Responses_NonStreamChainAndReplay(t *testing.T) {
	f := newProtoFixture(t)

	// 第一轮:新链。
	w := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6",
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "remember 42"}},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("responses client decode: %v (%s)", err, w.Body.String())
	}
	if resp.Object != "response" || resp.Status != "completed" || resp.Model != "glm-4.6" {
		t.Errorf("resp = %+v", resp)
	}
	if !strings.HasPrefix(resp.ID, "resp_") {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// 链行:账户归属 + 上游账号归属落库;粘性绑定指向服务账号。
	var chain struct {
		ChainID    string `db:"chain_id"`
		Account    string `db:"billing_account_id"`
		UpstreamID string `db:"upstream_account_id"`
		ModelID    string `db:"model_id"`
	}
	if err := f.db.Get(&chain, `SELECT chain_id, billing_account_id, upstream_account_id, model_id
		FROM inference_response_chains WHERE id = $1`, resp.ID); err != nil {
		t.Fatalf("chain row missing: %v", err)
	}
	if chain.Account != f.account.ID || chain.UpstreamID != f.upAcctID || chain.ChainID != resp.ID {
		t.Errorf("chain = %+v", chain)
	}
	var bindAcct string
	if err := f.db.QueryRow(`SELECT account_id FROM inference_session_bindings
		WHERE session_key = $1 AND status = 'active'`, "respchain:"+resp.ID).Scan(&bindAcct); err != nil {
		t.Fatalf("session binding missing: %v", err)
	}
	if bindAcct != f.upAcctID {
		t.Errorf("binding account = %s, want %s", bindAcct, f.upAcctID)
	}

	// 第二轮:引用 previous_response_id —— transcript 回放必须到达上游
	// (上游请求体含第一轮用户输入),且仍路由到绑定账号。
	w2 := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "previous_response_id": resp.ID,
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "what did I say?"}},
		}},
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("chained code = %d body = %s", w2.Code, w2.Body.String())
	}
	var upstreamBody []byte
	for i := 0; i < 8; i++ {
		select {
		case b := <-f.lastBodies:
			if strings.Contains(string(b), "what did I say?") {
				upstreamBody = b
			}
		default:
		}
		if upstreamBody != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if upstreamBody == nil {
		t.Fatal("upstream never saw the chained request")
	}
	if !strings.Contains(string(upstreamBody), "remember 42") {
		t.Errorf("transcript replay missing from upstream payload: %s", upstreamBody)
	}
	// 第二轮链行共享同一 chain_id。
	var chainID2 string
	var resp2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT chain_id FROM inference_response_chains WHERE id = $1`, resp2.ID).Scan(&chainID2); err != nil {
		t.Fatal(err)
	}
	if chainID2 != resp.ID {
		t.Errorf("chain_id = %s, want %s", chainID2, resp.ID)
	}
}

// 跨客户隔离:另一客户的 Key 引用 resp id → 404(与不存在无差别)。
func TestV1Responses_CrossCustomerIsolation(t *testing.T) {
	f := newProtoFixture(t)
	w := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "input": "hello",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// 第二个客户(独立 billing account + 独立 Key + 同模型权益)。
	userID := f.addUser(t)
	ctx := context.Background()
	acct2, err := f.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct2.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{f.modelID},
		PolicyVersionID: f.policyID, AnchorAt: time.Now().Add(-time.Minute),
		EffectiveFrom: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	created, err := f.keySvc.CreateKey(ctx, userID, access.CreateParams{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	w2 := f.call(t, http.MethodPost, "/v1/responses", created.Plaintext, map[string]any{
		"model": "glm-4.6", "previous_response_id": resp.ID, "input": "continue",
	})
	if w2.Code != http.StatusNotFound {
		t.Fatalf("cross-customer reference = %d, want 404 (body=%s)", w2.Code, w2.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &errBody); err != nil ||
		errBody.Error.Code != "previous_response_not_found" {
		t.Errorf("error = %s", w2.Body.String())
	}
}

// store:false 不落链,后续引用 404(OpenAI 语义)。
func TestV1Responses_StoreFalseNotReferenceable(t *testing.T) {
	f := newProtoFixture(t)
	w := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "input": "hello", "store": false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_response_chains WHERE id = $1`, resp.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("store:false must not persist a chain row (n=%d)", n)
	}
	w2 := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "previous_response_id": resp.ID, "input": "continue",
	})
	if w2.Code != http.StatusNotFound {
		t.Fatalf("referencing a store:false response = %d, want 404", w2.Code)
	}
}

// 绑定账号失效 → 显式 Migrate(不静默换号):新账号服务,旧绑定
// ended/migrated,新绑定 active。
func TestV1Responses_BoundAccountInvalidMigratesExplicitly(t *testing.T) {
	f := newProtoFixture(t)
	ctx := context.Background()

	w := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "input": "first",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// 第二个上游账号(独立凭据 —— (provider_id, credential_id) 唯一)。
	credSvc2 := mustCredSvc(t, f.store)
	cv2, err := credSvc2.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		f.provID, "second", "api_key", "sk-up-2", "seed2", nil)
	if err != nil {
		t.Fatal(err)
	}
	acct2 := &domain.UpstreamAccount{
		ProviderID: f.provID, CredentialID: cv2.ID,
		Status: domain.AccountActive, ConcurrencyLimit: 8,
	}
	if err := f.store.InsertUpstreamAccount(ctx, acct2); err != nil {
		t.Fatal(err)
	}
	// 账号 1 失效(reauth 传播的终态)。
	if _, err := f.db.Exec(`UPDATE inference_upstream_accounts SET status='reauth_required' WHERE id=$1`, f.upAcctID); err != nil {
		t.Fatal(err)
	}

	w2 := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "previous_response_id": resp.ID, "input": "second",
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("migrated call = %d body = %s", w2.Code, w2.Body.String())
	}
	// 旧绑定 ended(migrated),新绑定 active → acct2。
	var oldStatus, oldReason string
	if err := f.db.QueryRow(`SELECT status, ended_reason FROM inference_session_bindings
		WHERE session_key = $1 AND account_id = $2`, "respchain:"+resp.ID, f.upAcctID).Scan(&oldStatus, &oldReason); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "ended" || oldReason != "migrated" {
		t.Errorf("old binding = %s/%s, want ended/migrated", oldStatus, oldReason)
	}
	var newAcct string
	if err := f.db.QueryRow(`SELECT account_id FROM inference_session_bindings
		WHERE session_key = $1 AND status='active'`, "respchain:"+resp.ID).Scan(&newAcct); err != nil {
		t.Fatal(err)
	}
	if newAcct != acct2.ID {
		t.Errorf("new binding account = %s, want %s", newAcct, acct2.ID)
	}
	// 第二轮的实际服务账号 = 新账号(归属落链)。
	var resp2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	var servedBy string
	if err := f.db.QueryRow(`SELECT upstream_account_id FROM inference_response_chains WHERE id=$1`, resp2.ID).Scan(&servedBy); err != nil {
		t.Fatal(err)
	}
	if servedBy != acct2.ID {
		t.Errorf("served by %s, want migrated account %s", servedBy, acct2.ID)
	}
}

// 流式:Responses SSE 事件序列 + response.completed。
func TestV1Responses_StreamEvents(t *testing.T) {
	f := newProtoFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
	  "model":"glm-4.6","stream":true,
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	req.Header.Set("Authorization", "Bearer "+f.keyPlain)
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var events []string
	var text strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "output_text.delta") {
			var ev struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				text.WriteString(ev.Delta)
			}
		}
	}
	seq := strings.Join(events, ",")
	if !strings.HasPrefix(seq, "response.created,response.in_progress") {
		t.Errorf("stream must open with response.created/in_progress: %s", seq)
	}
	if !strings.HasSuffix(seq, "response.completed") {
		t.Errorf("stream must end with response.completed: %s", seq)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	// 流式完成轮也落链(store 缺省 true)。
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_response_chains`).Scan(&n); err != nil || n != 1 {
		t.Errorf("chains = %d, want 1", n)
	}
}

// 能力拒绝:background → 400(OpenAI 原生错误形状),零痕迹。
func TestV1Responses_ExplicitRejection(t *testing.T) {
	f := newProtoFixture(t)
	w := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "input": "x", "background": true,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil ||
		errBody.Error.Code != "invalid_input" || !strings.Contains(errBody.Error.Message, "background") {
		t.Errorf("error = %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// T8 — 可执行能力矩阵：客户端入口 × 上游协议 × 文本/工具/图片/思考回放
//
// 每一格要么支持（200 + 上游真的收到请求），要么明确拒绝（协议原生 400 +
// 零准入 + 零派发）。另含 SSE 终止矩阵（完整结束 / 半截 EOF）。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Fixture: one model, one deployment on a chosen upstream protocol, all three
// client entries mounted.
// ---------------------------------------------------------------------------

// matrixFixture serves the three client entries against ONE upstream
// protocol, so the matrix can hold "client entry × upstream protocol"
// apart. The upstream speaks the deployment's protocol; the client surfaces
// always speak their own.
type matrixFixture struct {
	*v1Fixture
	engine *gin.Engine
	proto  domain.Protocol
	// upCalls counts upstream requests; upBodies records their bodies.
	upCalls  atomic.Int32
	upBodies chan []byte
	// upstreamHandler overrides the default answer when set.
	upstreamHandler atomic.Value // func(w http.ResponseWriter, r *http.Request, body []byte)
	// truncate makes the upstream end its SSE stream WITHOUT the protocol's
	// terminal frame (半截 EOF).
	truncate atomic.Bool
}

func newMatrixFixture(t *testing.T, proto domain.Protocol) *matrixFixture {
	t.Helper()
	fx := newV1Fixture(t)
	f := &matrixFixture{v1Fixture: fx, proto: proto, upBodies: make(chan []byte, 32)}

	// The model must serve all three CLIENT protocols (the client-protocol
	// gate is the model's protocol list, not the upstream's).
	if _, err := fx.db.Exec(`UPDATE inference_models
		SET protocols = ARRAY['openai_chat','anthropic_messages','openai_responses']::text[]
		WHERE id = $1`, fx.modelID); err != nil {
		t.Fatal(err)
	}
	var provID string
	if err := fx.db.QueryRow(`SELECT provider_id FROM inference_deployments LIMIT 1`).Scan(&provID); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.upCalls.Add(1)
		select {
		case f.upBodies <- body:
		default:
		}
		if h, ok := f.upstreamHandler.Load().(func(http.ResponseWriter, *http.Request, []byte)); ok {
			h(w, r, body)
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			f.writeUpstreamStream(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.upstreamNonStreamBody())
	}))
	t.Cleanup(srv.Close)

	dep := &domain.Deployment{
		ProviderID: provID, UpstreamModel: "matrix-upstream", BaseURL: srv.URL,
		Protocol:       proto,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := fx.store.InsertDeployment(context.Background(), dep); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	if err := fx.store.InsertRoute(context.Background(), &domain.ModelRoute{
		ModelID: fx.modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true,
		PoolStrategy: domain.PoolRoundRobin,
	}); err != nil {
		t.Fatalf("insert route: %v", err)
	}

	snap := &catalog.Snapshot{
		Models: map[string]domain.Model{fx.modelID: {
			ID: fx.modelID, DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192,
			Protocols: []domain.Protocol{
				domain.ProtocolOpenAIChat, domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses,
			},
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
			SupportsTools: true, SupportsReasoning: true,
		}},
		Providers:     map[string]domain.Provider{},
		Deployments:   map[string]domain.Deployment{dep.ID: *dep},
		RoutesByModel: map[string][]domain.ModelRoute{fx.modelID: {{ModelID: fx.modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true}}},
	}
	if p, err := fx.store.GetProvider(context.Background(), provID); err == nil {
		snap.Providers[p.ID] = *p
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.1/8"})
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       providers.NewOpenAIChat(),
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
		domain.ProtocolOpenAIResponses:  providers.NewOpenAIResponses(),
	}
	routingSvc := routing.NewService(fx.store, adapters, nil)
	gw := gateway.NewService(staticSnap{snap}, fx.store,
		access.NewEntitlementResolver(fx.store, nil), quota.NewService(fx.store, nil),
		routingSvc, mustCredSvc(t, fx.store), providers.NewHTTPClient(egress), egress, nil)
	gw.SetSessionBinder(routing.NewSessionBinder(fx.store, nil))

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	resolver := access.NewResolver(fx.store, nil)
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(resolver))
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(gw).Create)
	v1.POST("/messages", httpapi.NewMessagesHandler(gw).Create)
	v1.POST("/responses", httpapi.NewResponsesHandler(gw, fx.store, nil).Create)
	f.engine = engine
	return f
}

// upstreamNonStreamBody is the non-streaming answer of the deployment's
// upstream protocol.
func (f *matrixFixture) upstreamNonStreamBody() string {
	switch f.proto {
	case domain.ProtocolAnthropicMessage:
		return `{"id":"msg_1","type":"message","role":"assistant","model":"up",` +
			`"content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":7,"output_tokens":5}}`
	case domain.ProtocolOpenAIResponses:
		return `{"id":"resp_1","object":"response","status":"completed","model":"up",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],` +
			`"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}`
	default:
		return `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
	}
}

// writeUpstreamStream writes the upstream SSE of the deployment's protocol.
// With truncate set the terminal frame is omitted (半截 EOF).
func (f *matrixFixture) writeUpstreamStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	var frames []string
	switch f.proto {
	case domain.ProtocolAnthropicMessage:
		frames = []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":7}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
	case domain.ProtocolOpenAIResponses:
		frames = []string{
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"in_progress\"}}\n\n",
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total_tokens\":12}}}\n\n",
		}
	default:
		frames = []string{
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n",
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n",
			"data: [DONE]\n\n",
		}
	}
	if f.truncate.Load() {
		frames = frames[:len(frames)-1] // drop the terminal frame → 半截 EOF
	}
	for _, fr := range frames {
		_, _ = io.WriteString(w, fr)
		if fl != nil {
			fl.Flush()
		}
	}
}

// call issues one request against the matrix engine.
func (f *matrixFixture) call(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+f.keyPlain)
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// lastUpstreamBody returns the most recent upstream request body.
func (f *matrixFixture) lastUpstreamBody(t *testing.T) string {
	t.Helper()
	select {
	case b := <-f.upBodies:
		return string(b)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream saw no request")
		return ""
	}
}

// countRows counts one inference table (zero-admission assertions).
func (f *matrixFixture) countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Request builders per client entry
// ---------------------------------------------------------------------------

// entryBodies returns one request body per capability probe, shaped for the
// given client entry. A nil body means "not applicable to this entry".
func entryBodies(entry, capability string) map[string]any {
	switch entry {
	case "chat":
		switch capability {
		case "text":
			return map[string]any{"model": "glm-4.6", "messages": []any{
				map[string]any{"role": "user", "content": "hi"}}}
		case "tools":
			return map[string]any{"model": "glm-4.6", "messages": []any{
				map[string]any{"role": "user", "content": "hi"}},
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": "run_shell", "parameters": map[string]any{"type": "object"}}}}}
		case "images":
			return map[string]any{"model": "glm-4.6", "messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}}}}}
		case "thinking_replay":
			return map[string]any{"model": "glm-4.6", "messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": "ok", "reasoning_content": "step-1"}}}
		}
	case "messages":
		switch capability {
		case "text":
			return map[string]any{"model": "glm-4.6", "max_tokens": 64, "messages": []any{
				map[string]any{"role": "user", "content": "hi"}}}
		case "tools":
			return map[string]any{"model": "glm-4.6", "max_tokens": 64, "messages": []any{
				map[string]any{"role": "user", "content": "hi"}},
				"tools": []any{map[string]any{"name": "run_shell", "input_schema": map[string]any{"type": "object"}}}}
		case "images":
			return map[string]any{"model": "glm-4.6", "max_tokens": 64, "messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "AAAA"}}}}}}
		case "thinking_replay":
			// A signed thinking block NEXT TO a text block: the turn carries
			// replayable content, so the accept-and-drop path is the one under
			// test (a turn with ONLY thinking was already rejected).
			return map[string]any{"model": "glm-4.6", "max_tokens": 64, "messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "text", "text": "ok"},
					map[string]any{"type": "thinking", "thinking": "step-1", "signature": "sig-abc"}}}}}
		}
	case "responses":
		switch capability {
		case "text":
			return map[string]any{"model": "glm-4.6", "input": "hi"}
		case "tools":
			return map[string]any{"model": "glm-4.6", "input": "hi",
				"tools": []any{map[string]any{"type": "function", "name": "run_shell",
					"parameters": map[string]any{"type": "object"}}}}
		case "images":
			return map[string]any{"model": "glm-4.6", "input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}}}}
		case "thinking_replay":
			return map[string]any{"model": "glm-4.6", "input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "hi"}}},
				map[string]any{"type": "reasoning", "id": "rs_1",
					"summary": []any{map[string]any{"type": "summary_text", "text": "step-1"}}}}}
		}
	}
	return nil
}

func entryPath(entry string) string {
	switch entry {
	case "chat":
		return "/v1/chat/completions"
	case "messages":
		return "/v1/messages"
	default:
		return "/v1/responses"
	}
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

// TestCapabilityMatrix_ClientEntryByUpstreamProtocol is the executable
// capability matrix: every cell is either "supported" (200 + the upstream
// really received the request) or "rejected" (protocol-native 400 + zero
// admission + zero dispatch). Nothing may be silently degraded.
//
// Rejected cells in this matrix:
//   - images on every /v1 entry: the standard surfaces stay an explicit TEXT
//     subset (the Kaya /chat surface is the only image-capable entry);
//   - thinking replay on the messages entry (signed thinking cannot be
//     replayed faithfully) and on the responses entry (reasoning items carry
//     no encrypted content this gateway can replay).
func TestCapabilityMatrix_ClientEntryByUpstreamProtocol(t *testing.T) {
	entries := []string{"chat", "messages", "responses"}
	protocols := []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses,
	}
	capabilities := []string{"text", "tools", "images", "thinking_replay"}

	for _, proto := range protocols {
		for _, entry := range entries {
			for _, capability := range capabilities {
				name := string(proto) + "/" + entry + "/" + capability
				t.Run(name, func(t *testing.T) {
					f := newMatrixFixture(t, proto)
					body := entryBodies(entry, capability)
					if body == nil {
						t.Skip("capability not applicable to this entry")
					}
					w := f.call(t, http.MethodPost, entryPath(entry), body)
					rejected := (capability == "images") ||
						(capability == "thinking_replay" && entry != "chat")
					if rejected {
						if w.Code != http.StatusBadRequest {
							t.Fatalf("code = %d, want 400（明确拒绝）: %s", w.Code, w.Body.String())
						}
						if n := f.countRows(t, "inference_requests"); n != 0 {
							t.Errorf("requests = %d, want 0 (rejection precedes admission)", n)
						}
						if n := f.countRows(t, "inference_reservations"); n != 0 {
							t.Errorf("reservations = %d, want 0 (rejection leaves no hold)", n)
						}
						if n := f.upCalls.Load(); n != 0 {
							t.Errorf("upstream calls = %d, want 0 (rejection precedes dispatch)", n)
						}
						return
					}
					if w.Code != http.StatusOK {
						t.Fatalf("code = %d, want 200（支持）: %s", w.Code, w.Body.String())
					}
					if n := f.upCalls.Load(); n != 1 {
						t.Errorf("upstream calls = %d, want 1", n)
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// reasoning effort — 逐字保留，不可表达/不支持的值明确拒绝
// ---------------------------------------------------------------------------

// TestV1ChatCompletions_ReasoningEffortIsPreservedUpstream：客户端声明的
// reasoning effort 必须原样到达上游 —— 不得一律折成 medium。
func TestV1ChatCompletions_ReasoningEffortIsPreservedUpstream(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
			w := f.call(t, http.MethodPost, "/v1/chat/completions", map[string]any{
				"model": "glm-4.6", "reasoning_effort": effort,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
			}
			body := f.lastUpstreamBody(t)
			if !strings.Contains(body, effort) {
				t.Errorf("upstream body does not carry the requested effort %q: %s", effort, body)
			}
			if effort != "medium" && strings.Contains(body, `"reasoning_effort":"medium"`) {
				t.Errorf("effort %q was folded to medium: %s", effort, body)
			}
		})
	}
}

// TestV1ChatCompletions_UnsupportedReasoningEffortIsRejected：不支持的 effort
// 值必须明确拒绝（出站前），不得静默忽略。
func TestV1ChatCompletions_UnsupportedReasoningEffortIsRejected(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	w := f.call(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "glm-4.6", "reasoning_effort": "ultra",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
	}
	if n := f.countRows(t, "inference_requests"); n != 0 {
		t.Errorf("requests = %d, want 0 (rejection precedes admission)", n)
	}
	if n := f.upCalls.Load(); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// TestV1Responses_ReasoningEffortIsPreservedUpstream：Responses 入口的
// reasoning.effort 逐字保留（不得折成 medium）。
func TestV1Responses_ReasoningEffortIsPreservedUpstream(t *testing.T) {
	for _, effort := range []string{"low", "high", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
			w := f.call(t, http.MethodPost, "/v1/responses", map[string]any{
				"model": "glm-4.6", "input": "hi",
				"reasoning": map[string]any{"effort": effort},
			})
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
			}
			body := f.lastUpstreamBody(t)
			if !strings.Contains(body, effort) {
				t.Errorf("upstream body does not carry the requested effort %q: %s", effort, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 服务端工具 —— /v1 入口没有服务端执行面，必须原生 400
// ---------------------------------------------------------------------------

// TestV1ChatCompletions_ServerSideToolIsRejected：/v1/chat/completions 的
// tools 只接受 function 定义；服务端工具类型（web_search/computer/
// code_execution/mcp …）没有执行面也没有计量保护，必须明确 400。
func TestV1ChatCompletions_ServerSideToolIsRejected(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	w := f.call(t, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "glm-4.6",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    []any{map[string]any{"type": "web_search"}},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
	}
	if n := f.countRows(t, "inference_requests"); n != 0 {
		t.Errorf("requests = %d, want 0 (rejection precedes admission)", n)
	}
	if n := f.upCalls.Load(); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 思考回放 —— 无法忠实回放的历史必须原生 4xx
// ---------------------------------------------------------------------------

// TestV1Messages_ThinkingRequestIsRejected：Anthropic Messages 面不产出
// 签名思考块（没有无损的签名回放），因此显式开启 thinking 的请求明确
// 400（普通文本/工具流不受影响）。
func TestV1Messages_ThinkingRequestIsRejected(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	w := f.call(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm-4.6", "max_tokens": 2048,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
	}
	if n := f.countRows(t, "inference_requests"); n != 0 {
		t.Errorf("requests = %d, want 0 (rejection precedes admission)", n)
	}
	if n := f.upCalls.Load(); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// TestV1Responses_ReasoningItemIsRejected：Responses 输入里的 reasoning 项
// 无法忠实回放（本网关不实现加密推理存储），必须原生 400 而不是「接受后
// 丢弃」。
func TestV1Responses_ReasoningItemIsRejected(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	w := f.call(t, http.MethodPost, "/v1/responses", map[string]any{
		"model": "glm-4.6", "input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"}}},
			map[string]any{"type": "reasoning", "id": "rs_1",
				"summary": []any{map[string]any{"type": "summary_text", "text": "step-1"}}},
		},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", w.Code, w.Body.String())
	}
	if n := f.countRows(t, "inference_requests"); n != 0 {
		t.Errorf("requests = %d, want 0 (rejection precedes admission)", n)
	}
	if n := f.upCalls.Load(); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// SSE 终止语义 —— 完整结束 / 半截 EOF（矩阵的第四个维度）
// ---------------------------------------------------------------------------

// TestCapabilityMatrix_SSETermination：三个客户端入口 × 三个上游协议的
// 流式终止语义。
//
//   - 完整结束：客户端流必须以**本协议**的终止标记收尾
//     （chat: `data: [DONE]`；messages: `message_stop`；
//     responses: `response.completed`）；
//   - 半截 EOF：上游流在终止标记之前断开 —— 客户端流必须注入**本协议的**
//     原生错误事件，并且**不得**出现终止标记（残缺答案绝不被渲染成完整
//     答案）。
func TestCapabilityMatrix_SSETermination(t *testing.T) {
	entries := []struct {
		name    string
		path    string
		body    map[string]any
		term    string
		errHint string
	}{
		{"chat", "/v1/chat/completions", map[string]any{
			"model": "glm-4.6", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, "data: [DONE]", `"error"`},
		{"messages", "/v1/messages", map[string]any{
			"model": "glm-4.6", "max_tokens": 64, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, "message_stop", "event: error"},
		{"responses", "/v1/responses", map[string]any{
			"model": "glm-4.6", "input": "hi", "stream": true,
		}, "response.completed", "event: error"},
	}
	protocols := []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses,
	}
	for _, proto := range protocols {
		for _, e := range entries {
			t.Run(string(proto)+"/"+e.name+"/complete", func(t *testing.T) {
				f := newMatrixFixture(t, proto)
				w := f.call(t, http.MethodPost, e.path, e.body)
				if w.Code != http.StatusOK {
					t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
				}
				out := w.Body.String()
				if !strings.Contains(out, e.term) {
					t.Errorf("client stream is missing the protocol terminal marker %q: %s", e.term, out)
				}
			})
			t.Run(string(proto)+"/"+e.name+"/truncated_eof", func(t *testing.T) {
				f := newMatrixFixture(t, proto)
				f.truncate.Store(true)
				w := f.call(t, http.MethodPost, e.path, e.body)
				if w.Code != http.StatusOK {
					t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
				}
				out := w.Body.String()
				if strings.Contains(out, e.term) {
					t.Errorf("a truncated upstream stream must NOT produce the terminal marker %q: %s", e.term, out)
				}
				if !strings.Contains(out, e.errHint) {
					t.Errorf("a truncated upstream stream must inject the native error event %q: %s", e.errHint, out)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 网关自己生成的 Messages thinking —— 不得伪造空 signature
// ---------------------------------------------------------------------------

// TestV1Messages_NoFakedThinkingSignature：上游回了推理内容时，Messages
// 面的答复不得输出 `signature: ""` 冒充可回放的原生签名思考块。
func TestV1Messages_NoFakedThinkingSignature(t *testing.T) {
	f := newMatrixFixture(t, domain.ProtocolOpenAIChat)
	// The upstream answers with reasoning_content.
	f.upstreamHandler.Store(func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello","reasoning_content":"step-1"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	})
	w := f.call(t, http.MethodPost, "/v1/messages", map[string]any{
		"model": "glm-4.6", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	if strings.Contains(out, `"signature":""`) || strings.Contains(out, `"signature": ""`) {
		t.Errorf("the gateway faked an empty thinking signature: %s", out)
	}
	if !strings.Contains(out, "Hello") {
		t.Errorf("the ordinary text answer must survive: %s", out)
	}
}

// ---------------------------------------------------------------------------
// T8 — Responses 面的「产出 vs 回放」不对称
//
// 网关在答复里**产出** reasoning 输出项（推理摘要，客户端看得见），但
// **输入**里的 reasoning 项被明确 400（无法忠实回放：本网关不实现 OpenAI
// 的加密推理存储，include=reasoning.encrypted_content 早已被拒）。
//
// 后果（真实客户端风险，必须写进运维文档）：手动接续（客户端把上一轮的
// output items 原样回传进 input）的客户端在**第二轮**就会撞上 400。
// previous_response_id 链不受影响 —— 落链的 transcript 不含 reasoning 项，
// 因此链式回放永远不撞那个 400。
// ---------------------------------------------------------------------------

func TestV1Responses_ReasoningOutputItemDoesNotBreakTheChain(t *testing.T) {
	f := newProtoFixture(t)
	// 上游回推理内容 → 答复里带 reasoning 输出项。
	f.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case f.lastBodies <- body:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-r1","object":"chat.completion","created":1,"model":"glm-upstream",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello","reasoning_content":"plan"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	})

	// 第一轮：答复里必须有 reasoning 输出项（产出侧）。
	w := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6",
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "remember 42"}},
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	sawReasoning := false
	for _, o := range resp.Output {
		if o.Type == "reasoning" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatalf("the response must carry the reasoning output item: %s", w.Body.String())
	}

	// 落链的 transcript 不得含 reasoning 项（回放侧）：否则链式回放会撞上
	// 「输入里的 reasoning 项 → 400」。
	var transcript []byte
	if err := f.db.QueryRow(`SELECT transcript FROM inference_response_chains WHERE id = $1`,
		resp.ID).Scan(&transcript); err != nil {
		t.Fatalf("chain row missing: %v", err)
	}
	if strings.Contains(string(transcript), `"type":"reasoning"`) {
		t.Errorf("the stored transcript must not contain a reasoning item: %s", transcript)
	}

	// 第二轮：previous_response_id 链必须照常工作（200）。
	w2 := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6", "previous_response_id": resp.ID,
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "what did I say?"}},
		}},
	})
	if w2.Code != http.StatusOK {
		t.Fatalf("chained code = %d body = %s (previous_response_id must keep working)",
			w2.Code, w2.Body.String())
	}

	// 手动接续（回显 reasoning 项）→ 明确 400，不静默丢弃。
	w3 := f.call(t, http.MethodPost, "/v1/responses", f.keyPlain, map[string]any{
		"model": "glm-4.6",
		"input": []map[string]any{
			{"type": "message", "role": "user",
				"content": []map[string]any{{"type": "input_text", "text": "remember 42"}}},
			{"type": "reasoning", "id": "rs_1",
				"summary": []map[string]any{{"type": "summary_text", "text": "plan"}}},
		},
	})
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("manual echo code = %d, want 400 (reasoning 项必须明确拒绝): %s",
			w3.Code, w3.Body.String())
	}
}
