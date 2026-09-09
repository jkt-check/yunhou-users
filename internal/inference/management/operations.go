// operations.go — 运营读模型（Task 15，设计 §9.2 /admin/model-usage、
// /admin/model-customers、/admin/upstreams 的统计与异常筛选能力）。
//
// 口径（控制者决定 2/5/6，逐字执行）：
//   - 用量/成本只来自 inference_requests / inference_attempts /
//     inference_usage_records / inference_ledger_entries —— 绝不使用
//     usage_events 心跳表。
//   - 金额与 Task 11 客户面逐字同一账本派生口径（charge=Σ原始分录，
//     reversed=Σ冲正，adjusted=Σ请求级调整 debit+/credit−，net=派生）；
//     聚合必须能与账本抽样核对（测试里做抽样对账断言）。
//   - 采购成本来自 inference_attempts.cost_micros/cost_currency/cost_basis
//     （上游凭据/账号池观测与价格配置的落库事实），按 币种 × cost_basis
//     （reported/estimated/allocated）分片呈现，绝不跨币种合并、绝不编造；
//     cost_micros IS NULL 的尝试计入 cost_unknown_attempts（未知保持未知）。
//   - 只读派生路径：统计/预览/异常筛选全部直读，不触碰预占/结算的权威
//     事务状态，不为统计引入对写路径的共享锁。
//   - 历史统计读带 as_of/complete_through（直读权威表，二者相等）。

package management

