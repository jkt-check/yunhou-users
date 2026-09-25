// quota_view.go — 客户配额读模型（Task 11，设计 §6/§9.2 /user/model-quotas）。
//
// quota 读是"当前权威状态"：窗口 used/reserved 直接读 inference_quota_windows
// 的活行（配额闸门写入的同一事务状态），不做任何延迟聚合；as_of 即读取时刻。
// 读取绝不激活五小时窗口（quota.ReadWindow，设计 §6：读取额度接口不激活窗口）。
//
// 视图选择的权益与调用路径同一优先序（access.SelectEntitlement：显式套餐优先、
// 赠送仅在没有显式套餐时参与）；没有可用权益时如实展示最近停用的权益 +
// blocked_by 原因（已过期/已吊销），而不是 404 —— 控制台需要区分"从没买过"
// 与"买过期了"。

package management

import (
	"context"
	"sort"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/quota"
)

// Block reasons of the quota view (设计 §9.2 blocked_by; OpenAPI 枚举同源).
const (
	// BlockQuotaExhausted — 某窗口 remaining=0（enabled 且 limit-used-reserved
	// 归 0）。零限额策略天然落此（limit=0）。
	BlockQuotaExhausted = "quota_exhausted"
	// BlockEntitlementExpired — 权益已过 effective_to（不论存储 status 是否
	// 已被卫生任务翻转；授权判定本就只看 effective_to）。
	BlockEntitlementExpired = "entitlement_expired"
	// BlockEntitlementRevoked / BlockEntitlementSuperseded — 退款/取消吊销，
	// 或被升级取代后无其他可用权益。
	BlockEntitlementRevoked    = "entitlement_revoked"
	BlockEntitlementSuperseded = "entitlement_superseded"
	// BlockEntitlementNotYetEffective — effective_from 在未来（当前无发放路
	// 径会产生，防御性如实展示）。
	BlockEntitlementNotYetEffective = "entitlement_not_yet_effective"
	// BlockNoActiveEntitlement — 账户从未有过任何权益。
	BlockNoActiveEntitlement = "no_active_entitlement"
	// BlockAccountNotActive — 计费账户被暂停/关闭（运营动作）。
	BlockAccountNotActive = "account_not_active"
)

// BlockKindEntitlement / BlockKindAccount distinguish non-window blocks in
// BlockedBy; window blocks carry the domain.WindowKind verbatim.
const (
	BlockKindEntitlement = "entitlement"
	BlockKindAccount     = "account"
)

// WindowBlockView is the customer display of one exhausted window. The
// amounts mirror the window JSON of the same response (decimal-integer
// microcredits); ResetsAt is nil when the recovery instant is unknown
// (e.g. an unactivated zero-limit five-hour window) — never invented
// (设计 §9.1: 未知恢复不能编造倒计时).
type WindowBlockView struct {
	Kind     domain.WindowKind
	Limit    domain.Microcredit
	Used     domain.Microcredit
	Reserved domain.Microcredit
	// Remaining is Remaining(window) at block time (max(0, limit-used-
	// reserved) — 与 ExhaustedWindowBlock 谓词同一计算，不重复实现).
	Remaining domain.Microcredit
	ResetsAt  *time.Time
}

// Block is one current blocking constraint: either an exhausted window or
// an entitlement/account state. Window blocks carry the counter snapshot;
// entitlement/account blocks carry Reason and (for expiry) ExpiredAt.
type Block struct {
	Kind   string // "five_hour" | "weekly" | "monthly" | BlockKindEntitlement | BlockKindAccount
	Reason string // one of the Block* constants
	// Window is set for quota_exhausted blocks.
	Window *WindowBlockView
	// ExpiredAt is set for BlockEntitlementExpired (the effective_to that
	// cut authorization off).
	ExpiredAt *time.Time
}

// EntitlementView is the entitlement shown in the quota response: the one
// the call path would select, or — when none is usable — the most recent
// retired one (如实展示过期/吊销状态).
type EntitlementView struct {
	ID              string
	Status          domain.EntitlementStatus
	SourceType      domain.EntitlementSource
	ModelIDs        []string
	PolicyVersionID string
	AnchorAt        time.Time
	EffectiveFrom   time.Time
	EffectiveTo     *time.Time
	Revision        int
}

// QuotaView is the assembled /user/model-quotas read model. Unit is always
// "microcredit"; every amount renders as a decimal integer string at the
// HTTP layer (设计 §9.2).
type QuotaView struct {
	ServerTime time.Time
	AsOf       time.Time
	Unit       string
	// Entitlement is nil only when the account has NEVER had any
	// entitlement (BlockedBy then carries no_active_entitlement).
	Entitlement *EntitlementView
	BlockedBy   []Block
	// Windows holds one view per window kind in the fixed order
	// (five_hour, weekly, monthly), including explicitly DISABLED ones
	// (Disabled=true, limit null — never read as unlimited, 设计 §9.2).
	// Empty only when there is no entitlement to key windows on.
	Windows []quota.WindowView
}

