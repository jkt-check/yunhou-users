// pricing_preview.go — 价格/额度策略变更的影响预览（Task 15，设计 §9.2
// /admin/model-prices、/admin/quota-policies 的"影响预览"能力）。
//
// 口径：
//   - 纯只读：预览不落库、不改任何发布状态；生效时间只来自请求输入与
//     既有版本的生效区间。
//   - 生效语义如实展示：新价格版本按 [effective_from, effective_to) 生效；
//     已入场请求钉住入场时的 price/policy revision（031 持久化准入绑定），
//     在途与历史请求的计费永远不受变更影响——"旧订阅保留版本"是结构性
//     事实，预览里显式标注并给出在途数量。
//   - 受影响面来自真实数据：覆盖该模型的活跃权益数、经订阅来源关联到的
//     套餐（跨域只读，Task 11 先例）、引用该策略当前发布版的活跃权益数。
//   - 金额一律 DecimalInt64 语义（int64 微单位）+ ISO-4217 币种；预览的
//     价格形状过 accounting.PriceVersion.Validate（与落库 CHECK 同一规则）。

package management

import (
	"context"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// PriceVersionInfo is the management-side view of one stored price revision
// （避免 management → postgres 依赖；与 postgres.PriceVersion 同形）.
type PriceVersionInfo struct {
	ID                string
	ModelID           string
	Kind              string
	Currency          string
	InputPerMtok      int64
	CacheReadPerMtok  int64
	CacheWritePerMtok int64
	OutputPerMtok     int64
	Revision          int
	EffectiveFrom     time.Time
	EffectiveTo       *time.Time
}

// PolicyVersionInfo is the management-side view of one stored policy
// revision （窗口限额 nil = 该窗口禁用，≠ 无限）.
type PolicyVersionInfo struct {
	ID               string
	Name             string
	Revision         int
	ModelIDs         []string
	FiveHourLimit    *int64
	WeeklyLimit      *int64
	MonthlyLimit     *int64
	RPMLimit         *int
	TPMLimit         *int64
	ConcurrencyLimit *int
	OveragePolicy    string
	Status           string
}

// PricingPreviewStore is the read surface the preview needs; satisfied by
// inference/postgres.Store.
type PricingPreviewStore interface {
	GetModel(ctx context.Context, id string) (*domain.Model, error)
	LatestPriceVersionInfo(ctx context.Context, modelID, kind string, at time.Time) (*PriceVersionInfo, error)
	LatestPolicyVersionByName(ctx context.Context, name string) (*PolicyVersionInfo, error)
	// CountActiveEntitlementsCoveringModel counts active entitlements whose
	// explicit model set contains modelID.
	CountActiveEntitlementsCoveringModel(ctx context.Context, modelID string) (int64, error)
	// AffectedPlansForModel lists the plans reachable from active
	// subscription-sourced entitlements covering modelID (跨域只读).
	AffectedPlansForModel(ctx context.Context, modelID string) ([]AffectedPlan, error)
	// CountActiveEntitlementsUsingPolicy counts active entitlements pinned
	// to one policy version id.
	CountActiveEntitlementsUsingPolicy(ctx context.Context, policyVersionID string) (int64, error)
	// CountInFlightRequestsForModel counts non-terminal requests of the
	// model (它们钉住了入场时的价格/策略版本).
	CountInFlightRequestsForModel(ctx context.Context, modelID string) (int64, error)
}

// AffectedPlan is one plan reached through active entitlements.
type AffectedPlan struct {
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name"`
	Accounts int64  `json:"accounts"` // 该套餐下覆盖此模型的活跃权益数
}

// PricingPreviewService assembles change-impact previews.
type PricingPreviewService struct {
	store PricingPreviewStore
	clock domain.Clock
}

// NewPricingPreviewService builds the service; nil clock uses UTC.
func NewPricingPreviewService(store PricingPreviewStore, clock domain.Clock) *PricingPreviewService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &PricingPreviewService{store: store, clock: clock}
}

