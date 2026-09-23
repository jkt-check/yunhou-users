package repo

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/yunhou/users/internal/model"
)

// plan_benefit_repo.go — plan_benefit_configs / plan_upgrade_rules
// (migration 029，设计 §4.2/§4.3)。套餐 → 权益版本的发布配置是商品的
// "支付配置"：没有配置行的 coding-plan 套餐不可购买；跨档升级必须存在
// 显式规则行。

// PlanBenefitRepo is the read surface for the plan→benefit mapping and the
// configured cross-tier upgrade rules. Writes are operator SQL / future
// admin tasks (Task 15); the purchase path only reads and snapshots.
type PlanBenefitRepo interface {
	// FindByPlanID returns the benefit config for a plan, sql.ErrNoRows
	// when the plan has none (= product not configured for purchase).
	FindByPlanID(ctx context.Context, planID string) (*model.PlanBenefitConfig, error)
	// FindByPlanIDTx shares the caller's transaction connection (the order
	// eligibility tx already holds the plan FOR SHARE lock on it).
	FindByPlanIDTx(ctx context.Context, tx *sqlx.Tx, planID string) (*model.PlanBenefitConfig, error)
	// HasUpgradeRule reports whether a configured cross-tier upgrade
	// from→to exists (plan_upgrade_rules, design §4.2: 商品未配置升级报价
	// 规则时不开放即时跨档升级). Same-plan renewal never needs a rule.
	HasUpgradeRule(ctx context.Context, fromPlanID, toPlanID string) (bool, error)
}

type planBenefitRepo struct{ db *sqlx.DB }

func NewPlanBenefitRepo(db *sqlx.DB) *planBenefitRepo { return &planBenefitRepo{db: db} }

var _ PlanBenefitRepo = (*planBenefitRepo)(nil)

func (r *planBenefitRepo) FindByPlanID(ctx context.Context, planID string) (*model.PlanBenefitConfig, error) {
	var c model.PlanBenefitConfig
	err := r.db.GetContext(ctx, &c, `SELECT * FROM plan_benefit_configs WHERE plan_id = $1`, planID)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *planBenefitRepo) FindByPlanIDTx(ctx context.Context, tx *sqlx.Tx, planID string) (*model.PlanBenefitConfig, error) {
	var c model.PlanBenefitConfig
	err := tx.GetContext(ctx, &c, `SELECT * FROM plan_benefit_configs WHERE plan_id = $1`, planID)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *planBenefitRepo) HasUpgradeRule(ctx context.Context, fromPlanID, toPlanID string) (bool, error) {
	var exists bool
	err := r.db.GetContext(ctx, &exists, `
		SELECT EXISTS(SELECT 1 FROM plan_upgrade_rules WHERE from_plan_id = $1 AND to_plan_id = $2)
	`, fromPlanID, toPlanID)
	return exists, err
}
