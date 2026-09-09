// inference_protocols_test.go — Task 13 跨协议集成验收（真实库 + 真实
// httptest 上游,覆盖 OpenAI 与 Anthropic 两种上游协议）:
//   1. 多轮工具调用 ID 保留(chat/messages/responses × openai/anthropic 上游);
//   2. 错误中断流式序列(三协议各见其原生终止/错误语义,结算归类一致);
//   3. token 分类账本跨协议一致性(同一请求三个协议同一账本分类);
//   4. 无法兼容字段的明确拒绝(400 + 零痕迹);
//   5. 会话绑定归属与跨客户隔离(previous_response_id 链)。
// skip 不算通过:无库即失败由 setupDB 的 Fatalf 承担(connect 失败)。

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

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

// protoStack is the full Task 13 stack: two providers (one OpenAI-speaking,
// one Anthropic-speaking upstream), two models pinned to one deployment
// each, one customer with both models entitled.
type protoStack struct {
	db     *sqlx.DB
	store  *postgres.Store
	engine *gin.Engine

	openaiUpstream     *protoUpstream
	anthropicUpstream  *protoUpstream
	keyPlain           string
	accountID          string
	openaiModel        string // routes to the openai_chat deployment
	anthropicModel     string // routes to the anthropic_messages deployment
	openaiAccountID    string
	anthropicAccountID string
}

// protoUpstream records request bodies and serves scripted responses.
type protoUpstream struct {
	*httptest.Server
	bodies  chan []byte
	handler func(w http.ResponseWriter, body []byte)
}

func newProtoUpstream(t *testing.T, h func(w http.ResponseWriter, body []byte)) *protoUpstream {
	t.Helper()
	u := &protoUpstream{bodies: make(chan []byte, 16), handler: h}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case u.bodies <- body:
		default:
		}
		u.handler(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *protoUpstream) lastBody(t *testing.T) []byte {
	t.Helper()
	select {
	case b := <-u.bodies:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("upstream saw no request")
		return nil
	}
}