// PriceChangePreview is the proposed price change (per-1M-token micro
// rates; sale_credit 无币种，sale_money/upstream_cost 必填币种).
type PriceChangePreview struct {
	ModelID           string
	Kind              string // sale_credit | sale_money | upstream_cost
	Currency          string
	InputPerMtok      int64
	CacheReadPerMtok  int64
	CacheWritePerMtok int64
	OutputPerMtok     int64
	EffectiveFrom     time.Time // zero = 立即（服务时钟 now）
	EffectiveTo       *time.Time
}

// PriceVersionView renders one stored/proposed revision for comparison.
type PriceVersionView struct {
	Revision          int        `json:"revision"`
	PriceVersionID    string     `json:"price_version_id,omitempty"` // 既有版本才有
	Currency          string     `json:"currency,omitempty"`
	InputPerMtok      string     `json:"input_micros_per_mtok"`
	CacheReadPerMtok  string     `json:"cache_read_micros_per_mtok"`
	CacheWritePerMtok string     `json:"cache_write_micros_per_mtok"`
	OutputPerMtok     string     `json:"output_micros_per_mtok"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	EffectiveTo       *time.Time `json:"effective_to,omitempty"`
}

// PriceChangePreviewResult is the assembled impact preview.
type PriceChangePreviewResult struct {
	ServerTime    time.Time `json:"server_time"`
	ModelID       string    `json:"model_id"`
	ModelLifecycle string   `json:"model_lifecycle"`
	Kind          string    `json:"kind"`
	EffectiveFrom time.Time `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	// CurrentEffective 是生效时刻被替换的版本；null = 该时刻尚无有效版本
	// （新定价——此前不可售/不可计费）。
	CurrentEffective *PriceVersionView `json:"current_effective"`
	Proposed         PriceVersionView  `json:"proposed"`
	// 受影响面。
	AffectedAccounts int64          `json:"affected_accounts"` // 覆盖该模型的活跃权益
	AffectedPlans    []AffectedPlan `json:"affected_plans"`
	// 在途请求钉住旧版本（数量 + 结构性说明）。
	InFlightPinnedRequests int64 `json:"inflight_pinned_requests"`
	// ExistingSubscriptionsKeepVersion: 已入场/已结算请求永远按钉住的版本
	// 计费；新请求自 effective_from 起用新版本。结构性事实，恒为 true。
	ExistingSubscriptionsKeepVersion bool `json:"existing_subscriptions_keep_version"`
}

