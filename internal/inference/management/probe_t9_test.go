package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
)

// probe_t9_test.go — T9 的探测契约（协议鉴权/版本头、凭据材料测试与连通性
// 探测的区分、供应商不提供 /models 时的诚实结论）。
//
// 断言全部落在**已存在**的 API 表面上（ProbeResult 的 JSON 形状 + 真实
// 出站请求的头），因此它们在未改动的生产代码上是行为断言失败（RED），
// 而不是编译错误。
//
// 语义（probe.go 的契约）：
//   - credential_state：凭据**材料**测试的结果（decrypted / missing /
//     unusable）；
//   - probe_method：探测实际做了什么（connectivity / none）；
//   - probe_outcome：诚实的结论（verified / failed / unsupported /
//     unconfirmed）——供应商不提供 /models 时是 unsupported，**不是**「模型
//     故障」，也**不是**「推理成功」；
//   - inference_verified：恒为 false —— 探测从不发起真实推理（不许花钱）。

// probeJSON 断言用：ProbeResult 的线上形状。RED 阶段这些键还不存在，
// 断言因此失败而不是编译失败。
type probeJSON struct {
	DeploymentID      string `json:"deployment_id"`
	OK                bool   `json:"ok"`
	Status            int    `json:"status"`
	LatencyMS         int64  `json:"latency_ms"`
	ErrorSummary      string `json:"error_summary"`
	CredentialState   string `json:"credential_state"`
	ProbeMethod       string `json:"probe_method"`
	ProbeOutcome      string `json:"probe_outcome"`
	ProbeSupported    bool   `json:"probe_supported"`
	InferenceVerified bool   `json:"inference_verified"`
	Description       string `json:"description"`
}

func probeResultJSON(t *testing.T, res *ProbeResult) probeJSON {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal probe result: %v", err)
	}
	var out probeJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode probe result: %v (%s)", err, raw)
	}
	return out
}

// anthropicProbeFixture：一个 anthropic_messages 协议的部署 + 可选 oauth
// 凭据。凭据的 auth type 由 fake resolver 返回的 *domain.Credential 携带
// （这是既有 API 表面 —— 生产代码当前完全忽略它）。
func anthropicProbeFixture(baseURL string, authType string) (*fakeProbeStore, *fakeBearerResolver) {
	store := &fakeProbeStore{
		deployments: map[string]domain.Deployment{
			"dep-1": {
				ID: "dep-1", ProviderID: "prov-1", UpstreamModel: "claude-upstream",
				BaseURL: baseURL, Protocol: domain.ProtocolAnthropicMessage,
				Status: domain.DeploymentActive,
			},
		},
		credentials: []domain.Credential{
			{ID: "cred-1", ProviderID: "prov-1", AuthType: authType, Status: "active", Generation: 7},
		},
	}
	resolver := &fakeBearerResolver{
		tokens:    map[string]string{"cred-1": "sk-anthropic-secret"},
		authTypes: map[string]string{"cred-1": authType},
	}
	return store, resolver
}

// TestProbeDeployment_AnthropicUsesProtocolAuthAndVersionHeaders 钉住 T9：
// 探测的鉴权/版本头**按协议**来 —— anthropic_messages 走 x-api-key +
// anthropic-version，绝不是 OpenAI 兼容的 Authorization: Bearer。
func TestProbeDeployment_AnthropicUsesProtocolAuthAndVersionHeaders(t *testing.T) {
	var gotAPIKey, gotVersion, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	store, resolver := anthropicProbeFixture(srv.URL, "api_key")
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotAPIKey != "sk-anthropic-secret" {
		t.Errorf("anthropic probe must use the x-api-key slot, got %q", gotAPIKey)
	}
	if gotVersion == "" {
		t.Error("anthropic probe must send the protocol version header")
	}
	if gotAuth != "" {
		t.Errorf("anthropic probe must not send an Authorization bearer header, got %q", gotAuth)
	}
	if !res.OK {
		t.Fatalf("probe = %+v, want ok=true", res)
	}
}

// TestProbeDeployment_AnthropicOAuthRefusedBeforeDispatch 钉住 T1 的明确
// 拒绝在探测面同样成立：anthropic_messages 的支持集里没有 OAuth 供应商，
// 因此 OAuth 凭据在**出站前**被拒绝 —— 绝不被塞进 x-api-key 槽，也绝不
// 当作「探测失败」冒充上游故障。
func TestProbeDeployment_AnthropicOAuthRefusedBeforeDispatch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := anthropicProbeFixture(srv.URL, "oauth")
	resolver.tokens["cred-1"] = "oauth-access-token"
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("an oauth credential must not be dispatched to anthropic, hits=%d", hits.Load())
	}
	if res.OK {
		t.Fatalf("probe = %+v, want ok=false (the refusal is not a successful probe)", res)
	}
	if !strings.Contains(strings.ToLower(res.ErrorSummary), "oauth") {
		t.Errorf("error_summary %q must name the oauth refusal", res.ErrorSummary)
	}
	j := probeResultJSON(t, res)
	if j.ProbeMethod != "none" {
		t.Errorf("probe_method = %q, want none (nothing was sent)", j.ProbeMethod)
	}
}

