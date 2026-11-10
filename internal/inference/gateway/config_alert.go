// config_alert.go — T7:存量脏配置的响亮告警通道（与生命周期门 I-6 同款
// 契约：默认结构化日志 + management.AuditAlertHook，生产可覆写接 on-call）。
//
// 口径：已发布快照里可能残留**未实现**的配置（不支持的 pool_strategy）。
// 这类配置「可读但不可用」——旧快照必须仍能加载（否则整个目录变砖），
// 但路由不得被静默当成已实现的策略。发现它就必须告警：否则存量脏配置
// 永远无人知晓，运营会以为「配了就生效」。

package gateway

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// unsupportedPoolStrategyMessage is the client-visible configuration error
// for a model whose every route uses an unsupported pool strategy. It names
// the offending route and the supported replacement so the operator can fix
// the row in one step (发布预演给出同一个口径).
func unsupportedPoolStrategyMessage(modelID string, routes []domain.ModelRoute) string {
	parts := make([]string, 0, len(routes))
	for _, r := range routes {
		parts = append(parts, fmt.Sprintf("route %s (pool_strategy=%s)", r.ID, r.PoolStrategy))
	}
	return "model " + modelID + " has no dispatchable route: " + strings.Join(parts, ", ") +
		" — " + string(catalog.SupportedPoolStrategy) + " is the only supported pool strategy"
}

// defaultUnsupportedPoolStrategyReporter is the production reporter: one
// structured log line (巡检规则按 UNSUPPORTED_POOL_STRATEGY 命中) plus the
// audit-alert channel. cause 必非 nil.
func defaultUnsupportedPoolStrategyReporter(ctx context.Context, p *domain.Principal, m *domain.Model, routes []domain.ModelRoute, cause error) {
	actor := p.UserID
	if actor == "" {
		actor = p.OperatorSubject
	}
	ids := make([]string, 0, len(routes))
	strategies := make([]string, 0, len(routes))
	for _, r := range routes {
		ids = append(ids, r.ID)
		strategies = append(strategies, string(r.PoolStrategy))
	}
	log.Printf("UNSUPPORTED_POOL_STRATEGY model=%s routes=%s strategies=%s principal_kind=%s billing_account=%s user_id=%s",
		m.ID, strings.Join(ids, ","), strings.Join(strategies, ","), p.Kind, p.BillingAccountID, p.UserID)
	management.AuditAlertHook(ctx, management.AuditEvent{
		Action:     "model.route.pool_strategy_unsupported",
		ObjectType: "model",
		ObjectID:   m.ID,
		Reason: fmt.Sprintf("published routes use an unsupported pool_strategy: routes=%s strategies=%s",
			strings.Join(ids, ","), strings.Join(strategies, ",")),
		ActorUser: actor,
		Detail: map[string]any{
			"route_ids":               ids,
			"pool_strategies":         strategies,
			"supported_pool_strategy": string(catalog.SupportedPoolStrategy),
			"principal_kind":          string(p.Kind),
			"billing_account":         p.BillingAccountID,
		},
	}, cause)
}

// reportUnsupportedPoolStrategy dispatches to the configured hook, falling
// back to the default reporter when unset (zero-value Service safety).
func (s *Service) reportUnsupportedPoolStrategy(ctx context.Context, p *domain.Principal, m *domain.Model, routes []domain.ModelRoute, cause error) {
	if s.OnUnsupportedPoolStrategy != nil {
		s.OnUnsupportedPoolStrategy(ctx, p, m, routes, cause)
		return
	}
	defaultUnsupportedPoolStrategyReporter(ctx, p, m, routes, cause)
}
