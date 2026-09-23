package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/httpapi"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
)

// admin_models_reason_test.go — 评审轮2 finding6：写端点 DTO/查询参数的
// reason 端到端透传到 CatalogManager 审计事件（此前参数位一直空转）。
// DB 测试（无 postgres 时 skip），只需编译过即可在本地验证。

// captureAudit 是收集审计事件的内存记录器（management.AuditRecorder）。
type captureAudit struct {
	mu     sync.Mutex
	events []management.AuditEvent
}

func (r *captureAudit) Record(_ context.Context, ev management.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *captureAudit) last() management.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

// newReasonTestServer 与 newTestServer 同形，但接线内存审计记录器以断言
// reason 透传；只清模型表（本文件只演练模型写端点）。
func newReasonTestServer(t *testing.T) (*gin.Engine, *captureAudit) {
	t.Helper()
	if testDSN == "" {
		t.Skip("skip: no postgres available")
	}
	db, err := sqlx.Connect("postgres", testDSN)
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`TRUNCATE inference_models RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	rec := &captureAudit{}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := httpapi.NewAdminModelsHandler(management.NewCatalogManager(
		catalog.NewService(postgres.NewStore(db)), rec,
		func(context.Context, string) error { return nil })) // permissive egress stub
	group := engine.Group("/admin")
	h.RegisterWrite(group)
	return engine, rec
}

func TestAdminModels_ReasonFlowsToAudit(t *testing.T) {
	engine, rec := newReasonTestServer(t)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body == "" {
			rdr = strings.NewReader("")
		} else {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	// 带 reason 的创建：审计事件 model.create 携带该 reason。
	w := call(http.MethodPost, "/admin/models",
		`{"id":"m-reason","display_name":"M","context_tokens":1024,"max_output_tokens":128,"reason":"promo onboarding"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if ev := rec.last(); ev.Action != "model.create" || ev.Reason != "promo onboarding" {
		t.Fatalf("audit = %+v, want model.create with reason", ev)
	}

	// 带 reason 的生命周期变更：审计事件 model.lifecycle 携带该 reason。
	w = call(http.MethodPost, "/admin/models/m-reason/lifecycle",
		`{"lifecycle":"deprecated","reason":"superseded by v2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("lifecycle = %d: %s", w.Code, w.Body.String())
	}
	if ev := rec.last(); ev.Action != "model.lifecycle" || ev.Reason != "superseded by v2" {
		t.Fatalf("audit = %+v, want model.lifecycle with reason", ev)
	}

	// 无请求体的 DELETE 走 ?reason= 查询参数。
	w = call(http.MethodDelete, "/admin/models/m-reason?reason=retired%20cleanup", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if ev := rec.last(); ev.Action != "model.delete" || ev.Reason != "retired cleanup" {
		t.Fatalf("audit = %+v, want model.delete with reason", ev)
	}

	// 未传 reason：空串（现状兼容），不报错。
	w = call(http.MethodPost, "/admin/models",
		`{"id":"m-noreason","display_name":"M2","context_tokens":1024,"max_output_tokens":128}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create without reason = %d: %s", w.Code, w.Body.String())
	}
	if ev := rec.last(); ev.Reason != "" {
		t.Fatalf("audit reason = %q, want empty for absent reason", ev.Reason)
	}
}
