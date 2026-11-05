package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/model"
)

// adapter_dispatch_test.go — Dispatch 的失败分类学与管线（Task 16 覆盖率
// 补强）：429/5xx/400 状态、传输错误二分（未执行 vs 执行未知）、调用方
// 取消、自定义头再校验、非流式 decode、错误体截断、EndpointURL/请求 ID。

func testDeployment(proto domain.Protocol, baseURL string) *domain.Deployment {
	return &domain.Deployment{
		ProviderID: "p1", UpstreamModel: "up", BaseURL: baseURL,
		Protocol: proto, ConnectTimeout: time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
}

func chatCall(dep *domain.Deployment, stream bool) *Call {
	return &Call{
		Deployment: dep, Auth: Auth{Type: AuthTypeAPIKey, Token: "sk-test"}, RequestID: "req-1",
		Request: &ChatRequest{
			Model: "m1", Stream: stream,
			Messages: []model.ChatMessage{{Role: "user", Content: "hi"}},
		},
		OutputCap: 16,
	}
}

func TestDispatchError_RetryableMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *DispatchError
		want bool
	}{
		{"429", &DispatchError{StatusCode: 429}, true},
		{"500", &DispatchError{StatusCode: 500}, true},
		{"503", &DispatchError{StatusCode: 503}, true},
		{"400", &DispatchError{StatusCode: 400}, false},
		{"401", &DispatchError{StatusCode: 401}, false},
		{"transport", &DispatchError{Err: io.EOF}, true},
		{"nil", &DispatchError{}, false},
		{"canceled", &DispatchError{Err: context.Canceled}, false},
	} {
		if got := tc.err.Retryable(); got != tc.want {
			t.Errorf("%s: Retryable() = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Error/Unwrap 形状。
	e := &DispatchError{StatusCode: 500, Body: "boom"}
	if !strings.Contains(e.Error(), "500") || e.Unwrap() != nil {
		t.Errorf("status error shape: %v / %v", e.Error(), e.Unwrap())
	}
	e2 := &DispatchError{Err: io.EOF}
	if !strings.Contains(e2.Error(), "transport") || !errors.Is(e2.Unwrap(), io.EOF) {
		t.Errorf("transport error shape: %v", e2.Error())
	}
}

func TestIsCanceled_AndClassifyTransport(t *testing.T) {
	if IsCanceled(context.Canceled) != true {
		t.Error("context.Canceled must classify as canceled")
	}
	// url.Error timeout → 执行未知，不是取消。
	uerr := &url.Error{Op: "Post", URL: "http://x", Err: &timeoutErr{}}
	if IsCanceled(uerr) {
		t.Error("timeout must NOT be canceled (unknown execution)")
	}
	// 传输二分：DNS/ECONNREFUSED = 未执行；其余 = 执行未知。
	if classifyTransport(&net.DNSError{IsNotFound: true}) {
		t.Error("DNS failure = definitely not executed")
	}
	if classifyTransport(syscall.ECONNREFUSED) {
		t.Error("conn refused = definitely not executed")
	}
	if !classifyTransport(io.EOF) {
		t.Error("EOF mid-flight = executed unknown")
	}
}

type timeoutErr struct{}

func (e *timeoutErr) Error() string   { return "timeout" }
func (e *timeoutErr) Timeout() bool   { return true }
func (e *timeoutErr) Temporary() bool { return true }

func TestDispatch_StatusAndBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-Id") != "req-1" {
			t.Errorf("tracing header missing")
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("X-Request-Id", "up-9")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, strings.Repeat("e", 10<<10)) // 超 cap 的错误体
	}))
	defer srv.Close()

	d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), chatCall(testDeployment(domain.ProtocolOpenAIChat, srv.URL), false))
	if d != nil {
		t.Fatal("429 must not produce a result")
	}
	de, ok := err.(*DispatchError)
	if !ok || de.StatusCode != 429 || !de.Retryable() {
		t.Fatalf("err = %#v, want retryable 429", err)
	}
	if len(de.Body) > upstreamErrorBodyCap {
		t.Fatalf("error body not capped: %d", len(de.Body))
	}
}

