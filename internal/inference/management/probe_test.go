package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// probe_test.go — Task 1（R7-N7）失败测试先行：deployment 级上游连通性探测。
// stub upstream 用 httptest.Server；resolver/store 为手搓 fake。核心钉住：
// 200 → ok=true + latency；404（缺 /v1 事故形状）→ ok=false + status=404；
// 拒绝/超时 → ok=false + error_summary；凭据解析失败 → ok=false 且响应绝不
// 含 secret 字节；永不写库（fake store 无写方法可调用）。

type fakeProbeStore struct {
	deployments map[string]domain.Deployment
	credentials []domain.Credential
}

func (f *fakeProbeStore) GetDeployment(ctx context.Context, id string) (*domain.Deployment, error) {
	d, ok := f.deployments[id]
	if !ok {
		return nil, domain.NewError(domain.CodeNotFound, "deployment")
	}
	cp := d
	return &cp, nil
}

func (f *fakeProbeStore) ListCredentials(ctx context.Context, providerID string, limit int) ([]domain.Credential, error) {
	out := []domain.Credential{}
	for _, c := range f.credentials {
		if c.ProviderID == providerID {
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeBearerResolver 记录 pinGeneration 以钉住代次钉扎语义。
type fakeBearerResolver struct {
	tokens map[string]string
	errs   map[string]error
	pins   map[string]int64
}

func (f *fakeBearerResolver) ResolveBearerToken(ctx context.Context, id string, pinGeneration *int64) (string, *domain.Credential, error) {
	if f.pins == nil {
		f.pins = map[string]int64{}
	}
	if pinGeneration != nil {
		f.pins[id] = *pinGeneration
	}
	if err := f.errs[id]; err != nil {
		return "", nil, err
	}
	tok, ok := f.tokens[id]
	if !ok {
		return "", nil, domain.NewError(domain.CodeNotFound, "credential")
	}
	return tok, &domain.Credential{ID: id}, nil
}

func probeTestDeployment(id, baseURL string) domain.Deployment {
	return domain.Deployment{
		ID: id, ProviderID: "prov-1", UpstreamModel: "glm-upstream",
		BaseURL: baseURL, Protocol: domain.ProtocolOpenAIChat,
		ConnectTimeout: time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
}

func probeTestFixture(baseURL string) (*fakeProbeStore, *fakeBearerResolver) {
	store := &fakeProbeStore{
		deployments: map[string]domain.Deployment{"dep-1": probeTestDeployment("dep-1", baseURL)},
		credentials: []domain.Credential{
			{ID: "cred-1", ProviderID: "prov-1", AuthType: "api_key", Status: "active", Generation: 7},
		},
	}
	resolver := &fakeBearerResolver{tokens: map[string]string{"cred-1": "sk-probe-secret"}}
	return store, resolver
}

func permissiveURL(context.Context, string) error { return nil }

func TestProbeDeployment_OK(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("unexpected probe request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.OK || res.Status != http.StatusOK {
		t.Fatalf("want ok=true status=200, got %+v", res)
	}
	if res.ErrorSummary != "" {
		t.Fatalf("unexpected error_summary %q", res.ErrorSummary)
	}
	if gotAuth != "Bearer sk-probe-secret" {
		t.Fatalf("upstream auth header = %q", gotAuth)
	}
	if resolver.pins["cred-1"] != 7 {
		t.Fatalf("generation pinning = %d, want 7", resolver.pins["cred-1"])
	}
}

// TestProbeDeployment_MissingV1Shape 钉住 k3 事故形状：base_url 缺 /v1 →
// 上游 404 → ok=false + status=404 + 摘要含真实状态码。
func TestProbeDeployment_MissingV1Shape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL) // 刻意不带 /v1
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || res.Status != http.StatusNotFound {
		t.Fatalf("want ok=false status=404, got %+v", res)
	}
	if !strings.Contains(res.ErrorSummary, "404") {
		t.Fatalf("error_summary %q must carry the real status", res.ErrorSummary)
	}
}

func TestProbeDeployment_ConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭：dial 必拒绝

	store, resolver := probeTestFixture(url)
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || res.Status != 0 || res.ErrorSummary == "" {
		t.Fatalf("want ok=false status=0 summary set, got %+v", res)
	}
}

func TestProbeDeployment_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	svc.Timeout = 50 * time.Millisecond
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || !strings.Contains(res.ErrorSummary, "timed out") {
		t.Fatalf("want timeout summary, got %+v", res)
	}
}

