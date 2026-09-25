// usage_view.go — 客户用量读模型（Task 11，设计 §9.2 /user/model-usage）。
//
// 口径（控制者决定，逐字执行）：
//   - 用量只来自 inference_requests / inference_usage_records /
//     inference_ledger_entries —— 绝不使用 usage_events 心跳表（心跳是
//     客户端活跃信号，不是计费事实，设计 §2）。
//   - 这是历史统计读：响应带 as_of / complete_through（本实现直接读权威
//     表，complete_through = as_of，无延迟）。延迟统计不得用作实时放行
//     依据 —— 实时放行只看 /user/model-quotas 背后的权威窗口状态（本任务
//     只做读，此处仅注明口径）。
//   - 计量完整性：每请求 usage_status ∈ reported/estimated/unknown/pending
//     如实计数；token 桶 NULL ≠ 0（未知保持未知，设计 §7.1）。
//   - 时间范围受限（默认最近 30 天，最大 92 天）；明细分页用 keyset 游标
//     （created_at DESC, id DESC）， offset 翻页在持续写入下会跳行/重复。

package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// UsageGroupBy enumerates the summary grouping dimensions (设计 §9.2: 按模
// 型/Key 分组).
type UsageGroupBy string

const (
	GroupByModel UsageGroupBy = "model"
	GroupByKey   UsageGroupBy = "key"
)

// Valid reports whether g is a known grouping.
func (g UsageGroupBy) Valid() bool { return g == GroupByModel || g == GroupByKey }

// Usage range limits (限制时间范围). The default window is the last 30
// days; the maximum span is 92 days (约一个自然季度) per query.
const (
	DefaultUsageRange = 30 * 24 * time.Hour
	MaxUsageRange     = 92 * 24 * time.Hour
)

// UsageSummaryFilter is the validated input of the grouped summary.
// [From, To) is a half-open UTC interval (窗口边界语义与 §6 一致).
type UsageSummaryFilter struct {
	From time.Time
	To   time.Time
	// GroupBy: GroupByModel (default) or GroupByKey.
	GroupBy UsageGroupBy
	// ModelID / APIKeyID optionally narrow the scope (also applied to the
	// daily series).
	ModelID  string
	APIKeyID string
}

// UsageGroup is one aggregate row of the summary. Token buckets sum the
// LATEST usage revision of every attempt of the in-range requests; a nil
// bucket means "no reported value" (unknown), never zero.
type UsageGroup struct {
	// ModelID is the group key under group_by=model; APIKeyID under
	// group_by=key (nil = 无 Key 的 JWT/facade 调用组).
	ModelID  *string
	APIKeyID *string
	// KeyName/KeyPrefix are display correlation for the key group (never
	// key material).
	KeyName   string
	KeyPrefix string

	RequestsTotal         int64
	SettledRequests       int64
	ReleasedRequests      int64
	InFlightRequests      int64 // reserved/dispatching/streaming/settling/authenticated
	FailedRequests        int64
	ReconciliationPending int64 // status=reconciliation_required（待核对）

	// 计量完整性（usage_status 计数）。
	Reported  int64
	Estimated int64
	Unknown   int64
	Pending   int64

	// 金额口径派生自不可变账本（与 reconciliation 重建不变量逐字一致：
	// 窗口 used ≡ Σ charge − Σ reversal ± Σ 请求级 adjustment（debit + /
	// credit −），Task 9 accounting/reconciliation.go）：
	//   - ChargeMicros  = Σ entry_type='charge'（原始 charge 分录，修正不
	//     重写它 —— CorrectSettlement 作废时 settled_micros 归零但 charge
	//     分录保留，因此 charge 绝不能读 settled_micros，否则 void 后
	//     net = 0 − reversal 出现负净额）；
	//   - ReversedMicros = Σ entry_type='reversal'；
	//   - AdjustedMicros = 请求级 adjustment 签名合计（debit + / credit −，
	//     含 CorrectSettlement 补差与运营补偿 —— 后者只写账本不动
	//     settled_micros，账本派生口径下对客户自然可见）。
	ChargeMicros   int64
	ReversedMicros int64
	AdjustedMicros int64

	Tokens domain.UsageBuckets
}

// NetMicros is charge − reversed + adjusted (debit + / credit −).
func (g UsageGroup) NetMicros() int64 { return g.ChargeMicros - g.ReversedMicros + g.AdjustedMicros }