// PreviewPriceChange validates the proposed revision and assembles the
// impact view. kind/currency/rates 过 accounting 纯校验；effective 区间
// [from, to) 边界属新版本（与 ResolvePrice 同一语义）。
func (s *PricingPreviewService) PreviewPriceChange(ctx context.Context, cmd PriceChangePreview) (*PriceChangePreviewResult, error) {
	now := s.clock.Now().UTC()
	if cmd.EffectiveFrom.IsZero() {
		cmd.EffectiveFrom = now
	}
	cmd.EffectiveFrom = cmd.EffectiveFrom.UTC()
	kind := accounting.PriceKind(cmd.Kind)
	pure := accounting.PriceVersion{
		ModelID: cmd.ModelID, Kind: kind, Currency: cmd.Currency,
		Revision: 1, // 占位：Validate 要求 >0；真实 revision 由存储侧分配
		EffectiveFrom: cmd.EffectiveFrom, EffectiveTo: cmd.EffectiveTo,
	}
	if err := pure.Validate(); err != nil {
		return nil, err
	}
	for _, rate := range []int64{cmd.InputPerMtok, cmd.CacheReadPerMtok, cmd.CacheWritePerMtok, cmd.OutputPerMtok} {
		if rate < 0 {
			return nil, domain.NewError(domain.CodeInvalidInput, "pricing preview: rates must be >= 0")
		}
	}
	m, err := s.store.GetModel(ctx, cmd.ModelID)
	if err != nil {
		return nil, err
	}

	res := &PriceChangePreviewResult{
		ServerTime: now, ModelID: m.ID, ModelLifecycle: string(m.Lifecycle),
		Kind: string(kind), EffectiveFrom: cmd.EffectiveFrom, EffectiveTo: cmd.EffectiveTo,
		Proposed: PriceVersionView{
			Revision: 0, // 未落库——revision 由发布时分配，预览不编造
			Currency:  cmd.Currency,
			InputPerMtok:      int64Str(cmd.InputPerMtok),
			CacheReadPerMtok:  int64Str(cmd.CacheReadPerMtok),
			CacheWritePerMtok: int64Str(cmd.CacheWritePerMtok),
			OutputPerMtok:     int64Str(cmd.OutputPerMtok),
			EffectiveFrom: cmd.EffectiveFrom, EffectiveTo: cmd.EffectiveTo,
		},
		ExistingSubscriptionsKeepVersion: true,
	}
	current, err := s.store.LatestPriceVersionInfo(ctx, cmd.ModelID, cmd.Kind, cmd.EffectiveFrom)
	if err == nil {
		res.CurrentEffective = &PriceVersionView{
			Revision: current.Revision, PriceVersionID: current.ID, Currency: current.Currency,
			InputPerMtok:      int64Str(current.InputPerMtok),
			CacheReadPerMtok:  int64Str(current.CacheReadPerMtok),
			CacheWritePerMtok: int64Str(current.CacheWritePerMtok),
			OutputPerMtok:     int64Str(current.OutputPerMtok),
			EffectiveFrom: current.EffectiveFrom, EffectiveTo: current.EffectiveTo,
		}
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return nil, err
	}
	if res.AffectedAccounts, err = s.store.CountActiveEntitlementsCoveringModel(ctx, cmd.ModelID); err != nil {
		return nil, err
	}
	if res.AffectedPlans, err = s.store.AffectedPlansForModel(ctx, cmd.ModelID); err != nil {
		return nil, err
	}
	if res.InFlightPinnedRequests, err = s.store.CountInFlightRequestsForModel(ctx, cmd.ModelID); err != nil {
		return nil, err
	}
	return res, nil
}

// PolicyChangePreview is the proposed quota-policy change, addressed by
// policy NAME (the immutable revisions under one name form its history).
type PolicyChangePreview struct {
	Name             string
	ModelIDs         []string
	FiveHourLimit    *int64
	WeeklyLimit      *int64
	MonthlyLimit     *int64
	RPMLimit         *int
	TPMLimit         *int64
	ConcurrencyLimit *int
	OveragePolicy    string // reject | clamp_if_declared | allow_overage
}

// PolicyChangePreviewResult is the policy impact view.
type PolicyChangePreviewResult struct {
	ServerTime time.Time `json:"server_time"`
	Name       string    `json:"name"`
	// CurrentPublished 是该 name 当前发布版；null = 首次定义。
	CurrentPublished *PolicyVersionView `json:"current_published"`
	Proposed         PolicyVersionView  `json:"proposed"`
	// 模型集合差分（新增/移除）。
	AddedModels   []string `json:"added_models"`
	RemovedModels []string `json:"removed_models"`
	// 引用当前发布版的活跃权益数——它们**保留旧版本**（权益钉住
	// policy_version_id，新政策只影响新发权益）。
	AffectedAccountsKeptOnOld int64 `json:"affected_accounts_kept_on_old"`
	// 结构性事实：旧订阅/既有权益保留其钉住的策略版本。
	ExistingSubscriptionsKeepVersion bool `json:"existing_subscriptions_keep_version"`
}

