package providers

import (
	"context"
	"errors"
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
		Deployment: dep, Secret: []byte("sk-test"), RequestID: "req-1",
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
	resp := &http.Response{Header: http.Header{"Request-Id": []string{"r-2"}}}
	if got := upstreamRequestID(resp); got != "r-2" {
		t.Errorf("request id fallback = %q", got)
	}
}