func writeOpenAIChunks(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, c := range chunks {
		_, _ = io.WriteString(w, "data: "+c+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// openAICompletionHandler answers text completions (usage 7/5) — non-stream
// and stream.
func openAICompletionHandler(w http.ResponseWriter, body []byte) {
	if strings.Contains(string(body), `"stream":true`) {
		writeOpenAIChunks(w,
			`{"id":"chatcmpl-i1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"id":"chatcmpl-i1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-i1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
			"[DONE]")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-i1ns","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
}

// openAIToolCallHandler always answers a tool call (id call_up_1), then text.
func openAIToolCallHandler(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-t1","object":"chat.completion","created":1,"model":"up",
		"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[
		  {"id":"call_up_1","type":"function","function":{"name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":21,"completion_tokens":9,"total_tokens":30}}`)
}

// anthropicMessagesHandler answers the Anthropic Messages shape; tool mode
// when the request carries tools, text otherwise.
func anthropicMessagesHandler(w http.ResponseWriter, body []byte) {
	if strings.Contains(string(body), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, ev := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-up\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = io.WriteString(w, ev)
			if fl != nil {
				fl.Flush()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(string(body), `"tools"`) {
		_, _ = io.WriteString(w, `{"id":"msg_up_t1","type":"message","role":"assistant","model":"claude-up",
			"content":[{"type":"tool_use","id":"toolu_up_9","name":"run_shell","input":{"cmd":"ls"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":21,"output_tokens":9,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}`)
		return
	}
	_, _ = io.WriteString(w, `{"id":"msg_up_1","type":"message","role":"assistant","model":"claude-up",
		"content":[{"type":"text","text":"Hi there"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":11,"output_tokens":6}}`)
}

func newProtoStack(t *testing.T) *protoStack {
	t.Helper()
	db := setupDB(t)
	ctx := context.Background()
	_, err := db.Exec(`TRUNCATE
		inference_response_chains, inference_bulk_imports,
		inference_session_bindings, inference_oauth_grants,
		inference_audit_log, operator_roles,
		inference_reconciliation_jobs, inference_outbox,
		inference_ledger_entries, inference_adjustments,
		inference_concurrency_leases, inference_reservations,
		inference_quota_windows, inference_usage_records,
		inference_attempts, inference_requests,
		inference_entitlements, inference_policy_versions,
		inference_price_versions,
		inference_api_keys, inference_billing_accounts,
		inference_upstream_accounts, inference_credentials,
		inference_config_revisions, inference_model_routes,
		inference_deployments, inference_providers, inference_models,
		users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
	store := postgres.NewStore(db)
	st := &protoStack{db: db, store: store,
		openaiModel: "codex-pro", anthropicModel: "claude-pro"}

	st.openaiUpstream = newProtoUpstream(t, openAICompletionHandler)
	st.anthropicUpstream = newProtoUpstream(t, anthropicMessagesHandler)

	protocols := []domain.Protocol{
		domain.ProtocolOpenAIChat, domain.ProtocolAnthropicMessage, domain.ProtocolOpenAIResponses,
	}
	for _, m := range []string{st.openaiModel, st.anthropicModel} {
		if err := store.InsertModel(ctx, &domain.Model{
			ID: m, DisplayName: m, Lifecycle: domain.LifecycleActive,
			ContextTokens: 200000, MaxOutputTokens: 8192, Protocols: protocols,
			InputModalities: []string{"text"}, OutputModalities: []string{"text"},
			SupportsTools: true, SupportsReasoning: true,
		}); err != nil {
			t.Fatalf("insert model %s: %v", m, err)
		}
	}
	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	credSvc := credentials.NewService(vault, store, store)

	snap := &catalog.Snapshot{
		Models:        map[string]domain.Model{},
		Providers:     map[string]domain.Provider{},
		Deployments:   map[string]domain.Deployment{},
		RoutesByModel: map[string][]domain.ModelRoute{},
	}
	for _, m := range []string{st.openaiModel, st.anthropicModel} {
		mod, err := store.GetModel(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		snap.Models[m] = *mod
	}
	addLeg := func(providerCode, modelID, baseURL string, proto domain.Protocol) string {
		prov := &domain.Provider{Code: providerCode, DisplayName: providerCode,
			AccessType: domain.AccessOfficialAPI, Status: "active"}
		if err := store.InsertProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		dep := &domain.Deployment{
			ProviderID: prov.ID, UpstreamModel: "up-model", BaseURL: baseURL,
			Protocol:       proto,
			ConnectTimeout: 2 * time.Second, RequestTimeout: 10 * time.Second,
			Status: domain.DeploymentActive,
		}
		if err := store.InsertDeployment(ctx, dep); err != nil {
			t.Fatal(err)
		}
		cv, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "it"},
			prov.ID, "main", "api_key", "sk-"+providerCode, "seed", nil)
		if err != nil {
			t.Fatal(err)
		}
		acct := &domain.UpstreamAccount{
			ProviderID: prov.ID, CredentialID: cv.ID, Status: domain.AccountActive, ConcurrencyLimit: 8,
		}
		if err := store.InsertUpstreamAccount(ctx, acct); err != nil {
			t.Fatal(err)
		}
		snap.Providers[prov.ID] = *prov
		snap.Deployments[dep.ID] = *dep
		snap.RoutesByModel[modelID] = []domain.ModelRoute{
			{ModelID: modelID, DeploymentID: dep.ID, Weight: 1, Enabled: true},
		}
		return acct.ID
	}
	st.openaiAccountID = addLeg("prov-openai", st.openaiModel, st.openaiUpstream.URL, domain.ProtocolOpenAIChat)
	st.anthropicAccountID = addLeg("prov-anthropic", st.anthropicModel, st.anthropicUpstream.URL, domain.ProtocolAnthropicMessage)

	// policy / prices / entitlement / key
	pol := &postgres.PolicyVersion{
		Name: "coding-plan", Revision: 1, ModelIDs: []string{st.openaiModel, st.anthropicModel},
		FiveHourLimit: microc(1_000_000_000), WeeklyLimit: microc(10_000_000_000),
		MonthlyLimit: microc(100_000_000_000), Status: "published",
	}
	if err := store.InsertPolicyVersion(ctx, pol); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	for _, m := range []string{st.openaiModel, st.anthropicModel} {
		if err := store.InsertPriceVersion(ctx, &postgres.PriceVersion{
			ModelID: m, Kind: postgres.PriceSaleCredit,
			InputPerMtok: 1_000_000, OutputPerMtok: 2_000_000,
			Revision: 1, EffectiveFrom: now, Unit: "microcredit",
		}); err != nil {
			t.Fatal(err)
		}
	}
	userID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}
	acct, err := store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	st.accountID = acct.ID
	if err := store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acct.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{st.openaiModel, st.anthropicModel},
		PolicyVersionID: pol.ID, AnchorAt: now, EffectiveFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	keySvc := access.NewKeyService(store, nil)
	created, err := keySvc.CreateKey(ctx, userID, access.CreateParams{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	st.keyPlain = created.Plaintext

	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[domain.Protocol]providers.Adapter{
		domain.ProtocolOpenAIChat:       providers.NewOpenAIChat(),
		domain.ProtocolAnthropicMessage: providers.NewAnthropicMessages(),
	}
	routingSvc := routing.NewService(store, adapters, nil)
	gw := gateway.NewService(staticSnapshot{snap}, store,
		access.NewEntitlementResolver(store, nil), quota.NewService(store, nil),
		routingSvc, credSvc, providers.NewHTTPClient(egress), egress, nil)
	gw.SetSessionBinder(routing.NewSessionBinder(store, nil))

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	resolver := access.NewResolver(store, nil)
	v1 := engine.Group("/v1", httpapi.APIKeyAuth(resolver, access.NewRPMCounter(nil), 0))
	v1.POST("/chat/completions", httpapi.NewChatCompletionsHandler(gw).Create)
	v1.POST("/messages", httpapi.NewMessagesHandler(gw).Create)
	v1.POST("/responses", httpapi.NewResponsesHandler(gw, store, nil).Create)
	st.engine = engine
	return st
}

type staticSnapshot struct{ snap *catalog.Snapshot }

func (s staticSnapshot) Current(ctx context.Context) (*catalog.Snapshot, error) { return s.snap, nil }

func microc(v int64) *domain.Microcredit {
	m := domain.Microcredit(v)
	return &m
}

func (st *protoStack) call(t *testing.T, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+st.keyPlain)
	w := httptest.NewRecorder()
	st.engine.ServeHTTP(w, req)
	return w
}

// sseEvents parses an SSE body into (event, data) pairs.
func sseEvents(t *testing.T, body string) [][2]string {
	t.Helper()
	var out [][2]string
	var event string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, [2]string{event, strings.TrimPrefix(line, "data: ")})
			event = ""
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. 多轮工具调用 ID 保留
// ---------------------------------------------------------------------------

func TestProtocols_MultiTurnToolCallIDPreserved(t *testing.T) {
	t.Run("chat→openai", func(t *testing.T) {
		st := newProtoStack(t)
		st.openaiUpstream.handler = openAIToolCallHandler
		// 多轮历史:assistant tool_calls id call_hist_1 + tool 消息回应。
		w := st.call(t, "/v1/chat/completions", `{
		  "model":"codex-pro",
		  "messages":[
		    {"role":"user","content":"list"},
		    {"role":"assistant","content":"","tool_calls":[{"id":"call_hist_1","type":"function","function":{"name":"run_shell","arguments":"{}"}}]},
		    {"role":"tool","tool_call_id":"call_hist_1","content":"a.txt"}
		  ],
		  "tools":[{"type":"function","function":{"name":"run_shell","parameters":{"type":"object"}}}]
		}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		up := string(st.openaiUpstream.lastBody(t))
		if !strings.Contains(up, `"tool_call_id":"call_hist_1"`) || !strings.Contains(up, `"id":"call_hist_1"`) {
			t.Errorf("history tool-call id not preserved upstream: %s", up)
		}
		// 上游新发的 tool_call id 透传回客户端。
		var resp struct {
			Choices []struct {
				Message struct {
					ToolCalls []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Choices[0].Message.ToolCalls[0].ID != "call_up_1" {
			t.Errorf("upstream tool id not preserved to client: %s", w.Body.String())
		}
	})

	t.Run("messages→anthropic 双重翻译", func(t *testing.T) {
		st := newProtoStack(t)
		// Messages 面 + Anthropic 上游:客户端 tool_use/tool_result 历史
		// (toolu_hist_1)经内部 chat 形状再到 Anthropic 上游,ID 全程不变;
		// 上游新 tool_use (toolu_up_9) 回到客户端仍是原 id。
		w := st.call(t, "/v1/messages", `{
		  "model":"claude-pro","max_tokens":256,
		  "messages":[
		    {"role":"user","content":"list"},
		    {"role":"assistant","content":[
		      {"type":"text","text":"checking"},
		      {"type":"tool_use","id":"toolu_hist_1","name":"run_shell","input":{"cmd":"ls"}}
		    ]},
		    {"role":"user","content":[
		      {"type":"tool_result","tool_use_id":"toolu_hist_1","content":"a.txt"}
		    ]}
		  ],
		  "tools":[{"name":"run_shell","input_schema":{"type":"object"}}]
		}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		up := string(st.anthropicUpstream.lastBody(t))
		if !strings.Contains(up, `"id":"toolu_hist_1"`) {
			t.Errorf("tool_use id not preserved upstream: %s", up)
		}
		if !strings.Contains(up, `"tool_use_id":"toolu_hist_1"`) {
			t.Errorf("tool_result id not preserved upstream: %s", up)
		}
		// Anthropic 原生响应形状 + 新 tool_use id 保留。
		var msg struct {
			Type       string `json:"type"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content"`
			Usage struct {
				InputTokens          int `json:"input_tokens"`
				OutputTokens         int `json:"output_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
			t.Fatalf("anthropic client decode: %v (%s)", err, w.Body.String())
		}
		if msg.Type != "message" || msg.StopReason != "tool_use" ||
			len(msg.Content) != 1 || msg.Content[0].ID != "toolu_up_9" || msg.Content[0].Name != "run_shell" {
			t.Errorf("msg = %s", w.Body.String())
		}
		// usage:Anthropic 口径 input 不含 cache_read(21 本就不含),cache 单列。
		if msg.Usage.InputTokens != 21 || msg.Usage.CacheReadInputTokens != 2 || msg.Usage.OutputTokens != 9 {
			t.Errorf("usage = %+v", msg.Usage)
		}
	})

	t.Run("responses→openai", func(t *testing.T) {
		st := newProtoStack(t)
		st.openaiUpstream.handler = openAIToolCallHandler
		w := st.call(t, "/v1/responses", `{
		  "model":"codex-pro",
		  "input":[
		    {"type":"message","role":"user","content":[{"type":"input_text","text":"list"}]},
		    {"type":"function_call","call_id":"call_hist_7","name":"run_shell","arguments":"{}"},
		    {"type":"function_call_output","call_id":"call_hist_7","output":"a.txt"}
		  ],
		  "tools":[{"type":"function","name":"run_shell","parameters":{"type":"object"}}]
		}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
		}
		up := string(st.openaiUpstream.lastBody(t))
		if !strings.Contains(up, `"id":"call_hist_7"`) || !strings.Contains(up, `"tool_call_id":"call_hist_7"`) {
			t.Errorf("call_id not preserved upstream: %s", up)
		}
		var resp struct {
			Output []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"output"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" ||
			resp.Output[0].CallID != "call_up_1" || resp.Output[0].Name != "run_shell" {
			t.Errorf("output = %s", w.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// 2. 错误中断流式序列
// ---------------------------------------------------------------------------

// brokenStreamHandler 发半截内容后无终止标记直接结束(上游断流)。
func brokenStreamHandler(w http.ResponseWriter, body []byte) {
	if !strings.Contains(string(body), `"stream":true`) {
		openAICompletionHandler(w, body)
		return
	}
	writeOpenAIChunks(w,
		`{"id":"chatcmpl-brk","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"par"}}]}`)
	// 无 [DONE] —— 直接结束响应体。
}

func TestProtocols_BrokenStreamSequence(t *testing.T) {
	t.Run("chat: 内联错误块,无 [DONE]", func(t *testing.T) {
		st := newProtoStack(t)
		st.openaiUpstream.handler = brokenStreamHandler
		w := st.call(t, "/v1/chat/completions", `{"model":"codex-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		s := w.Body.String()
		if !strings.Contains(s, `"error"`) || strings.Contains(s, "[DONE]") {
			t.Errorf("chat broken stream must carry in-band error and NO [DONE]:\n%s", s)
		}
		assertReconciliationParked(t, st.db)
	})

	t.Run("messages: event error,无 message_stop", func(t *testing.T) {
		st := newProtoStack(t)
		st.anthropicUpstream.handler = func(w http.ResponseWriter, body []byte) {
			// Anthropic 上游半截:text delta 后直接 EOF(无 message_stop)。
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_b\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n")
		}
		w := st.call(t, "/v1/messages", `{"model":"claude-pro","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		events := sseEvents(t, w.Body.String())
		var last string
		sawContent := false
		for _, ev := range events {
			last = ev[0]
			if ev[0] == "content_block_delta" {
				sawContent = true
			}
		}
		if last != "error" {
			t.Errorf("last event = %q, want error", last)
		}
		if !sawContent {
			t.Error("partial content delta missing")
		}
		for _, ev := range events {
			if ev[0] == "message_stop" {
				t.Fatalf("broken stream must never emit message_stop: %v", events)
			}
		}
		// 中断但 message_start 已携带用量(input=5;output 未报到):已读部分
		// 按 estimated 结算(设计 §7.1/§7.2:已知实际量不记零、不丢账),
		// 未报到桶保持 NULL(NULL ≠ 0)。
		var status, usageStatus string
		var in, out *int64
		if err := st.db.QueryRow(`SELECT r.status, r.usage_status, u.input_tokens, u.output_tokens
			FROM inference_requests r JOIN inference_usage_records u ON u.request_id = r.id
			ORDER BY r.created_at DESC LIMIT 1`).Scan(&status, &usageStatus, &in, &out); err != nil {
			t.Fatal(err)
		}
		if status != "settled" || usageStatus != "estimated" || in == nil || *in != 5 || out != nil {
			t.Errorf("interrupted-with-usage = %s/%s in=%v out=%v, want settled/estimated in=5 out=NULL",
				status, usageStatus, in, out)
		}
	})

	t.Run("responses: event error,无 response.completed", func(t *testing.T) {
		st := newProtoStack(t)
		st.openaiUpstream.handler = brokenStreamHandler
		w := st.call(t, "/v1/responses", `{"model":"codex-pro","stream":true,"input":"hi"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		events := sseEvents(t, w.Body.String())
		var last string
		for _, ev := range events {
			last = ev[0]
			if ev[0] == "response.completed" {
				t.Fatalf("broken stream must never emit response.completed: %v", events)
			}
		}
		if last != "error" {
			t.Errorf("last event = %q, want error", last)
		}
		assertReconciliationParked(t, st.db)
		// 半截轮次不落链(不可被 previous_response_id 引用)。
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_response_chains`).Scan(&n); err != nil || n != 0 {
			t.Errorf("chains = %d, want 0 for a broken turn", n)
		}
	})
}

// assertReconciliationParked: 中断且未读用量 → 预占保留 + 核对队列
// (不记零、不静默释放,设计 §7.2)。
func assertReconciliationParked(t *testing.T, db *sqlx.DB) {
	t.Helper()
	var status, usageStatus string
	if err := db.QueryRow(`SELECT status, usage_status FROM inference_requests
		ORDER BY created_at DESC LIMIT 1`).Scan(&status, &usageStatus); err != nil {
		t.Fatal(err)
	}
	if status != "reconciliation_required" || usageStatus != "unknown" {
		t.Errorf("request = %s/%s, want reconciliation_required/unknown", status, usageStatus)
	}
	var jobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM inference_reconciliation_jobs`).Scan(&jobs); err != nil || jobs == 0 {
		t.Errorf("reconciliation jobs = %d, want >0", jobs)
	}
}

// ---------------------------------------------------------------------------
// 3. token 分类账本跨协议一致性
// ---------------------------------------------------------------------------

// 同一逻辑请求在三个协议面下产生同一账本分类:usage 桶一致(7/5
// reported)、charge 金额一致(7×1+5×2=17 micros)、entry_type=charge、
// protocol 列如实记录各自协议。
func TestProtocols_LedgerConsistencyCrossProtocol(t *testing.T) {
	st := newProtoStack(t)

	calls := []struct {
		path     string
		protocol string
		body     string
	}{
		{"/v1/chat/completions", "openai_chat",
			`{"model":"codex-pro","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/messages", "anthropic_messages",
			`{"model":"codex-pro","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", "openai_responses",
			`{"model":"codex-pro","input":"hi"}`},
	}
	for _, c := range calls {
		w := st.call(t, c.path, c.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code = %d body = %s", c.path, w.Code, w.Body.String())
		}
	}

	rows, err := st.db.Query(`SELECT protocol, status, settled_micros FROM inference_requests ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var proto, status string
		var settled int64
		if err := rows.Scan(&proto, &status, &settled); err != nil {
			t.Fatal(err)
		}
		if proto != calls[i].protocol {
			t.Errorf("request %d protocol = %s, want %s", i, proto, calls[i].protocol)
		}
		if status != "settled" || settled != 17 {
			t.Errorf("request %d (%s) = %s/%d, want settled/17", i, proto, status, settled)
		}
		i++
	}
	if i != 3 {
		t.Fatalf("requests = %d, want 3", i)
	}

	// usage_records:三次调用的桶完全一致(input=7 output=5, reported)。
	urecs, err := st.db.Query(`SELECT input_tokens, output_tokens, source FROM inference_usage_records ORDER BY recorded_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer urecs.Close()
	n := 0
	for urecs.Next() {
		var in, out int64
		var source string
		if err := urecs.Scan(&in, &out, &source); err != nil {
			t.Fatal(err)
		}
		if in != 7 || out != 5 || source != "reported" {
			t.Errorf("usage record = %d/%d/%s, want 7/5/reported (跨协议同口径)", in, out, source)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("usage records = %d, want 3", n)
	}

	// 账本:每请求恰一条 charge 分录、金额一致、无 reversal。
	var charges, reversals int
	if err := st.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE entry_type='charge' AND amount_micros=17),
		COUNT(*) FILTER (WHERE entry_type='reversal') FROM inference_ledger_entries`).Scan(&charges, &reversals); err != nil {
		t.Fatal(err)
	}
	if charges != 3 || reversals != 0 {
		t.Errorf("ledger charges=%d reversals=%d, want 3/0", charges, reversals)
	}
}

// ---------------------------------------------------------------------------
// 4. 无法兼容字段的明确拒绝
// ---------------------------------------------------------------------------

func TestProtocols_ExplicitRejectionMatrix(t *testing.T) {
	st := newProtoStack(t)
	cases := []struct {
		name string
		path string
		body string
		// 各自协议原生错误形状的识别子串
		wantMarker string
	}{
		{"chat modalities", "/v1/chat/completions",
			`{"model":"codex-pro","modalities":["text","image"],"messages":[{"role":"user","content":"x"}]}`,
			`"code":"invalid_input"`},
		{"messages image block", "/v1/messages",
			`{"model":"codex-pro","max_tokens":10,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"A"}}]}]}`,
			`"type":"invalid_request_error"`},
		{"messages server tool", "/v1/messages",
			`{"model":"codex-pro","max_tokens":10,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
			`"type":"invalid_request_error"`},
		{"responses background", "/v1/responses",
			`{"model":"codex-pro","input":"x","background":true}`,
			`"code":"invalid_input"`},
		{"responses item_reference", "/v1/responses",
			`{"model":"codex-pro","input":[{"type":"item_reference","id":"msg_zzz"}]}`,
			`"code":"invalid_input"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := st.call(t, tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantMarker) {
				t.Errorf("error shape marker %q missing: %s", tc.wantMarker, w.Body.String())
			}
		})
	}
	// 全部拒绝发生在预占之前:零请求、零账本。
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_requests`).Scan(&n); err != nil || n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_ledger_entries`).Scan(&n); err != nil || n != 0 {
		t.Errorf("ledger = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 5. 会话绑定归属与跨客户隔离
// ---------------------------------------------------------------------------

func TestProtocols_SessionBindingAttributionAndIsolation(t *testing.T) {
	st := newProtoStack(t)
	ctx := context.Background()

	// 第一轮:建立链 + 绑定。
	w := st.call(t, "/v1/responses", `{"model":"codex-pro","input":"turn one"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var bindAccount string
	if err := st.db.QueryRow(`SELECT account_id FROM inference_session_bindings
		WHERE session_key = $1 AND status='active'`, "respchain:"+resp.ID).Scan(&bindAccount); err != nil {
		t.Fatalf("binding missing: %v", err)
	}
	if bindAccount != st.openaiAccountID {
		t.Fatalf("binding = %s, want the only account %s", bindAccount, st.openaiAccountID)
	}

	// 加第二个账号:无钉住时 round-robin 会轮到它;钉住必须留在原账号。
	cv2, err := credentials.NewService(
		mustVaultP(t), st.store, st.store).Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "it"},
		provIDOf(t, st.db, st.openaiAccountID), "second", "api_key", "sk-second", "seed2", nil)
	if err != nil {
		t.Fatal(err)
	}
	acct2 := &domain.UpstreamAccount{
		ProviderID: provIDOf(t, st.db, st.openaiAccountID), CredentialID: cv2.ID,
		Status: domain.AccountActive, ConcurrencyLimit: 8,
	}
	if err := st.store.InsertUpstreamAccount(ctx, acct2); err != nil {
		t.Fatal(err)
	}

	w2 := st.call(t, "/v1/responses", fmt.Sprintf(`{"model":"codex-pro","previous_response_id":%q,"input":"turn two"}`, resp.ID))
	if w2.Code != http.StatusOK {
		t.Fatalf("chained code = %d body = %s", w2.Code, w2.Body.String())
	}
	var resp2 struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	var servedBy string
	if err := st.db.QueryRow(`SELECT upstream_account_id FROM inference_response_chains WHERE id=$1`, resp2.ID).Scan(&servedBy); err != nil {
		t.Fatal(err)
	}
	if servedBy != st.openaiAccountID {
		t.Errorf("sticky pin violated: served by %s, want bound %s", servedBy, st.openaiAccountID)
	}
	// transcript 回放到达上游(turn one 在第二轮的上游请求体中)。
	foundReplay := false
	for {
		select {
		case b := <-st.openaiUpstream.bodies:
			if strings.Contains(string(b), "turn two") {
				if !strings.Contains(string(b), "turn one") {
					t.Errorf("transcript replay missing upstream: %s", b)
				}
				foundReplay = true
			}
		default:
		}
		if foundReplay {
			break
		}
		select {
		case b := <-st.openaiUpstream.bodies:
			if strings.Contains(string(b), "turn two") {
				if !strings.Contains(string(b), "turn one") {
					t.Errorf("transcript replay missing upstream: %s", b)
				}
				foundReplay = true
			}
		case <-time.After(3 * time.Second):
			t.Fatal("chained request never reached upstream")
		}
	}

	// 跨客户:另一客户引用同一 resp id → 404,且其请求不会被绑到该会话。
	user2 := uuid.NewString()
	if _, err := st.db.Exec(`INSERT INTO users (id) VALUES ($1)`, user2); err != nil {
		t.Fatal(err)
	}
	acctB, err := st.store.EnsureBillingAccount(ctx, user2)
	if err != nil {
		t.Fatal(err)
	}
	var polID string
	if err := st.db.QueryRow(`SELECT policy_version_id FROM inference_entitlements LIMIT 1`).Scan(&polID); err != nil {
		t.Fatal(err)
	}
	if err := st.store.InsertEntitlement(ctx, &domain.Entitlement{
		BillingAccountID: acctB.ID, SourceType: domain.SourceSubscription,
		SourceID: "sub-" + uuid.NewString(), ModelIDs: []string{st.openaiModel},
		PolicyVersionID: polID, AnchorAt: time.Now().Add(-time.Minute),
		EffectiveFrom: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	keySvc := access.NewKeyService(st.store, nil)
	createdB, err := keySvc.CreateKey(ctx, user2, access.CreateParams{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(fmt.Sprintf(`{"model":"codex-pro","previous_response_id":%q,"input":"hijack"}`, resp.ID)))
	req.Header.Set("Authorization", "Bearer "+createdB.Plaintext)
	wB := httptest.NewRecorder()
	st.engine.ServeHTTP(wB, req)
	if wB.Code != http.StatusNotFound {
		t.Fatalf("cross-customer chain reference = %d, want 404", wB.Code)
	}
	// 绑定仍归原账号、无并发第二条 active 绑定。
	var activeBindings int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM inference_session_bindings
		WHERE session_key = $1 AND status='active'`, "respchain:"+resp.ID).Scan(&activeBindings); err != nil {
		t.Fatal(err)
	}
	if activeBindings != 1 {
		t.Errorf("active bindings = %d, want exactly 1", activeBindings)
	}
}

func mustVaultP(t *testing.T) *credentials.Vault {
	t.Helper()
	v, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func provIDOf(t *testing.T, db *sqlx.DB, accountID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(`SELECT provider_id FROM inference_upstream_accounts WHERE id=$1`, accountID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
