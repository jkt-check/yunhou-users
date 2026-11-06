// config_errors_test.go — T7「配置行为收口」：配置错误必须在预占/派发前
// 明确失败，绝不静默降级（保存后假装生效 / 静默当 reject）。
//
// 覆盖两类存量脏配置：
//   1. 已发布快照里的不支持 pool_strategy（least_loaded / 通用
//      session_sticky）——旧快照可读，但路由不可调度；模型没有其他候选
//      时在预占前明确失败并告警。
//   2. 存量配额策略仍带未实现的 clamp_if_declared —— 新推理明确报「策略
//      配置不可用」，绝不静默当 reject。

package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// legacyPoolStrategy rewrites the fixture snapshot's route to a legacy
// (known but unsupported) pool strategy — exactly what an older published
// revision carries.
func legacyPoolStrategy(f *fixture, strategy domain.PoolStrategy) {
	routes := f.snap.RoutesByModel[f.modelID]
	if len(routes) == 0 {
		return
	}
	routes[0].PoolStrategy = strategy
}

// captureAlerts swaps the audit-alert channel for the duration of a test
// and returns the collected events.
func captureAlerts(t *testing.T) *[]management.AuditEvent {
	t.Helper()
	prev := management.AuditAlertHook
	var events []management.AuditEvent
	management.AuditAlertHook = func(ctx context.Context, ev management.AuditEvent, cause error) {
		events = append(events, ev)
	}
	t.Cleanup(func() { management.AuditAlertHook = prev })
	return &events
}

// TestGateway_UnsupportedPoolStrategyFailsBeforeAdmission：模型只有一条
// 不支持的 pool_strategy 路由时，请求必须在预占/派发前以明确的配置错误
// 失败 —— 零预占、零出站、零请求行，并且必须告警。
func TestGateway_UnsupportedPoolStrategyFailsBeforeAdmission(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	f := newFixture(t, up, withSnapshotSource(func(f *fixture) SnapshotSource {
		legacyPoolStrategy(f, domain.PoolLeastLoaded)
		return &staticSnapshot{f.snap}
	}))
	alerts := captureAlerts(t)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("an unsupported pool strategy must fail the call with a configuration error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "least_loaded") {
		t.Errorf("the error must name the unsupported configuration: %v", err)
	}
	if !strings.Contains(msg, string(domain.PoolRoundRobin)) {
		t.Errorf("the error must name the supported replacement: %v", err)
	}
	// 预占/派发前失败：零出站、零请求行。
	if n := up.calls.Load(); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (配置错误不得出站)", n)
	}
	var reqs int
	if err := f.db.QueryRow(`SELECT count(*) FROM inference_requests`).Scan(&reqs); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if reqs != 0 {
		t.Errorf("requests = %d, want 0 (必须在预占前失败)", reqs)
	}
	// 告警：路由被标记不可用必须响亮（否则存量脏配置永远无人知晓）。
	if len(*alerts) == 0 {
		t.Error("an unsupported pool strategy must raise an alarm")
	}
}

// TestGateway_DisabledLegacyRouteDoesNotBlockTheCall：存量脏配置里那条
// 路由**被停用**时，它本来就不参与调度 —— 不能因此把整个模型判成「没有
// 可调度路由」。模型仍有支持集内的路由时，请求照常服务（运维侧仍收到
// 告警：那条行迟早会被重新启用）。
func TestGateway_DisabledLegacyRouteDoesNotBlockTheCall(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	f := newFixture(t, up, withSnapshotSource(func(f *fixture) SnapshotSource {
		// 一条**停用**的旧路由：它不参与调度，也不该让模型整体不可用。
		f.snap.RoutesByModel[f.modelID] = append(f.snap.RoutesByModel[f.modelID], domain.ModelRoute{
			ID: "legacy-disabled", ModelID: f.modelID, DeploymentID: f.deploymentID,
			Weight: 1, Enabled: false, PoolStrategy: domain.PoolSessionSticky,
		})
		return &staticSnapshot{f.snap}
	}))
	alerts := captureAlerts(t)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("a disabled legacy route must not block the call: %v", err)
	}
	if n := up.calls.Load(); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
	if len(*alerts) == 0 {
		t.Error("the legacy route must still be alarmed (it will be re-enabled one day)")
	}
}

// TestGateway_ClampIfDeclaredPolicyFailsClearly：存量策略仍带未实现的
// clamp_if_declared 时，新推理必须明确报「策略配置不可用」——绝不静默当
// reject（那会悄悄改变客户的额度语义）。
func TestGateway_ClampIfDeclaredPolicyFailsClearly(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"up",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	f := newFixture(t, up, withOveragePolicy("clamp_if_declared"))

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key,
		domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("a policy with the unimplemented clamp_if_declared mode must fail the call")
	}
	if !strings.Contains(err.Error(), "clamp_if_declared") {
		t.Errorf("the error must name the unsupported policy mode: %v", err)
	}
	// 绝不静默当 reject：不得出站、不得留下请求行。
	if n := up.calls.Load(); n != 0 {
		t.Errorf("upstream calls = %d, want 0 (策略配置不可用不得出站)", n)
	}
	var reqs int
	if err := f.db.QueryRow(`SELECT count(*) FROM inference_requests`).Scan(&reqs); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if reqs != 0 {
		t.Errorf("requests = %d, want 0 (必须在预占前失败)", reqs)
	}
}