// QuotaViewStore is the read surface the quota view needs; satisfied by
// inference/postgres.Store. All reads are the authoritative current state.
type QuotaViewStore interface {
	GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error)
	// ListEntitlements returns the account's entitlements in ANY status
	// (retired rows included — the view renders expired/revoked honestly).
	ListEntitlements(ctx context.Context, billingAccountID string) ([]domain.Entitlement, error)
	// LoadWindows reads active window rows without activating anything.
	LoadWindows(ctx context.Context, entitlementID string, kinds []domain.WindowKind) ([]domain.QuotaWindow, error)
	// GetQuotaPolicy returns the pure policy shape of one immutable version.
	GetQuotaPolicy(ctx context.Context, policyVersionID string) (quota.Policy, error)
}

// QuotaViewService assembles the customer quota view.
type QuotaViewService struct {
	store QuotaViewStore
	clock domain.Clock
}

// NewQuotaViewService builds the service; a nil clock uses the system
// clock (UTC).
func NewQuotaViewService(store QuotaViewStore, clock domain.Clock) *QuotaViewService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &QuotaViewService{store: store, clock: clock}
}

// UnitMicrocredit is the unit token of every amount in the view (设计 §9.2).
const UnitMicrocredit = "microcredit"

// Get builds the quota view for the caller's own billing account. A user
// without a billing account (never touched the product) gets the empty
// view with a no_active_entitlement block — reads never create rows.
func (s *QuotaViewService) Get(ctx context.Context, userID string) (*QuotaView, error) {
	now := s.clock.Now().UTC()
	view := &QuotaView{
		ServerTime: now, AsOf: now, Unit: UnitMicrocredit,
		BlockedBy: []Block{}, Windows: []quota.WindowView{},
	}
	account, err := s.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			view.BlockedBy = append(view.BlockedBy, Block{Kind: BlockKindEntitlement, Reason: BlockNoActiveEntitlement})
			return view, nil
		}
		return nil, err
	}
	if account.Status != string(domain.BillingAccountActive) {
		view.BlockedBy = append(view.BlockedBy, Block{Kind: BlockKindAccount, Reason: BlockAccountNotActive})
	}
	ents, err := s.store.ListEntitlements(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	ent := SelectViewEntitlement(ents, now)
	if ent == nil {
		if len(view.BlockedBy) == 0 {
			view.BlockedBy = append(view.BlockedBy, Block{Kind: BlockKindEntitlement, Reason: BlockNoActiveEntitlement})
		}
		return view, nil
	}
	view.Entitlement = &EntitlementView{
		ID: ent.ID, Status: ent.Status, SourceType: ent.SourceType,
		ModelIDs: ent.ModelIDs, PolicyVersionID: ent.PolicyVersionID,
		AnchorAt: ent.AnchorAt.UTC(), EffectiveFrom: ent.EffectiveFrom.UTC(),
		EffectiveTo: ent.EffectiveTo, Revision: ent.Revision,
	}

	if block, ok := EntitlementBlock(ent, now); ok {
		view.BlockedBy = append(view.BlockedBy, block)
	}

	policy, err := s.store.GetQuotaPolicy(ctx, ent.PolicyVersionID)
	if err != nil {
		return nil, err
	}
	windows, err := s.store.LoadWindows(ctx, ent.ID, nil)
	if err != nil {
		return nil, err
	}
	views, err := quota.Views(policy, ent.AnchorAt, windows, now)
	if err != nil {
		// now 早于锚点（未来生效的权益）：窗口周期尚不存在，如实省略
		// （blocked_by 已带 not_yet_effective），不编造窗口边界。
		if domain.CodeOf(err) == domain.CodeInvalidInput {
			return view, nil
		}
		return nil, err
	}
	view.Windows = views
	for _, w := range views {
		if b, ok := ExhaustedWindowBlock(w); ok {
			view.BlockedBy = append(view.BlockedBy, b)
		}
	}
	return view, nil
}

// Remaining computes max(0, limit-used-reserved) for an enabled window view
// (设计 §6). A disabled window has no remaining (nil).
func Remaining(w quota.WindowView) *domain.Microcredit {
	if w.Disabled || w.Limit == nil {
		return nil
	}
	r := *w.Limit - w.Used - w.Reserved
	if r < 0 {
		r = 0
	}
	return &r
}