// TestProbeDeployment_OpenAIOAuthUsesBearer 钉住：OpenAI 兼容协议上的
// OAuth 凭据走 Authorization: Bearer（access token，不是密封闭包）。
func TestProbeDeployment_OpenAIOAuthUsesBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	resolver.authTypes = map[string]string{"cred-1": "oauth"}
	resolver.tokens["cred-1"] = "oauth-access-token"
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotAuth != "Bearer oauth-access-token" {
		t.Fatalf("openai oauth probe auth header = %q", gotAuth)
	}
	if !res.OK {
		t.Fatalf("probe = %+v, want ok=true", res)
	}
}

// TestProbeDeployment_VendorWithoutModelsIsUnsupportedNotFault 钉住 T9 的
// 核心诚实性：供应商不提供 /models 时，结论是「此探测方式不支持/无法确认」，
// **不是**模型故障，也**不是**推理成功。
func TestProbeDeployment_VendorWithoutModelsIsUnsupportedNotFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK {
		t.Fatalf("probe = %+v, want ok=false (no model health may be faked)", res)
	}
	j := probeResultJSON(t, res)
	if j.ProbeOutcome != "unsupported" {
		t.Errorf("probe_outcome = %q, want unsupported (供应商未提供 /models)", j.ProbeOutcome)
	}
	if j.ProbeSupported {
		t.Error("probe_supported must be false when the vendor does not expose the endpoint")
	}
	if j.InferenceVerified {
		t.Error("inference_verified must be false: the probe never performs a real inference")
	}
	if !strings.Contains(strings.ToLower(j.Description), "not") {
		t.Errorf("description %q must state that this is not evidence of an unhealthy model", j.Description)
	}
}

// TestProbeDeployment_SeparatesCredentialProbeAndInference 钉住三件事的
// 区分：凭据材料可解密（credential_state=decrypted）、指定探测成功
// （probe_outcome=verified / probe_method=connectivity）、本次**没有**做
// 真实推理（inference_verified=false）。
func TestProbeDeployment_SeparatesCredentialProbeAndInference(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	j := probeResultJSON(t, res)
	if j.CredentialState != "decrypted" {
		t.Errorf("credential_state = %q, want decrypted (材料可解密)", j.CredentialState)
	}
	if j.ProbeMethod != "connectivity" {
		t.Errorf("probe_method = %q, want connectivity", j.ProbeMethod)
	}
	if j.ProbeOutcome != "verified" {
		t.Errorf("probe_outcome = %q, want verified (指定探测成功)", j.ProbeOutcome)
	}
	if !j.ProbeSupported {
		t.Error("probe_supported must be true when the endpoint answered")
	}
	if j.InferenceVerified {
		t.Error("inference_verified must be false: this task never performs a paid inference")
	}
	if j.Description == "" {
		t.Error("description must be present (old fields stay, the description is appended)")
	}
}

// TestProbeDeployment_MissingCredentialIsUnconfirmed 钉住：没有可用凭据时
// 探测**没有发生**（probe_method=none），结论是 unconfirmed，绝不是
// 「上游模型故障」。
func TestProbeDeployment_MissingCredentialIsUnconfirmed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	store.credentials = nil
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("no credential means no outbound call, hits=%d", hits.Load())
	}
	j := probeResultJSON(t, res)
	if j.CredentialState != "missing" {
		t.Errorf("credential_state = %q, want missing", j.CredentialState)
	}
	if j.ProbeMethod != "none" {
		t.Errorf("probe_method = %q, want none", j.ProbeMethod)
	}
	if j.ProbeOutcome != "unconfirmed" {
		t.Errorf("probe_outcome = %q, want unconfirmed", j.ProbeOutcome)
	}
	if j.InferenceVerified {
		t.Error("inference_verified must be false")
	}
}

// TestProbeDeployment_UnusableCredentialIsUnconfirmed 钉住：凭据在但材料
// 解不开（bundle 不可用/密文损坏）时，结论是 unconfirmed 且不外泄内部
// 细节。
func TestProbeDeployment_UnusableCredentialIsUnconfirmed(t *testing.T) {
	store, resolver := probeTestFixture("https://api.example.com/v1")
	resolver.errs = map[string]error{"cred-1": errProbeBoom{}}
	svc := NewProbeService(store, resolver, permissiveURL, &http.Client{})
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	j := probeResultJSON(t, res)
	if j.CredentialState != "unusable" {
		t.Errorf("credential_state = %q, want unusable", j.CredentialState)
	}
	if j.ProbeOutcome != "unconfirmed" {
		t.Errorf("probe_outcome = %q, want unconfirmed", j.ProbeOutcome)
	}
	if strings.Contains(j.ErrorSummary, "vault boom") {
		t.Errorf("error_summary leaks internals: %q", j.ErrorSummary)
	}
}

// errProbeBoom 是一个非 domain 错误（可能带内部细节）。
type errProbeBoom struct{}

func (errProbeBoom) Error() string { return "vault boom: internal detail" }
