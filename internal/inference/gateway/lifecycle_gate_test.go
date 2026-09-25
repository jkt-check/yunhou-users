// lifecycle_gate_test.go — 安全审查 I-6:非 active 模型的直达流量治理。
//
// 口径(产品拍板):deprecated 放行存量(拒新 grant 是 entitlement 层另案);
// retired/draft 拒新请求。两阶段上线:先观察模式(默认)——命中非 active
// 模型只记结构化日志 + 经 OnLifecycleGateHit 告警(生产接到 I-7 的
// AuditAlertHook),不拦截;存量排查确认影响面后才经
// INFERENCE_LIFECYCLE_ENFORCE 切强制模式(403 model_not_allowed)。
// 被拒请求发生在计量之前,不产生任何费用;在途请求 pin 旧快照天然免疫。
package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// completionHandler is the shared non-stream upstream success response.
var completionHandler = func(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-gate","object":"chat.completion","created":1,"model":"glm-4.6-upstream",
		"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
}

// capturedGateEvent records one lifecycle-gate observation.
type capturedGateEvent struct {
	ev      management.AuditEvent
	blocked bool
	cause   error
}

// captureGateHook swaps the service hook for the test duration.
func captureGateHook(f *fixture) *capturedGateEvent {
	cap := &capturedGateEvent{}
	f.gateway.OnLifecycleGateHit = func(ctx context.Context, p *domain.Principal, m *domain.Model, blocked bool, cause error) {
		cap.ev = management.AuditEvent{
			ObjectID:  m.ID,
			ActorUser: p.UserID,
			Detail: map[string]any{
				"lifecycle":       string(m.Lifecycle),
				"billing_account": p.BillingAccountID,
				"api_key_id":      p.APIKeyID,
				"blocked":         blocked,
			},
		}
		cap.blocked = blocked
		cap.cause = cause
	}
	return cap
}

// 决策表(纯函数,无 DB):active 永不触发;deprecated 只观察不拦截;
// retired/draft 在强制模式下拦截。
func TestLifecycleGateDecision(t *testing.T) {
	cases := []struct {
		lc      domain.Lifecycle
		mode    LifecycleGateMode
		observe bool // 是否触发观察事件
		blocked bool
	}{
		{domain.LifecycleActive, LifecycleGateObserve, false, false},
		{domain.LifecycleActive, LifecycleGateEnforce, false, false},
		{domain.LifecycleDeprecated, LifecycleGateObserve, true, false},
		{domain.LifecycleDeprecated, LifecycleGateEnforce, true, false}, // 口径①:放行存量
		{domain.LifecycleRetired, LifecycleGateObserve, true, false},
		{domain.LifecycleRetired, LifecycleGateEnforce, true, true},
		{domain.LifecycleDraft, LifecycleGateObserve, true, false},
		{domain.LifecycleDraft, LifecycleGateEnforce, true, true},
	}
	for _, c := range cases {
		observe, blocked := lifecycleGateDecision(c.lc, c.mode)
		if observe != c.observe || blocked != c.blocked {
			t.Errorf("lifecycleGateDecision(%s, %v) = (%v,%v), want (%v,%v)",
				c.lc, c.mode, observe, blocked, c.observe, c.blocked)
		}
	}
}

// 观察模式(默认):retired 模型直达请求照常成功(计费链完整跑通),
// 但产生一条观察事件(结构化日志 + 告警通道的源头)。
func TestLifecycleGate_Observe_RetiredAllowedAndReported(t *testing.T) {
	up := newUpstream(t, completionHandler)
	f := newFixture(t, up, withModelLifecycle(domain.LifecycleRetired))
	cap := captureGateHook(f)

	p, key := f.principal()
	out, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1 (observe mode must not block)", up.calls.Load())
	}
	if out.RequestID == "" {
		t.Error("empty request id")
	}
	if cap.cause == nil {
		t.Fatal("gate observation not reported")
	}
	if cap.blocked {
		t.Error("blocked = true in observe mode")
	}
	if cap.ev.ObjectID != f.modelID {
		t.Errorf("event object = %q, want %q", cap.ev.ObjectID, f.modelID)
	}
	if cap.ev.Detail["lifecycle"] != string(domain.LifecycleRetired) {
		t.Errorf("detail lifecycle = %v", cap.ev.Detail["lifecycle"])
	}
	if cap.ev.Detail["billing_account"] != f.accountID {
		t.Errorf("detail billing_account = %v, want %q", cap.ev.Detail["billing_account"], f.accountID)
	}
}