// ExhaustedWindowBlock reports a window whose remaining is exactly zero:
// any admission (every hold is > 0) fails on it right now. A disabled or
// unactivated window with a positive limit never blocks. ResetsAt comes
// from the window view; an unactivated five-hour window has none (nil —
// the recovery instant is unknown until a consumption activates it).
func ExhaustedWindowBlock(w quota.WindowView) (Block, bool) {
	r := Remaining(w)
	if r == nil || *r > 0 {
		return Block{}, false
	}
	return Block{
		Kind:   string(w.Kind),
		Reason: BlockQuotaExhausted,
		Window: &WindowBlockView{
			Kind: w.Kind, Limit: *w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: *r, ResetsAt: w.ResetsAt,
		},
	}, true
}

// EntitlementBlock maps the entitlement's effective state at `now` to a
// block reason. Authorization is a pure function of effective_from/to —
// the stored status flip (sweeper hygiene) is not awaited (设计 §6: 已过
// 期权益不能继续调用，即使某个窗口刚刷新).
func EntitlementBlock(ent *domain.Entitlement, now time.Time) (Block, bool) {
	t := now.UTC()
	switch ent.Status {
	case domain.EntitlementRevoked:
		return Block{Kind: BlockKindEntitlement, Reason: BlockEntitlementRevoked}, true
	case domain.EntitlementSuperseded:
		return Block{Kind: BlockKindEntitlement, Reason: BlockEntitlementSuperseded}, true
	case domain.EntitlementExpired:
		// 卫生翻转（sweeper/worker 已把行置为 expired）：如实展示。
		return Block{Kind: BlockKindEntitlement, Reason: BlockEntitlementExpired, ExpiredAt: ent.EffectiveTo}, true
	}
	// status=active：授权判定只看 effective_from/to，不等卫生翻转
	// （设计 §6: 已过期权益不能继续调用，即使某个窗口刚刷新）。
	if ent.EffectiveTo != nil && !t.Before(ent.EffectiveTo.UTC()) {
		to := ent.EffectiveTo.UTC()
		return Block{Kind: BlockKindEntitlement, Reason: BlockEntitlementExpired, ExpiredAt: &to}, true
	}
	if t.Before(ent.EffectiveFrom.UTC()) {
		return Block{Kind: BlockKindEntitlement, Reason: BlockEntitlementNotYetEffective}, true
	}
	return Block{}, false
}

// SelectViewEntitlement picks the ONE entitlement the customer quota view
// displays, mirroring the call-path precedence (access.SelectEntitlement,
// 设计 §4.2) without the per-model dimension:
//
//  1. usable-now explicit purchases (subscription/order), earliest created;
//  2. usable-now gifts, earliest created;
//  3. retired/pending rows (expired/revoked/superseded/not-yet-effective),
//     explicit before gifts, NEWEST created first — the customer sees the
//     most recent plan state, not a stale historical row.
//
// Returns nil when the account has no entitlement rows at all.
func SelectViewEntitlement(ents []domain.Entitlement, now time.Time) *domain.Entitlement {
	t := now.UTC()
	usable := func(e *domain.Entitlement) bool {
		if e.Status != domain.EntitlementActive {
			return false
		}
		if t.Before(e.EffectiveFrom.UTC()) {
			return false
		}
		return e.EffectiveTo == nil || t.Before(e.EffectiveTo.UTC())
	}
	explicit := func(e *domain.Entitlement) bool { return e.SourceType != domain.SourceGrant }

	pool := make([]domain.Entitlement, 0, len(ents))
	for _, e := range ents {
		if usable(&e) {
			pool = append(pool, e)
		}
	}
	sortActive := func(list []domain.Entitlement) {
		sort.SliceStable(list, func(i, j int) bool {
			if explicit(&list[i]) != explicit(&list[j]) {
				return explicit(&list[i])
			}
			if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
				return list[i].CreatedAt.Before(list[j].CreatedAt)
			}
			return list[i].ID < list[j].ID
		})
	}
	if len(pool) > 0 {
		sortActive(pool)
		return &pool[0]
	}
	// Nothing usable: show the most recent retired/pending row.
	retired := make([]domain.Entitlement, 0, len(ents))
	for _, e := range ents {
		retired = append(retired, e)
	}
	if len(retired) == 0 {
		return nil
	}
	sort.SliceStable(retired, func(i, j int) bool {
		if explicit(&retired[i]) != explicit(&retired[j]) {
			return explicit(&retired[i])
		}
		if !retired[i].CreatedAt.Equal(retired[j].CreatedAt) {
			return retired[i].CreatedAt.After(retired[j].CreatedAt)
		}
		return retired[i].ID > retired[j].ID
	})
	return &retired[0]
}