// UsageSeriesBucket is one UTC calendar day of the time series
// (设计 §9.2: 时间序列). Bucket boundaries are UTC regardless of the DB
// session timezone. Amount columns share the ledger-derived semantics of
// UsageGroup (charge 毛额 + reversal + adjustment；净额 = NetMicros).
type UsageSeriesBucket struct {
	BucketStart    time.Time
	RequestsTotal  int64
	ChargeMicros   int64
	ReversedMicros int64
	AdjustedMicros int64
	Reported       int64
	Estimated      int64
	Unknown        int64
}

// NetMicros is charge − reversed + adjusted.
func (b UsageSeriesBucket) NetMicros() int64 {
	return b.ChargeMicros - b.ReversedMicros + b.AdjustedMicros
}

// UsageSummary is the assembled summary read model.
type UsageSummary struct {
	ServerTime time.Time
	AsOf       time.Time
	// CompleteThrough is the instant the statistics cover. This
	// implementation reads the authoritative tables directly, so it equals
	// AsOf; the field exists so any future延迟聚合 implementation must
	// state its cutoff explicitly (控制者决定: 历史统计可延迟但给出截止
	// 时间).
	CompleteThrough time.Time
	Unit            string
	From, To        time.Time
	GroupBy         UsageGroupBy
	Groups          []UsageGroup
	Series          []UsageSeriesBucket
}

// RequestCursor is the keyset position of the detail pagination
// (created_at DESC, id DESC). Serialized as base64url JSON — opaque to
// clients (no offset math, stable under concurrent inserts).
type RequestCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

// EncodeRequestCursor renders the cursor as an opaque URL-safe token.
func EncodeRequestCursor(c RequestCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeRequestCursor parses an opaque token; malformed input is a client
// error (invalid_input), never silently ignored. The id must be a UUID —
// a well-formed envelope carrying a non-UUID id would otherwise fail the
// `$n::uuid` cast deep in SQL (500); reject it at the boundary (400).
func DecodeRequestCursor(s string) (RequestCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return RequestCursor{}, domain.NewError(domain.CodeInvalidInput, "usage: malformed cursor")
	}
	var c RequestCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" || c.CreatedAt.IsZero() {
		return RequestCursor{}, domain.NewError(domain.CodeInvalidInput, "usage: malformed cursor")
	}
	if _, err := uuid.Parse(c.ID); err != nil {
		return RequestCursor{}, domain.NewError(domain.CodeInvalidInput, "usage: malformed cursor (id must be a UUID)")
	}
	return c, nil
}

// RequestListFilter is the validated input of the detail pagination.
type RequestListFilter struct {
	From, To time.Time
	ModelID  string
	APIKeyID string
	Cursor   *RequestCursor
	Limit    int // 1..100, default 50
}

// RequestRow is one logical request in the customer detail list.
// ChargeMicros derives from the IMMUTABLE ledger charge entry (nil while
// no charge exists — 预占中/已释放/待核对）; ReversedMicros and the signed
// AdjustedMicros carry post-settlement corrections (void 冲正 /
// CorrectSettlement 补差 / 运营补偿). Tokens sums the latest usage revision
// per attempt; a nil bucket means not reported.
type RequestRow struct {
	ID             string
	ModelID        string
	APIKeyID       *string
	KeyName        string
	KeyPrefix      string
	Protocol       string
	Stream         bool
	Status         domain.RequestStatus
	UsageStatus    domain.UsageSource
	ReservedMicros *domain.Microcredit
	ChargeMicros   *int64 // 账本 charge 分录；nil = 尚无 charge 行
	ReversedMicros int64
	AdjustedMicros int64 // 签名合计：debit + / credit −
	// HasUsage distinguishes "no usage record yet" from "record exists but
	// unknown" (both render null buckets; usage_status carries the state).
	HasUsage bool
	Tokens   domain.UsageBuckets

	CreatedAt   time.Time
	AdmittedAt  *time.Time
	CompletedAt *time.Time
}

// NetMicrosPtr is charge − reversed + adjusted; nil while the request has
// no charge entry (未结算).
func (r RequestRow) NetMicrosPtr() *int64 {
	if r.ChargeMicros == nil {
		return nil
	}
	n := *r.ChargeMicros - r.ReversedMicros + r.AdjustedMicros
	return &n
}

// RequestPage is one keyset page. NextCursor is empty when the page is
// the last one. ServerTime/AsOf are the service clock at read time; the
// effective [From, To) echoes the resolved range.
type RequestPage struct {
	ServerTime time.Time
	AsOf       time.Time
	Unit       string
	From, To   time.Time
	// Limit is the effective page size (defaults applied).
	Limit      int
	Items      []RequestRow
	NextCursor string
}

