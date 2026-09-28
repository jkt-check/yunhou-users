package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
)

// admin_publish_dryrun_test.go — Task 2（R7-N7）publish dry_run 的真实库
// handler 测试。钉住：?dry_run=1 零写库（active revision 仍不存在）、报告
// 含候选集摘要 + 真实探测结论；探测失败（404）只进报告不阻断；缺省
// dry_run 的正式 publish 行为逐字节不变（不跑探测、照常发布）。

type dryRunEnvelope struct {
	Code int `json:"code"`
	Data struct {
		WouldPublish struct {
			NextRevision      int `json:"next_revision"`
			Models            int `json:"models"`
			Providers         int `json:"providers"`
			Deployments       int `json:"deployments"`
			ActiveDeployments int `json:"active_deployments"`
			Routes            int `json:"routes"`
		} `json:"would_publish"`
		Probes []struct {
			DeploymentID string `json:"deployment_id"`
			OK           bool   `json:"ok"`
			Status       int    `json:"status"`
			ErrorSummary string `json:"error_summary"`
		} `json:"probes"`
	} `json:"data"`
	Message string `json:"message"`
}

func postDryRun(t *testing.T, fx probeFixture, query string) (*httptest.ResponseRecorder, dryRunEnvelope) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/catalog/publish?"+query, nil)
	fx.engine.ServeHTTP(w, req)
	var env dryRunEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body %s)", err, w.Body.String())
	}
	return w, env
}

func insertDryRunModel(t *testing.T, fx probeFixture) {
	t.Helper()
	if err := fx.store.InsertModel(context.Background(), &domain.Model{
		ID: "glm-4.6", DisplayName: "GLM 4.6", Lifecycle: domain.LifecycleActive,
		ContextTokens: 200000, MaxOutputTokens: 8192,
		Protocols:       []domain.Protocol{domain.ProtocolOpenAIChat},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
	}); err != nil {
		t.Fatalf("insert model: %v", err)
	}
}

func activeRevisionStatus(t *testing.T, fx probeFixture) int {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/catalog/active", nil)
	fx.engine.ServeHTTP(w, req)
	return w.Code
}

func TestAdminPublishDryRun_AdvisoryReportNoWrite(t *testing.T) {
	fx := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	insertDryRunModel(t, fx)

	w, env := postDryRun(t, fx, "dry_run=1&reason=prelaunch-check")
	if w.Code != http.StatusOK || env.Code != 0 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	wp := env.Data.WouldPublish
	if wp.NextRevision != 1 || wp.Models != 1 || wp.Providers != 1 || wp.Deployments != 1 || wp.ActiveDeployments != 1 {
		t.Fatalf("would_publish = %+v", wp)
	}
	if len(env.Data.Probes) != 1 || !env.Data.Probes[0].OK || env.Data.Probes[0].Status != http.StatusOK {
		t.Fatalf("probes = %+v", env.Data.Probes)
	}
	if strings.Contains(w.Body.String(), fx.secret) {
		t.Fatalf("response leaks secret: %s", w.Body.String())
	}
	// 零写库：dry_run 后 active revision 仍不存在。
	if code := activeRevisionStatus(t, fx); code != http.StatusNotFound {
		t.Fatalf("dry run must not publish; /catalog/active = %d", code)
	}

	// 缺省 dry_run：正式 publish 行为不变（不跑探测、照常发布）。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/admin/catalog/publish?reason=go-live", nil)
	fx.engine.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"revision":1`) {
		t.Fatalf("real publish changed behavior: status=%d body=%s", w2.Code, w2.Body.String())
	}
	if code := activeRevisionStatus(t, fx); code != http.StatusOK {
		t.Fatalf("after real publish /catalog/active = %d", code)
	}
}

// TestAdminPublishDryRun_ProbeFailureNeverBlocks：缺 /v1 的 404 探测失败
// 只进报告（ok:false + status:404），dry_run 本身仍 200，且不写库。
func TestAdminPublishDryRun_ProbeFailureNeverBlocks(t *testing.T) {
	fx := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	insertDryRunModel(t, fx)

	w, env := postDryRun(t, fx, "dry_run=1&reason=prelaunch-check")
	if w.Code != http.StatusOK {
		t.Fatalf("probe failure must not fail the dry run: status=%d body=%s", w.Code, w.Body.String())
	}
	if len(env.Data.Probes) != 1 || env.Data.Probes[0].OK || env.Data.Probes[0].Status != http.StatusNotFound {
		t.Fatalf("probes = %+v", env.Data.Probes)
	}
	if !strings.Contains(env.Data.Probes[0].ErrorSummary, "404") {
		t.Fatalf("error_summary %q must carry the real status", env.Data.Probes[0].ErrorSummary)
	}
	if code := activeRevisionStatus(t, fx); code != http.StatusNotFound {
		t.Fatalf("dry run must not publish; /catalog/active = %d", code)
	}
}

// TestAdminPublishDryRun_UnwiredProberFailsClosed 钉住评审修复（R7-N7 轮1
// finding1）：handler 未装配 prober 时，h.prober（*ProbeService 具体类型）
// 传入 DeploymentProber 接口是 typed-nil——manager 层 prober==nil 检查打不
// 中，真实路径会在首个 active deployment 上 nil-pointer panic。修复后：
// handler 在转换前判空，返回 500 envelope 而非 panic（本测试引擎故意不加
// gin.Recovery，panic 即测试崩）。库里保留 1 个 active deployment 确保
// 未修复时必踩循环。
func TestAdminPublishDryRun_UnwiredProberFailsClosed(t *testing.T) {
	fx := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	insertDryRunModel(t, fx)

	gin.SetMode(gin.TestMode)
	engine := gin.New() // 无 Recovery：panic 直接炸测试
	h := httpapi.NewAdminModelsHandler(management.NewCatalogManager(
		catalog.NewService(fx.store), nil, func(context.Context, string) error { return nil }))
	group := engine.Group("/admin")
	h.RegisterReadOnly(group)
	h.RegisterWrite(group)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/catalog/publish?dry_run=1&reason=x", nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unwired prober must fail closed with 500, got %d body=%s", w.Code, w.Body.String())
	}
	if code := activeRevisionStatus(t, fx); code != http.StatusNotFound {
		t.Fatalf("dry run must not publish; /catalog/active = %d", code)
	}
}