func TestDispatch_NonStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	}))
	defer srv.Close()
	d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), chatCall(testDeployment(domain.ProtocolOpenAIChat, srv.URL), false))
	if err != nil {
		t.Fatal(err)
	}
	if d.UpstreamRequestID != "chatcmpl-1" || !d.UsageReported || d.Usage.InputTokens == nil || *d.Usage.InputTokens != 7 {
		t.Fatalf("result = %+v", d)
	}
	if d.ContentBytes != int64(len("Hello")) {
		t.Fatalf("content bytes = %d", d.ContentBytes)
	}
}

func TestDispatch_StreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), chatCall(testDeployment(domain.ProtocolOpenAIChat, srv.URL), true))
	if err != nil {
		t.Fatal(err)
	}
	if d.Stream == nil {
		t.Fatal("stream result expected")
	}
	raw, err := io.ReadAll(d.Stream.TeeBody())
	if err != nil || !strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("stream body: %v %q", err, raw)
	}
	res := d.Stream.Tap.Result()
	if !res.SawUsage || !res.Terminal {
		t.Errorf("tap = %+v, want usage+terminal", res)
	}
}

func TestDispatch_TransportErrorClassification(t *testing.T) {
	// 连接拒绝（未监听端口）→ definitely-not-executed，可安全重试。
	d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(),
		chatCall(testDeployment(domain.ProtocolOpenAIChat, "http://127.0.0.1:1"), false))
	if d != nil {
		t.Fatal("no result on transport error")
	}
	de, ok := err.(*DispatchError)
	if !ok || de.ExecutedUnknown {
		t.Fatalf("err = %#v, want not-executed transport error", err)
	}
	// 调用方取消 → 不可重试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Dispatch(ctx, NewHTTPClient(nil), NewOpenAIChat(), chatCall(testDeployment(domain.ProtocolOpenAIChat, "http://127.0.0.1:1"), false))
	if de, ok := err.(*DispatchError); !ok || de.Retryable() || de.ExecutedUnknown {
		t.Fatalf("canceled dispatch = %#v, want non-retryable not-unknown", err)
	}
}

func TestDispatch_HeaderRevalidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":{"role":"assistant","content":"y"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()
	call := chatCall(testDeployment(domain.ProtocolOpenAIChat, srv.URL), false)
	call.ExtraHeaders = map[string]string{"Authorization": "evil"} // 黑名单头
	_, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), call)
	if err == nil || domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("blacklisted header must fail: %v", err)
	}
	// 良性头放行。
	call.ExtraHeaders = map[string]string{"X-Team": "ops"}
	d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), call)
	if err != nil || d == nil {
		t.Fatalf("benign header: %v", err)
	}
}

func TestDispatch_NonStreamDecodeFailureIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not json at all`)
	}))
	defer srv.Close()
	_, err := Dispatch(context.Background(), NewHTTPClient(nil), NewOpenAIChat(), chatCall(testDeployment(domain.ProtocolOpenAIChat, srv.URL), false))
	de, ok := err.(*DispatchError)
	if !ok || !de.ExecutedUnknown {
		t.Fatalf("unparseable 200 = %#v, want executed-unknown", err)
	}
}

func TestEndpointURL_AndUpstreamRequestID(t *testing.T) {
	if got := EndpointURL(testDeployment(domain.ProtocolAnthropicMessage, "https://api.a.com/")); got != "https://api.a.com/v1/messages" {
		t.Errorf("anthropic endpoint = %q", got)
	}
	if got := EndpointURL(testDeployment(domain.ProtocolOpenAIChat, "https://api.b.com")); got != "https://api.b.com/chat/completions" {
		t.Errorf("openai endpoint = %q", got)
	}
	if got := EndpointURL(testDeployment(domain.ProtocolOpenAIResponses, "https://api.c.com/v1/")); got != "https://api.c.com/v1/responses" {
		t.Errorf("openai_responses endpoint = %q", got)
	}
	resp := &http.Response{Header: http.Header{"Request-Id": []string{"r-2"}}}
	if got := upstreamRequestID(resp); got != "r-2" {
		t.Errorf("request id fallback = %q", got)
	}
}

// 评审轮2 I1：跨 origin 重定向绝不跟随 —— 重定向目标收不到任何请求（上游
// x-api-key / operator 扩展头与客户请求体不外泄），3xx 响应体作为上游错
// 误 surfaced。302 与 307（重发请求体的那类）都覆盖。
func TestDispatch_CrossOriginRedirectRefused(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprintf("status %d", code), func(t *testing.T) {
			targetHits := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHits++
				if k := r.Header.Get("x-api-key"); k != "" {
					t.Errorf("x-api-key leaked to redirect target: %q", k)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, code)
			}))
			defer origin.Close()

			call := chatCall(testDeployment(domain.ProtocolAnthropicMessage, origin.URL), false)
			call.ExtraHeaders = map[string]string{"X-Team": "ops"}
			d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewAnthropicMessages(), call)
			if d != nil {
				t.Fatal("cross-origin redirect must not produce a result")
			}
			de, ok := err.(*DispatchError)
			if !ok || de.StatusCode != code {
				t.Fatalf("err = %#v, want upstream %d surfaced", err, code)
			}
			if targetHits != 0 {
				t.Fatalf("redirect target received %d requests, want 0 (凭证绝不跨主机)", targetHits)
			}
		})
	}
}

// 评审轮2 I1 对照组：同 origin 重定向照常跟随，上游密钥与扩展头保留（凭证
// 本来就该发给这台主机）。
func TestDispatch_SameOriginRedirectFollowed(t *testing.T) {
	var sawFinal bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Redirect(w, r, "/v1/messages", http.StatusFound)
			return
		}
		sawFinal = true
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q on same-origin redirect", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("X-Team") != "ops" {
			t.Errorf("operator header = %q on same-origin redirect", r.Header.Get("X-Team"))
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"up",
			"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer srv.Close()

	call := chatCall(testDeployment(domain.ProtocolAnthropicMessage, srv.URL), false)
	call.ExtraHeaders = map[string]string{"X-Team": "ops"}
	d, err := Dispatch(context.Background(), NewHTTPClient(nil), NewAnthropicMessages(), call)
	if err != nil || d == nil {
		t.Fatalf("same-origin redirect must be followed: %v", err)
	}
	if !sawFinal {
		t.Error("final handler never reached")
	}
}

// ---------------------------------------------------------------------------
// T1 — 出站鉴权头：凭据类型 → 协议明确支持的头
// ---------------------------------------------------------------------------

// TestAuthHeaders_StaticCredentialsUnchanged: 静态 api_key/service 的出站头
// 保持旧口径（openai 兼容 = Bearer，anthropic = x-api-key）。
func TestAuthHeaders_StaticCredentialsUnchanged(t *testing.T) {
	chat, err := NewOpenAIChat().AuthHeaders(Auth{Type: AuthTypeAPIKey, Token: "sk-static"})
	if err != nil {
		t.Fatalf("openai_chat api_key: %v", err)
	}
	if chat["Authorization"] != "Bearer sk-static" {
		t.Errorf("openai_chat api_key Authorization = %q", chat["Authorization"])
	}
	resp, err := NewOpenAIResponses().AuthHeaders(Auth{Type: AuthTypeService, Token: "svc-static"})
	if err != nil {
		t.Fatalf("openai_responses service: %v", err)
	}
	if resp["Authorization"] != "Bearer svc-static" {
		t.Errorf("openai_responses service Authorization = %q", resp["Authorization"])
	}
	ant, err := NewAnthropicMessages().AuthHeaders(Auth{Type: AuthTypeAPIKey, Token: "sk-ant-static"})
	if err != nil {
		t.Fatalf("anthropic api_key: %v", err)
	}
	if ant["x-api-key"] != "sk-ant-static" {
		t.Errorf("anthropic api_key x-api-key = %q", ant["x-api-key"])
	}
	if ant["anthropic-version"] == "" {
		t.Error("anthropic-version header missing")
	}
	if _, ok := ant["Authorization"]; ok {
		t.Error("a static key must not be presented as a bearer token")
	}
}

// TestAuthHeaders_OAuthUsesBearerOnly: OpenAI 兼容协议里 oauth 凭据只以
// bearer 出站；封存的 bundle JSON 与 refresh token 到不了任何头。
func TestAuthHeaders_OAuthUsesBearerOnly(t *testing.T) {
	const access = "acc-token-adapter"
	for _, a := range []Adapter{NewOpenAIChat(), NewOpenAIResponses()} {
		h, err := a.AuthHeaders(Auth{Type: AuthTypeOAuth, Token: access})
		if err != nil {
			t.Fatalf("%s: oauth: %v", a.Protocol(), err)
		}
		if h["Authorization"] != "Bearer "+access {
			t.Errorf("%s: oauth Authorization = %q, want the bare access token", a.Protocol(), h["Authorization"])
		}
		if _, ok := h["x-api-key"]; ok {
			t.Errorf("%s: an oauth access token must not occupy the api-key slot", a.Protocol())
		}
		for name, v := range h {
			if strings.Contains(v, "access_token") || strings.Contains(v, "refresh_token") || strings.Contains(v, "{") {
				t.Errorf("%s: header %q carries bundle material", a.Protocol(), name)
			}
		}
	}
}

// TestAuthHeaders_AnthropicRefusesOAuth: anthropic_messages 的明确支持集里
// 没有 OAuth 供应商 —— 仓库自有的 OAuth 连接器说 OpenAI 兼容协议，而
// Anthropic 官方 claude.ai OAuth 需要本部署不发的 anthropic-beta 头。凭据
// 能解包不等于该协议兼容：明确拒绝、不猜头槽、错误不带凭据材料。
func TestAuthHeaders_AnthropicRefusesOAuth(t *testing.T) {
	const access = "acc-token-anthropic"
	h, err := NewAnthropicMessages().AuthHeaders(Auth{Type: AuthTypeOAuth, Token: access})
	if err == nil {
		t.Fatalf("anthropic_messages must refuse oauth credentials (header count=%d)", len(h))
	}
	if h != nil {
		t.Errorf("a refused oauth credential must not produce headers")
	}
	if domain.CodeOf(err) != domain.CodeInvalidInput {
		t.Errorf("refusal code = %v, want invalid_input", domain.CodeOf(err))
	}
	for _, forbidden := range []string{access, "access_token", "refresh_token"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("the refusal surfaced credential material")
		}
	}
}

// TestAuthHeaders_UnsupportedAuthTypeRefused: 协议不支持的凭据类型明确拒绝，
// 不猜头槽。
func TestAuthHeaders_UnsupportedAuthTypeRefused(t *testing.T) {
	for _, a := range []Adapter{NewOpenAIChat(), NewOpenAIResponses(), NewAnthropicMessages()} {
		h, err := a.AuthHeaders(Auth{Type: "magic", Token: "x"})
		if err == nil {
			t.Errorf("%s: an unsupported auth type must be refused (headers=%v)", a.Protocol(), h)
			continue
		}
		if domain.CodeOf(err) != domain.CodeInvalidInput {
			t.Errorf("%s: refusal code = %v, want invalid_input", a.Protocol(), domain.CodeOf(err))
		}
		if h != nil {
			t.Errorf("%s: a refused auth type must not produce headers", a.Protocol())
		}
	}
}