// UsageViewStore is the read surface the usage view needs; satisfied by
// inference/postgres.Store.
type UsageViewStore interface {
	GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error)
	// SummarizeUsageGroups aggregates in-range requests by the filter's
	// grouping dimension (model or key).
	SummarizeUsageGroups(ctx context.Context, accountID string, f UsageSummaryFilter) ([]UsageGroup, error)
	// SummarizeUsageSeries buckets in-range requests into UTC calendar days.
	SummarizeUsageSeries(ctx context.Context, accountID string, f UsageSummaryFilter) ([]UsageSeriesBucket, error)
	// ListRequestRows returns at most f.Limit+1 rows after the cursor in
	// (created_at DESC, id DESC) order — the extra row is the hasMore
	// signal the service converts into NextCursor.
	ListRequestRows(ctx context.Context, accountID string, f RequestListFilter) ([]RequestRow, error)
}

// UsageViewService assembles the customer usage views.
type UsageViewService struct {
	store UsageViewStore
	clock domain.Clock
}

// NewUsageViewService builds the service; a nil clock uses the system
// clock (UTC).
func NewUsageViewService(store UsageViewStore, clock domain.Clock) *UsageViewService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &UsageViewService{store: store, clock: clock}
}

// ResolveUsageRange applies defaults and validates the [from, to) window
// against the range limits (限制时间范围). Exported for handler-side
// parameter parsing.
func ResolveUsageRange(from, to time.Time, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-DefaultUsageRange)
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return time.Time{}, time.Time{}, domain.NewError(domain.CodeInvalidInput, "usage: from must be before to")
	}
	if to.Sub(from) > MaxUsageRange {
		return time.Time{}, time.Time{}, domain.NewError(domain.CodeInvalidInput,
			"usage: time range exceeds 92 days")
	}
	return from, to, nil
}

// Summary builds the grouped aggregate + daily series for the caller's own
// account. A user without a billing account gets an empty summary (reads
// never create rows). Zero From/To take the defaults (最近 30 天）; the
// range is validated here so no caller can widen it （限制时间范围）.
func (s *UsageViewService) Summary(ctx context.Context, userID string, f UsageSummaryFilter) (*UsageSummary, error) {
	now := s.clock.Now().UTC()
	from, to, err := ResolveUsageRange(f.From, f.To, now)
	if err != nil {
		return nil, err
	}
	f.From, f.To = from, to
	if f.GroupBy == "" {
		f.GroupBy = GroupByModel
	}
	if !f.GroupBy.Valid() {
		return nil, domain.NewError(domain.CodeInvalidInput, "usage: group_by must be model|key")
	}
	out := &UsageSummary{
		ServerTime: now, AsOf: now, CompleteThrough: now, Unit: UnitMicrocredit,
		From: f.From, To: f.To, GroupBy: f.GroupBy,
		Groups: []UsageGroup{}, Series: []UsageSeriesBucket{},
	}
	account, err := s.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return out, nil
		}
		return nil, err
	}
	groups, err := s.store.SummarizeUsageGroups(ctx, account.ID, f)
	if err != nil {
		return nil, err
	}
	out.Groups = groups
	series, err := s.store.SummarizeUsageSeries(ctx, account.ID, f)
	if err != nil {
		return nil, err
	}
	out.Series = series
	return out, nil
}

// ListRequests returns one keyset page of the caller's own request detail.
// Zero From/To take the defaults; the range limit applies here too.
func (s *UsageViewService) ListRequests(ctx context.Context, userID string, f RequestListFilter) (*RequestPage, error) {
	now := s.clock.Now().UTC()
	from, to, err := ResolveUsageRange(f.From, f.To, now)
	if err != nil {
		return nil, err
	}
	f.From, f.To = from, to
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 100 {
		return nil, domain.NewError(domain.CodeInvalidInput, "usage: limit must be 1..100")
	}
	out := &RequestPage{
		ServerTime: now, AsOf: now, Unit: UnitMicrocredit,
		From: f.From, To: f.To, Limit: f.Limit, Items: []RequestRow{},
	}
	account, err := s.store.GetBillingAccountByUser(ctx, userID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return out, nil
		}
		return nil, err
	}
	rows, err := s.store.ListRequestRows(ctx, account.ID, f)
	if err != nil {
		return nil, err
	}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		out.NextCursor = EncodeRequestCursor(RequestCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	out.Items = rows
	return out, nil
}