import (
	"context"
	"strconv"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// OpsGroupBy enumerates the operator aggregation dimensions.
type OpsGroupBy string

const (
	OpsGroupModel    OpsGroupBy = "model"
	OpsGroupProvider OpsGroupBy = "provider"
	OpsGroupCustomer OpsGroupBy = "customer"
)

// Valid reports whether g is a known operator grouping.
func (g OpsGroupBy) Valid() bool {
	return g == OpsGroupModel || g == OpsGroupProvider || g == OpsGroupCustomer
}

// OpsUsageFilter is the validated input of the operator summary. The
// [From, To) window reuses ResolveUsageRange (默认最近 30 天，最大 92 天).
type OpsUsageFilter struct {
	From, To  time.Time
	GroupBy   OpsGroupBy
	ModelID   string
	ProviderID string
	AccountID string // billing_account_id
}

// CostSlice is the procurement cost of one (currency, cost_basis) shard.
// Micros renders as a decimal string on the wire (DecimalInt64); 不同币种/
// 口径永不合并。
type CostSlice struct {
	Currency string `json:"currency"`
	Basis    string `json:"basis"` // reported | estimated | allocated
	Micros   int64  `json:"-"`
}

// OpsGroup is one aggregate row of the operator summary. 请求计数/计量完整
// 性与客户面同一语义；延迟来自 attempts（finished_at − started_at，毫秒，
// 仅已完成的尝试参与）；客户消费为账本派生 microcredit；采购成本为
// attempts 落库事实的分币种/口径分片。
type OpsGroup struct {
	// 分组键（按 GroupBy 恰有一个非空）。
	ModelID    string `json:"model_id,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`
	AccountID  string `json:"billing_account_id,omitempty"`
	UserID     string `json:"user_id,omitempty"`

	RequestsTotal         int64 `json:"requests"`
	SettledRequests       int64 `json:"settled"`
	ReleasedRequests      int64 `json:"released"`
	InFlightRequests      int64 `json:"in_flight"`
	FailedRequests        int64 `json:"failed"`
	ReconciliationPending int64 `json:"reconciliation_pending"`

	Reported  int64 `json:"reported"`
	Estimated int64 `json:"estimated"`
	Unknown   int64 `json:"unknown"`
	Pending   int64 `json:"pending"`

	// SuccessRate = (settled+released)/terminal（terminal = settled+
	// released+failed）；无终态请求时为 null。统计比率非金额，浮点四位
	// 小数；权威数字永远是上面的计数。
	SuccessRate *float64 `json:"success_rate"`

	AvgLatencyMs *int64 `json:"avg_latency_ms"`
	P95LatencyMs *int64 `json:"p95_latency_ms"`

	// 客户消费（microcredit 账本派生）。
	ChargeMicros   int64 `json:"-"`
	ReversedMicros int64 `json:"-"`
	AdjustedMicros int64 `json:"-"`

	// 采购成本分片与未知计数。
	CostSlices          []CostSlice `json:"-"`
	CostUnknownAttempts int64       `json:"cost_unknown_attempts"`

	Tokens domain.UsageBuckets `json:"-"`
}

// NetMicros is charge − reversed + adjusted.
func (g OpsGroup) NetMicros() int64 { return g.ChargeMicros - g.ReversedMicros + g.AdjustedMicros }

// OpsSummary is the assembled operator summary read model.
type OpsSummary struct {
	ServerTime      time.Time
	AsOf            time.Time
	CompleteThrough time.Time
	From, To        time.Time
	GroupBy         OpsGroupBy
	Groups          []OpsGroup
}

// Exception kinds of the operator exception filter.
const (
	// ExceptionStuckReservations: held 预占超过阈值（默认 1 小时）仍未结算/
	// 释放——恢复 worker 正在或应当处理的异常预占。
	ExceptionStuckReservations = "stuck_reservations"
	// ExceptionReauthAccounts: 上游账号授权失效（reauth_required）——
	// invalid_grant 传播后停止调度的账号（Task 12 语义）。
	ExceptionReauthAccounts = "reauth_required_accounts"
	// ExceptionSettlementBacklog: 结算积压——pending/running/escalated 的
	// 核对任务（含超期标记）。
	ExceptionSettlementBacklog = "settlement_backlog"
)

// ValidExceptionKind reports whether k is a known exception filter.
func ValidExceptionKind(k string) bool {
	switch k {
	case ExceptionStuckReservations, ExceptionReauthAccounts, ExceptionSettlementBacklog:
		return true
	}
	return false
}

// StuckReservation is one anomalous held reservation.
type StuckReservation struct {
	ReservationID string    `json:"reservation_id"`
	RequestID     string    `json:"request_id"`
	RequestStatus string    `json:"request_status"`
	AccountID     string    `json:"billing_account_id"`
	ModelID       string    `json:"model_id"`
	TargetKind    string    `json:"target_kind"`
	AmountMicros  int64     `json:"-"`
	AgeSeconds    int64     `json:"age_seconds"`
	CreatedAt     time.Time `json:"created_at"`
}

// ReauthAccount is one upstream account whose authorization lapsed.
type ReauthAccount struct {
	AccountID    string     `json:"account_id"`
	ProviderID   string     `json:"provider_id"`
	ProviderCode string     `json:"provider_code"`
	DisplayName  string     `json:"display_name"`
	Status       string     `json:"status"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// BacklogJob is one open reconciliation job with an overdue marker.
type BacklogJob struct {
	ID         string     `json:"id"`
	RequestID  *string    `json:"request_id"`
	Reason     string     `json:"reason"`
	Status     string     `json:"status"`
	DeadlineAt time.Time  `json:"deadline_at"`
	Overdue    bool       `json:"overdue"`
	CreatedAt  time.Time  `json:"created_at"`
}

// OpsExceptions is the assembled exception view (kind 决定哪个列表非空).
type OpsExceptions struct {
	ServerTime time.Time
	Kind       string
	StuckReservations []StuckReservation
	ReauthAccounts    []ReauthAccount
	SettlementBacklog []BacklogJob
}

// SharedDeploymentAccount flags one upstream account observed serving MORE
// THAN ONE deployment in the window — the "一个上游账号只挂一个部署"运营约
// 束的检测视图（Task 13 移交项：两部署共享同一上游账号时故障切换会撞租约
// 唯一键 500；调度语义不改，运营面显式检测）。观测来源是 attempts 的落库
// 事实，不是配置推断。
type SharedDeploymentAccount struct {
	AccountID    string    `json:"account_id"`
	ProviderID   string    `json:"provider_id"`
	ProviderCode string    `json:"provider_code"`
	DisplayName  string    `json:"display_name"`
	Deployments  []string  `json:"deployment_ids"`
	Attempts     int64     `json:"attempts"`
	LatestAt     time.Time `json:"latest_attempt_at"`
}

// OperationsStore is the read surface; satisfied by inference/postgres.Store.
type OperationsStore interface {
	SummarizeOpsUsage(ctx context.Context, f OpsUsageFilter) ([]OpsGroup, error)
	ListStuckReservations(ctx context.Context, olderThan time.Time, limit int) ([]StuckReservation, error)
	ListReauthAccounts(ctx context.Context, limit int) ([]ReauthAccount, error)
	ListSettlementBacklog(ctx context.Context, now time.Time, limit int) ([]BacklogJob, error)
	ListSharedDeploymentAccounts(ctx context.Context, from, to time.Time, limit int) ([]SharedDeploymentAccount, error)
	// ListAdjustments 补偿追踪：按账户/全量分页（newest first）。
	ListAdjustments(ctx context.Context, accountID string, limit int) ([]AdjustmentView, error)
}

// AdjustmentView is the operator-facing adjustment row (有原因、对象、金额、
// 操作者与幂等键 — 设计 §9.2 /admin/model-adjustments 的读取面).
type AdjustmentView struct {
	ID              string    `json:"id"`
	BillingAccountID string   `json:"billing_account_id"`
	RequestID       *string   `json:"request_id"`
	Reason          string    `json:"reason"`
	AmountMicros    int64     `json:"-"`
	Direction       string    `json:"direction"`
	Unit            string    `json:"unit"`
	Currency        string    `json:"currency,omitempty"`
	OperatorSubject string    `json:"operator_subject"`
	ServiceSubject  string    `json:"service_subject"`
	IdempotencyKey  string    `json:"idempotency_key"`
	CreatedAt       time.Time `json:"created_at"`
}

// OperationsService assembles the operator read models.
type OperationsService struct {
	store OperationsStore
	clock domain.Clock
}

// NewOperationsService builds the service; nil clock uses UTC.
func NewOperationsService(store OperationsStore, clock domain.Clock) *OperationsService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &OperationsService{store: store, clock: clock}
}

// stuckReservationAge is the default threshold for "异常预占"（held 超过
// 一小时仍未终态 — 恢复 worker 的正常周期远小于此）。
const stuckReservationAge = time.Hour

// Summary builds the operator aggregate. 默认 group_by=model；时间范围沿用
// 客户面的限制（≤92 天）——运营面同样不开放无界扫描。
func (s *OperationsService) Summary(ctx context.Context, f OpsUsageFilter) (*OpsSummary, error) {
	now := s.clock.Now().UTC()
	from, to, err := ResolveUsageRange(f.From, f.To, now)
	if err != nil {
		return nil, err
	}
	f.From, f.To = from, to
	if f.GroupBy == "" {
		f.GroupBy = OpsGroupModel
	}
	if !f.GroupBy.Valid() {
		return nil, domain.NewError(domain.CodeInvalidInput, "ops usage: group_by must be model|provider|customer")
	}
	// provider_id 收敛只在供应商分组下有意义（其余维度静默忽略等于撒谎）。
	if f.ProviderID != "" && f.GroupBy != OpsGroupProvider {
		return nil, domain.NewError(domain.CodeInvalidInput, "ops usage: provider_id requires group_by=provider")
	}
	groups, err := s.store.SummarizeOpsUsage(ctx, f)
	if err != nil {
		return nil, err
	}
	return &OpsSummary{
		ServerTime: now, AsOf: now, CompleteThrough: now,
		From: from, To: to, GroupBy: f.GroupBy,
		Groups: groups,
	}, nil
}

// Exceptions builds one exception view. kind 必选项；limit 上限 500。
func (s *OperationsService) Exceptions(ctx context.Context, kind string, limit int) (*OpsExceptions, error) {
	if !ValidExceptionKind(kind) {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"ops exceptions: kind must be stuck_reservations|reauth_required_accounts|settlement_backlog")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	now := s.clock.Now().UTC()
	out := &OpsExceptions{
		ServerTime: now, Kind: kind,
		StuckReservations: []StuckReservation{},
		ReauthAccounts:    []ReauthAccount{},
		SettlementBacklog: []BacklogJob{},
	}
	var err error
	switch kind {
	case ExceptionStuckReservations:
		out.StuckReservations, err = s.store.ListStuckReservations(ctx, now.Add(-stuckReservationAge), limit)
	case ExceptionReauthAccounts:
		out.ReauthAccounts, err = s.store.ListReauthAccounts(ctx, limit)
	case ExceptionSettlementBacklog:
		out.SettlementBacklog, err = s.store.ListSettlementBacklog(ctx, now, limit)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SharedDeploymentAccounts runs the "一账号一部署"检测视图 over [from, to)
// （默认最近 7 天，最大 92 天）。
func (s *OperationsService) SharedDeploymentAccounts(ctx context.Context, from, to time.Time, limit int) ([]SharedDeploymentAccount, error) {
	now := s.clock.Now().UTC()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-7 * 24 * time.Hour)
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return nil, domain.NewError(domain.CodeInvalidInput, "ops shared-accounts: from must be before to")
	}
	if to.Sub(from) > MaxUsageRange {
		return nil, domain.NewError(domain.CodeInvalidInput, "ops shared-accounts: time range exceeds 92 days")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.store.ListSharedDeploymentAccounts(ctx, from, to, limit)
}

// Adjustments lists compensation records (newest first), optionally scoped
// to one billing account.
func (s *OperationsService) Adjustments(ctx context.Context, accountID string, limit int) ([]AdjustmentView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.store.ListAdjustments(ctx, accountID, limit)
}

// --- wire helpers (十进制整数字符串；JS 大整数精度) ---

func int64Str(v int64) string { return strconv.FormatInt(v, 10) }

func microStrPtr(v *int64) *string {
	if v == nil {
		return nil
	}
	s := strconv.FormatInt(*v, 10)
	return &s
}