// 观察模式:draft 与 deprecated 同样只告警不拦截。
func TestLifecycleGate_Observe_DraftAndDeprecatedAllowed(t *testing.T) {
	for _, lc := range []domain.Lifecycle{domain.LifecycleDraft, domain.LifecycleDeprecated} {
		up := newUpstream(t, completionHandler)
		f := newFixture(t, up, withModelLifecycle(lc))
		cap := captureGateHook(f)

		p, key := f.principal()
		if _, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
			t.Fatalf("lifecycle=%s: ChatCompletions: %v", lc, err)
		}
		if up.calls.Load() != 1 {
			t.Errorf("lifecycle=%s: upstream calls = %d, want 1", lc, up.calls.Load())
		}
		if cap.cause == nil || cap.blocked {
			t.Errorf("lifecycle=%s: observation = %+v blocked=%v, want reported unblocked", lc, cap.ev, cap.blocked)
		}
	}
}

// 强制模式:retired 直达 → 403 model_not_allowed,零上游流量、零请求落库、
// 零计费;事件以 blocked=true 上报。
func TestLifecycleGate_Enforce_RetiredBlocked(t *testing.T) {
	up := newUpstream(t, completionHandler)
	f := newFixture(t, up, withModelLifecycle(domain.LifecycleRetired))
	f.gateway.LifecycleGate = LifecycleGateEnforce
	cap := captureGateHook(f)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if err == nil {
		t.Fatal("expected error in enforce mode")
	}
	if code := domain.CodeOf(err); code != domain.CodeModelNotAllowed {
		t.Fatalf("code = %v, want model_not_allowed (403): %v", code, err)
	}
	if !strings.Contains(err.Error(), "model_not_allowed") {
		t.Errorf("error message = %q, want model_not_allowed marker", err.Error())
	}
	if up.calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0 (blocked before dispatch)", up.calls.Load())
	}
	var cnt int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_requests`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("persisted requests = %d err=%v, want 0 (rejected before admission/metering)", cnt, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inference_ledger_entries`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("ledger entries = %d, want 0 (no charge for gated calls)", cnt)
	}
	if cap.cause == nil || !cap.blocked {
		t.Errorf("observation blocked = %v, want true", cap.blocked)
	}
}

// 强制模式:draft 直达同样被拒。
func TestLifecycleGate_Enforce_DraftBlocked(t *testing.T) {
	up := newUpstream(t, completionHandler)
	f := newFixture(t, up, withModelLifecycle(domain.LifecycleDraft))
	f.gateway.LifecycleGate = LifecycleGateEnforce
	cap := captureGateHook(f)

	p, key := f.principal()
	_, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi"))
	if code := domain.CodeOf(err); code != domain.CodeModelNotAllowed {
		t.Fatalf("code = %v, want model_not_allowed: %v", code, err)
	}
	if up.calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0", up.calls.Load())
	}
	if cap.cause == nil || !cap.blocked {
		t.Errorf("observation blocked = %v, want true", cap.blocked)
	}
}

// 强制模式:deprecated 仍放行存量直达(口径①),但继续上报观察事件。
func TestLifecycleGate_Enforce_DeprecatedAllowed(t *testing.T) {
	up := newUpstream(t, completionHandler)
	f := newFixture(t, up, withModelLifecycle(domain.LifecycleDeprecated))
	f.gateway.LifecycleGate = LifecycleGateEnforce
	cap := captureGateHook(f)

	p, key := f.principal()
	if _, err := f.gateway.ChatCompletions(context.Background(), p, key, domain.ProtocolOpenAIChat, chatReq(false, "hi")); err != nil {
		t.Fatalf("ChatCompletions: %v (deprecated must stay allowed in enforce mode)", err)
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", up.calls.Load())
	}
	if cap.cause == nil || cap.blocked {
		t.Errorf("observation blocked = %v, want reported unblocked", cap.blocked)
	}
}