// TestProbeDeployment_SecretNeverLeaks：上游错误体回显请求头也不许把
// secret 带进 error_summary（响应体只丢弃，永不引用）。
func TestProbeDeployment_SecretNeverLeaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":"bad key %s"}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || res.Status != http.StatusUnauthorized {
		t.Fatalf("want ok=false status=401, got %+v", res)
	}
	if strings.Contains(res.ErrorSummary, "sk-probe-secret") {
		t.Fatalf("error_summary leaks secret: %q", res.ErrorSummary)
	}
}

// TestProbeDeployment_ResolveFailureNoLeak：凭据解析失败的非 domain 错误
// （可能含内部细节）一律折叠为固定文案。
func TestProbeDeployment_ResolveFailureNoLeak(t *testing.T) {
	store, resolver := probeTestFixture("https://api.example.com/v1")
	resolver.errs = map[string]error{"cred-1": errors.New("vault boom: key bytes sk-probe-secret corrupt")}
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || res.ErrorSummary == "" {
		t.Fatalf("want ok=false with summary, got %+v", res)
	}
	if strings.Contains(res.ErrorSummary, "sk-probe-secret") || strings.Contains(res.ErrorSummary, "vault boom") {
		t.Fatalf("error_summary leaks internals: %q", res.ErrorSummary)
	}
}

func TestProbeDeployment_NoCredential(t *testing.T) {
	store, resolver := probeTestFixture("https://api.example.com/v1")
	store.credentials = nil
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || !strings.Contains(res.ErrorSummary, "credential") {
		t.Fatalf("want no-credential summary, got %+v", res)
	}
}

// TestProbeDeployment_RevokedSkippedRotatingUsed：revoked 凭据不参与探测，
// rotating 代次是合法回退（与 ResolveSecret 可派发口径一致）。
func TestProbeDeployment_RevokedSkippedRotatingUsed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	store.credentials = []domain.Credential{
		{ID: "cred-old", ProviderID: "prov-1", AuthType: "api_key", Status: "revoked", Generation: 3},
		{ID: "cred-rot", ProviderID: "prov-1", AuthType: "api_key", Status: "rotating", Generation: 4},
	}
	resolver.tokens = map[string]string{"cred-rot": "sk-rotating"}
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.OK {
		t.Fatalf("want ok=true via rotating credential, got %+v", res)
	}
	if resolver.pins["cred-rot"] != 4 {
		t.Fatalf("rotating generation pinning = %d, want 4", resolver.pins["cred-rot"])
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestProbeDeployment_EgressRejected：egress 拒绝即失败且绝不出站（SSRF 防线）。
func TestProbeDeployment_EgressRejected(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, resolver := probeTestFixture(srv.URL)
	svc := NewProbeService(store, resolver,
		func(context.Context, string) error { return errors.New("policy: host not allowed") }, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || !strings.Contains(res.ErrorSummary, "egress") {
		t.Fatalf("want egress rejection summary, got %+v", res)
	}
	if hits.Load() != 0 {
		t.Fatalf("no outbound call allowed on egress rejection, hits=%d", hits.Load())
	}
}

// TestProbeDeployment_EgressNilFailsClosed：未装配 validator 时拒绝一切
// 非空 base_url（与 CatalogManager 写路径同一 fail-closed 口径）。
func TestProbeDeployment_EgressNilFailsClosed(t *testing.T) {
	store, resolver := probeTestFixture("https://api.example.com/v1")
	svc := NewProbeService(store, resolver, nil, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || res.ErrorSummary == "" {
		t.Fatalf("nil egress validator must fail closed, got %+v", res)
	}
}

func TestProbeDeployment_EmptyBaseURL(t *testing.T) {
	store, resolver := probeTestFixture("")
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	res, err := svc.ProbeDeployment(context.Background(), "dep-1")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.OK || !strings.Contains(res.ErrorSummary, "base_url") {
		t.Fatalf("want empty base_url summary, got %+v", res)
	}
}

func TestProbeDeployment_NotFound(t *testing.T) {
	store, resolver := probeTestFixture("https://api.example.com/v1")
	svc := NewProbeService(store, resolver, permissiveURL, nil)
	if _, err := svc.ProbeDeployment(context.Background(), "dep-unknown"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("want CodeNotFound, got %v", err)
	}
}
