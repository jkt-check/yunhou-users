package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/credentials"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
	"github.com/yunhou/users/internal/inference/providers"
)

// admin_probe_test.go — Task 1（R7-N7）探测端点的真实库 handler 测试：
// vault 真解密 + egress 真校验（loopback 经 allowlist 放行）+ httptest stub
// 上游。钉住：200 → ok=true；404（缺 /v1 事故形状）→ ok=false + status=404；
// 未知 deployment → 404 envelope；响应体绝不含 secret 字节。

// probeFixture 是探测测试的装配结果。
type probeFixture struct {
	engine *gin.Engine
	store  *postgres.Store
	depID  string
	secret string
}

// newProbeFixture 建库行（deployment 行直插 store，刻意绕过写路径的
// loopback URL 校验——与 gateway 测试同一先例）+ 挂载探测端点的 gin 引擎。
// upstreamPathPrefix 非空时 stub 只认该前缀（模拟缺 /v1 的上游）。
func newProbeFixture(t *testing.T, upstream http.Handler) probeFixture {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`TRUNCATE inference_audit_log, inference_upstream_accounts,
		inference_credentials, inference_config_revisions, inference_model_routes,
		inference_deployments, inference_providers, inference_models
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	ctx := context.Background()
	store := postgres.NewStore(db)
	secret := "sk-probe-secret-do-not-leak"

	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	// localhost 通过 catalog 的 base_url 校验（非字面 IP），dial 落到
	// allowlist 内的 127.0.0.1。
	baseURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	prov := &domain.Provider{Code: "zhipu", DisplayName: "Zhipu", AccessType: domain.AccessOfficialAPI, Status: "active"}
	if err := store.InsertProvider(ctx, prov); err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	dep := &domain.Deployment{
		ProviderID: prov.ID, UpstreamModel: "glm-upstream", BaseURL: baseURL,
		Protocol:       domain.ProtocolOpenAIChat,
		ConnectTimeout: 2 * time.Second, RequestTimeout: 5 * time.Second,
		Status: domain.DeploymentActive,
	}
	if err := store.InsertDeployment(ctx, dep); err != nil {
		t.Fatalf("insert deployment: %v", err)
	}

	vault, err := credentials.NewVault(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	credSvc := credentials.NewService(vault, store, store)
	if _, err := credSvc.Create(ctx, credentials.Operator{UserID: uuid.NewString(), AppID: "test"},
		prov.ID, "main", "api_key", secret, "seed", nil); err != nil {
		t.Fatalf("credential: %v", err)
	}
	egress, err := credentials.NewEgressValidator([]string{"127.0.0.0/8", "::1/128"})
	if err != nil {
		t.Fatalf("egress: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := httpapi.NewAdminModelsHandler(management.NewCatalogManager(
		catalog.NewService(store), nil, egress.ValidateURL))
	h.SetProber(management.NewProbeService(store, credSvc, egress.ValidateURL,
		providers.NewHTTPClient(egress)))
	group := engine.Group("/admin")
	h.RegisterReadOnly(group)
	h.RegisterWrite(group)
	return probeFixture{engine: engine, store: store, depID: dep.ID, secret: secret}
}

type probeEnvelope struct {
	Code    int `json:"code"`
	Data    struct {
		DeploymentID string `json:"deployment_id"`
		OK           bool   `json:"ok"`
		Status       int    `json:"status"`
		LatencyMS    int64  `json:"latency_ms"`
		ErrorSummary string `json:"error_summary"`
	} `json:"data"`
	Message string `json:"message"`
}

func postProbe(t *testing.T, fx probeFixture, path string) (*httptest.ResponseRecorder, probeEnvelope) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	fx.engine.ServeHTTP(w, req)
	var env probeEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body %s)", err, w.Body.String())
	}
	return w, env
}

func TestAdminProbeDeployment_OK(t *testing.T) {
	fx := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe-secret-do-not-leak" {
			t.Errorf("upstream auth header = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))

	w, env := postProbe(t, fx, "/admin/deployments/"+fx.depID+"/probe")
	if w.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("status=%d envelope=%+v body=%s", w.Code, env, w.Body.String())
	}
	if !env.Data.OK || env.Data.Status != http.StatusOK || env.Data.DeploymentID != fx.depID {
		t.Fatalf("probe result = %+v", env.Data)
	}
	if strings.Contains(w.Body.String(), fx.secret) {
		t.Fatalf("response leaks secret: %s", w.Body.String())
	}
}

// TestAdminProbeDeployment_MissingV1Shape：stub 只认 /v1 前缀，deployment
// base_url 缺 /v1 → 探测如实 404（k3 事故形状的预发布拦截）。
func TestAdminProbeDeployment_MissingV1Shape(t *testing.T) {
	fx := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	w, env := postProbe(t, fx, "/admin/deployments/"+fx.depID+"/probe")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if env.Data.OK || env.Data.Status != http.StatusNotFound {
		t.Fatalf("probe result = %+v", env.Data)
	}
	if !strings.Contains(env.Data.ErrorSummary, "404") {
		t.Fatalf("error_summary %q must carry the real status", env.Data.ErrorSummary)
	}
	if strings.Contains(w.Body.String(), fx.secret) {
		t.Fatalf("response leaks secret: %s", w.Body.String())
	}
}

func TestAdminProbeDeployment_NotFound(t *testing.T) {
	fx := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	w, _ := postProbe(t, fx, "/admin/deployments/00000000-0000-0000-0000-000000000000/probe")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