// PolicyVersionView renders one policy revision for comparison.
type PolicyVersionView struct {
	Revision         int      `json:"revision"`
	PolicyVersionID  string   `json:"policy_version_id,omitempty"`
	ModelIDs         []string `json:"model_ids"`
	FiveHourLimit    *string  `json:"five_hour_limit_micros"`
	WeeklyLimit      *string  `json:"weekly_limit_micros"`
	MonthlyLimit     *string  `json:"monthly_limit_micros"`
	RPMLimit         *int     `json:"rpm_limit"`
	TPMLimit         *int64   `json:"tpm_limit"`
	ConcurrencyLimit *int     `json:"concurrency_limit"`
	OveragePolicy    string   `json:"overage_policy"`
	Status           string   `json:"status,omitempty"`
}

// PreviewPolicyChange diffs the proposed policy against the current
// published revision of the same name. 生效时间语义：策略版本按权益发放时
// 钉住——不存在全局生效时刻；预览显式说明既有权益保留旧版。
func (s *PricingPreviewService) PreviewPolicyChange(ctx context.Context, cmd PolicyChangePreview) (*PolicyChangePreviewResult, error) {
	if cmd.Name == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "policy preview: name required")
	}
	switch cmd.OveragePolicy {
	case "", "reject", "clamp_if_declared", "allow_overage":
	default:
		return nil, domain.NewError(domain.CodeInvalidInput, "policy preview: unknown overage_policy")
	}
	// 模型集合必须真实存在（授权不凭空指向模型）。
	for _, id := range cmd.ModelIDs {
		if _, err := s.store.GetModel(ctx, id); err != nil {
			return nil, domain.WrapError(domain.CodeInvalidInput, "policy preview: model "+id, err)
		}
	}
	res := &PolicyChangePreviewResult{
		ServerTime: s.clock.Now().UTC(), Name: cmd.Name,
		Proposed: PolicyVersionView{
			ModelIDs: cmd.ModelIDs,
			FiveHourLimit: microStrPtr(cmd.FiveHourLimit), WeeklyLimit: microStrPtr(cmd.WeeklyLimit),
			MonthlyLimit: microStrPtr(cmd.MonthlyLimit),
			RPMLimit: cmd.RPMLimit, TPMLimit: cmd.TPMLimit, ConcurrencyLimit: cmd.ConcurrencyLimit,
			OveragePolicy: cmd.OveragePolicy,
		},
		AddedModels:  []string{},
		RemovedModels: []string{},
		ExistingSubscriptionsKeepVersion: true,
	}
	current, err := s.store.LatestPolicyVersionByName(ctx, cmd.Name)
	if err == nil {
		res.CurrentPublished = &PolicyVersionView{
			Revision: current.Revision, PolicyVersionID: current.ID, ModelIDs: current.ModelIDs,
			FiveHourLimit: microStrPtr(current.FiveHourLimit),
			WeeklyLimit:   microStrPtr(current.WeeklyLimit),
			MonthlyLimit:  microStrPtr(current.MonthlyLimit),
			RPMLimit: current.RPMLimit, TPMLimit: current.TPMLimit, ConcurrencyLimit: current.ConcurrencyLimit,
			OveragePolicy: current.OveragePolicy, Status: current.Status,
		}
		res.AddedModels, res.RemovedModels = diffStringSets(cmd.ModelIDs, current.ModelIDs)
		if res.AffectedAccountsKeptOnOld, err = s.store.CountActiveEntitlementsUsingPolicy(ctx, current.ID); err != nil {
			return nil, err
		}
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return nil, err
	} else {
		res.AddedModels = append([]string{}, cmd.ModelIDs...)
	}
	return res, nil
}

func diffStringSets(newSet, oldSet []string) (added, removed []string) {
	old := map[string]bool{}
	for _, v := range oldSet {
		old[v] = true
	}
	neu := map[string]bool{}
	for _, v := range newSet {
		neu[v] = true
		if !old[v] {
			added = append(added, v)
		}
	}
	for _, v := range oldSet {
		if !neu[v] {
			removed = append(removed, v)
		}
	}
	return added, removed
}
