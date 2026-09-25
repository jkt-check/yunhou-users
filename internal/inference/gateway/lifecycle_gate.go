// lifecycle_gate.go — 安全审查 I-6:非 active 模型的直达流量治理。
//
// lifecycle 门禁此前只作用于列表层(Sellable),retired/draft 模型不出现在
// /v1/models 却仍会被点名直连。两阶段上线:
//
//   - 观察模式(默认):任何非 active 模型被直达时记一条结构化
//     LIFECYCLE_GATE_HIT 日志并经 OnLifecycleGateHit 告警(生产默认接到
//     I-7 的 management.AuditAlertHook),不拦截——先摸清存量影响面。
//   - 强制模式(INFERENCE_LIFECYCLE_ENFORCE=1):retired/draft 拒新请求,
//     403 model_not_allowed(不伪装 404);deprecated 按产品口径放行存量。
//
// 拒绝发生在计量之前:零预占、零计费;在途请求 pin 旧快照,天然免疫。
package gateway

import (
	"context"
	"fmt"
	"log"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// LifecycleGateMode selects observe vs enforce behaviour for non-active
// model traffic. The zero value is observe — fail-open by explicit choice
// until the inventory (观察期排查) proves the blast radius is containable.
type LifecycleGateMode int

const (
	// LifecycleGateObserve logs + alerts on every non-active model hit
	// but never blocks (第一阶段:存量排查).
	LifecycleGateObserve LifecycleGateMode = iota
	// LifecycleGateEnforce additionally rejects retired/draft models with
	// 403 model_not_allowed (第二阶段:排查确认影响面后开启).
	LifecycleGateEnforce
)

func (m LifecycleGateMode) String() string {
	if m == LifecycleGateEnforce {
		return "enforce"
	}
	return "observe"
}

// lifecycleGateDecision maps (lifecycle, mode) to (observe, blocked).
// active never triggers anything; deprecated is observed but never blocked
// (产品口径:放行存量 entitlement;拒新 grant 属 entitlement 层另案);
// retired/draft block only in enforce mode. Empty lifecycle is treated as
// active and intentionally silent (DB 有 NOT NULL DEFAULT 'draft' + CHECK
// 兜底,空值只可能来自手工快照);非空未知值只观察不拦截——对不认识的数据
// 拿遥测胜过硬失败。
func lifecycleGateDecision(lc domain.Lifecycle, mode LifecycleGateMode) (observe, blocked bool) {
	switch lc {
	case "", domain.LifecycleActive:
		return false, false
	case domain.LifecycleDeprecated:
		return true, false
	case domain.LifecycleRetired, domain.LifecycleDraft:
		return true, mode == LifecycleGateEnforce
	default:
		return true, false
	}
}

// defaultLifecycleGateReporter is the production OnLifecycleGateHit: one
// structured log line (巡检规则按 LIFECYCLE_GATE_HIT 命中) plus the I-7
// audit-alert channel — a non-active model taking traffic is exactly the
// class of anomaly AuditAlertHook exists for. cause 必非 nil。
// 归因说明:Principal 只携带 user/operator 身份,没有 app 腿——API key 与
// kaya JWT 调用方落在 user 腿(operator 调用方经 OperatorSubject 兜底),
// ActorApp 恒为空;双腿归因待 principal 携带服务身份后再补。
func defaultLifecycleGateReporter(ctx context.Context, p *domain.Principal, m *domain.Model, blocked bool, cause error) {
	actor := p.UserID
	if actor == "" {
		actor = p.OperatorSubject
	}
	log.Printf("LIFECYCLE_GATE_HIT model=%s lifecycle=%s blocked=%t principal_kind=%s billing_account=%s api_key_id=%s user_id=%s operator=%s",
		m.ID, m.Lifecycle, blocked, p.Kind,
		p.BillingAccountID, p.APIKeyID, p.UserID, p.OperatorSubject)
	management.AuditAlertHook(ctx, management.AuditEvent{
		Action:     "model.lifecycle.gate_hit",
		ObjectType: "model",
		ObjectID:   m.ID,
		Reason:     fmt.Sprintf("non-active model served via gateway: lifecycle=%s blocked=%t", m.Lifecycle, blocked),
		ActorUser:  actor,
		Detail: map[string]any{
			"lifecycle":       string(m.Lifecycle),
			"blocked":         blocked,
			"principal_kind":  string(p.Kind),
			"billing_account": p.BillingAccountID,
			"api_key_id":      p.APIKeyID,
		},
	}, cause)
}

// reportLifecycleGate dispatches to the configured hook, falling back to
// the default reporter when unset (zero-value Service safety).
func (s *Service) reportLifecycleGate(ctx context.Context, p *domain.Principal, m *domain.Model, blocked bool, cause error) {
	if s.OnLifecycleGateHit != nil {
		s.OnLifecycleGateHit(ctx, p, m, blocked, cause)
		return
	}
	defaultLifecycleGateReporter(ctx, p, m, blocked, cause)
}
