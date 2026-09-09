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
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(f.resolver, access.NewRPMCounter(nil), 0))
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
	var prov domain.Provider
	if err := f.db.Get(&prov, `SELECT * FROM inference_providers WHERE id = $1`, f.provID); err == nil {
		base.Providers[prov.ID] = prov
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
