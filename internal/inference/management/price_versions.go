// price_versions.go — 售价版本（inference_price_versions）的运营录入与查询
// 服务（spec: docs/superpowers/specs/2026-09-25-admin-price-versions-design.md）。
//
// 口径：
//   - 价格版本不可变、只追加（设计 §7.1）：本服务只有 create/list，纠正
//     错误的方式是发布新 revision；
//   - unit 由服务端按 kind 派生（sale_credit→microcredit，其余→
//     micromoney），从协议上消除 unit/currency 失配——表 CHECK
//     (unit='microcredit') = (currency IS NULL) 永远成立；
//   - 幂等：自然键 UNIQUE(model_id, kind, revision)。创建前先查，命中即
//     比较内容——同内容重放与同 revision 不同内容都返回 409
//     （PriceVersionExistsError.Identical 区分两者），插入撞唯一键的竞态
//     兜底同样映射 409；
//   - revision 单调追加是强制不变量（M-5）：创建事务内要求
//     revision == 该 (model_id, kind) 当前最大 revision + 1，否则 400；
//     幂等重放（命中既有 revision）先于该检查返回 409；
//   - 创建 = 写 + 审计同事务（accounts 先例：store.Begin +
//     AuditTxRecorder.RecordTx，审计失败即回滚）；列表只读，不落审计。

package management

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"time"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
)

// PriceVersionFilter narrows the admin list query (精确过滤 + 分页窗口).
type PriceVersionFilter struct {
	ModelID string
	Kind    string
	Limit   int
	Offset  int
}

// PriceVersionStore is the persistence surface PriceVersionService needs;
// satisfied by inference/postgres.Store.
type PriceVersionStore interface {
	GetModel(ctx context.Context, id string) (*domain.Model, error)
	// GetPriceVersionByRevision reads the UNIQUE(model_id, kind, revision)
	// row — the idempotent-create pre-check and the race-fallback re-read.
	GetPriceVersionByRevision(ctx context.Context, modelID, kind string, revision int) (*PriceVersionInfo, error)
	ListPriceVersions(ctx context.Context, f PriceVersionFilter) ([]PriceVersionInfo, error)
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	// MaxPriceVersionRevisionTx reads MAX(revision) of one (model_id, kind)
	// inside the caller's UnitOfWork (0 when none) — the max+1 monotonicity
	// gate of Create.
	MaxPriceVersionRevisionTx(ctx context.Context, w domain.UnitOfWork, modelID, kind string) (int, error)
	// InsertPriceVersionTx appends the immutable revision inside the
	// caller's UnitOfWork (写 + 审计同事务) and fills ID/CreatedAt.
	InsertPriceVersionTx(ctx context.Context, w domain.UnitOfWork, p *PriceVersionInfo) error
}

// CreatePriceVersionInput is the payload of POST /admin/price-versions after
// the handler's strict JSON decode; unit is NEVER caller-supplied.
type CreatePriceVersionInput struct {
	ModelID           string
	Kind              string // sale_credit | sale_money | upstream_cost
	Currency          string // sale_credit 必须空；其余 kind 必填 ^[A-Z]{3}$
	InputPerMtok      int64
	CacheReadPerMtok  int64
	CacheWritePerMtok int64
	OutputPerMtok     int64
	ExtraRates        json.RawMessage // 缺席/null → {"schema_version":1}
	Revision          int             // 约定 = 该 (model_id, kind) 当前最大 revision + 1
	EffectiveFrom     time.Time       // zero = 服务时钟 now（与 preview 同语义）
	EffectiveTo       *time.Time
	Reason            string // 审计必填
}

// PriceVersionExistsError reports a UNIQUE(model_id, kind, revision) hit on
// create. Identical=true means the replay carries exactly the stored content
// (safe retry); false means the same revision with conflicting content (操作
// 错误信号，revision 取错). The admin surface maps both to 409 with the
// existing version view in data, the message distinguishing the two.
type PriceVersionExistsError struct {
	Existing  *PriceVersionInfo
	Identical bool
}

func (e *PriceVersionExistsError) Error() string {
	if e.Identical {
		return "price version already exists for (model_id, kind, revision) — duplicate replay of identical content"
	}
	return "price version conflict: (model_id, kind, revision) already exists with different content"
}

// Unwrap exposes the conflict domain error so domain.CodeOf maps the typed
// error to CodeConflict (HTTP 409).
func (e *PriceVersionExistsError) Unwrap() error {
	return domain.NewError(domain.CodeConflict, e.Error())
}

// PriceVersionService is the operator-facing price-version surface.
type PriceVersionService struct {
	store PriceVersionStore
	audit AuditRecorder
	clock domain.Clock
}

// NewPriceVersionService builds the service; nil clock uses UTC, nil audit
// skips the audit trail (unit tests only — production always wires one).
func NewPriceVersionService(store PriceVersionStore, audit AuditRecorder, clock domain.Clock) *PriceVersionService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &PriceVersionService{store: store, audit: audit, clock: clock}
}

var priceCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Create validates and appends one immutable price revision. unit 派生、
// kind/currency/revision/生效区间过 accounting.PriceVersion.Validate（与落库
// CHECK 同一规则）；模型必须存在；（model_id, kind, revision) 幂等；
// revision 强制 max+1（创建事务内检查，幂等重放先于该检查返回 409）。
func (s *PriceVersionService) Create(ctx context.Context, actor string, in CreatePriceVersionInput) (*PriceVersionInfo, error) {
	if in.ModelID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "model_id is required")
	}
	kind := accounting.PriceKind(in.Kind)
	var unit string
	switch kind {
	case accounting.PriceSaleCredit:
		if in.Currency != "" {
			return nil, domain.NewError(domain.CodeInvalidInput, "currency must be empty for sale_credit")
		}
		unit = "microcredit"
	case accounting.PriceSaleMoney, accounting.PriceUpstreamCost:
		if in.Currency == "" {
			return nil, domain.NewError(domain.CodeInvalidInput, "currency is required for "+in.Kind)
		}
		if !priceCurrencyPattern.MatchString(in.Currency) {
			return nil, domain.NewError(domain.CodeInvalidInput, "invalid currency (must match ^[A-Z]{3}$)")
		}
		unit = "micromoney"
	default:
		return nil, domain.NewError(domain.CodeInvalidInput, "unknown kind "+in.Kind)
	}
	for _, rate := range []int64{in.InputPerMtok, in.CacheReadPerMtok, in.CacheWritePerMtok, in.OutputPerMtok} {
		if rate < 0 {
			return nil, domain.NewError(domain.CodeInvalidInput, "rates must be >= 0")
		}
	}
	if in.Revision <= 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "revision must be > 0")
	}
	// 列是 INT（int4）：超出范围会在落库时报 22003 → 500；提前 400。
	if in.Revision > math.MaxInt32 {
		return nil, domain.NewError(domain.CodeInvalidInput, "revision out of range")
	}
	if in.Reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	extra := in.ExtraRates
	if len(extra) == 0 || string(extra) == "null" {
		extra = json.RawMessage(`{"schema_version":1}`)
	} else {
		var obj map[string]any
		if err := json.Unmarshal(extra, &obj); err != nil || obj == nil {
			return nil, domain.NewError(domain.CodeInvalidInput, "invalid extra_rates (must be a JSON object)")
		}
	}
	from := in.EffectiveFrom
	if from.IsZero() {
		from = s.clock.Now().UTC()
	}
	from = from.UTC()
	var to *time.Time
	if in.EffectiveTo != nil {
		t := in.EffectiveTo.UTC()
		to = &t
	}
	// 与表 CHECK（effective_to > effective_from）同精度校验：CHECK 比较的
	// 是 µs 舍入（rint）后的存储值，ns 级比较会放过「舍入后相等」的区间，
	// 留到落库时炸 23514（错误映射会漏约束名）——按存储精度提前 400。
	if to != nil && pgRoundMicros(*to) <= pgRoundMicros(from) {
		return nil, domain.NewError(domain.CodeInvalidInput, "effective_to must be after effective_from")
	}
	// 与落库 CHECK 同一纯校验（kind/currency/revision/生效区间），防御层。
	pure := accounting.PriceVersion{
		ModelID: in.ModelID, Kind: kind, Currency: in.Currency,
		Revision: in.Revision, EffectiveFrom: from, EffectiveTo: to,
	}
	if err := pure.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.store.GetModel(ctx, in.ModelID); err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, domain.NewError(domain.CodeNotFound, "model not found")
		}
		return nil, err
	}

	candidate := &PriceVersionInfo{
		ModelID: in.ModelID, Kind: string(kind), Unit: unit, Currency: in.Currency,
		InputPerMtok: in.InputPerMtok, CacheReadPerMtok: in.CacheReadPerMtok,
		CacheWritePerMtok: in.CacheWritePerMtok, OutputPerMtok: in.OutputPerMtok,
		ExtraRates: extra, Revision: in.Revision,
		EffectiveFrom: from, EffectiveTo: to,
	}

	// 幂等预检：同 (model_id, kind, revision) 已存在 → 比较内容后 409。
	if existing, err := s.store.GetPriceVersionByRevision(ctx, in.ModelID, string(kind), in.Revision); err == nil {
		return nil, &PriceVersionExistsError{Existing: existing, Identical: samePriceVersionContent(existing, candidate)}
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return nil, err
	}

	uow, err := s.store.Begin(ctx)
	if err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "begin transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = uow.Rollback(ctx)
		}
	}()
	// revision 单调追加是强制不变量而非约定（M-5）：必须是该
	// (model_id, kind) 当前最大 revision + 1。检查在创建事务内做；两个
	// 并发 max+1 创建由 UNIQUE 定序，输家走上面的 409 竞态兜底。幂等重放
	// 不受此限——它在 Begin 之前的预检就已按 409 返回。
	maxRev, err := s.store.MaxPriceVersionRevisionTx(ctx, uow, in.ModelID, string(kind))
	if err != nil {
		return nil, err
	}
	if in.Revision != maxRev+1 {
		return nil, domain.NewError(domain.CodeInvalidInput, "revision must be max+1 for (model_id, kind)")
	}
	if err := s.store.InsertPriceVersionTx(ctx, uow, candidate); err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			// 与并发创建撞唯一键（预检竞态兜底）：回滚后重读赢家已提交的
			// 行，按同一规则 409。
			if existing, rerr := s.store.GetPriceVersionByRevision(ctx, in.ModelID, string(kind), in.Revision); rerr == nil {
				return nil, &PriceVersionExistsError{Existing: existing, Identical: samePriceVersionContent(existing, candidate)}
			}
			// 重读也失败时不得把含约束名的原始错误抛给调用方（评审轮1
			// finding：泄露 PG 内部细节）——固定文案的 409。
			return nil, domain.NewError(domain.CodeConflict, "price version conflict for (model_id, kind, revision)")
		}
		return nil, err
	}
	if s.audit != nil {
		// 同事务审计（与 bulk_import 齐平）：审计行与版本行同生共死；
		// 记录器不支持事务写入时整个创建失败（fail-closed，绝不落下
		// 无审计的价格变更）。
		txRec, ok := s.audit.(AuditTxRecorder)
		if !ok {
			return nil, domain.NewError(domain.CodeInternal, "price version: audit recorder lacks transactional support")
		}
		userID, appID := ParseActor(actor)
		if err := txRec.RecordTx(ctx, uow, AuditEvent{
			Action: "price_version.create", ObjectType: "price_version", ObjectID: candidate.ID,
			Reason: in.Reason, ActorUser: userID, ActorApp: appID,
			Detail: SanitizeDetail(priceVersionAuditDetail(candidate)),
		}); err != nil {
			return nil, domain.WrapError(domain.CodeInternal, "price version audit write failed", err)
		}
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return candidate, nil
}

// List returns the filtered page; the handler owns query-parameter
// validation (kind 白名单、offset >= 0、limit 钳制).
func (s *PriceVersionService) List(ctx context.Context, f PriceVersionFilter) ([]PriceVersionInfo, error) {
	return s.store.ListPriceVersions(ctx, f)
}

// priceVersionAuditDetail is the full create payload (model/kind/currency/
// 四档费率/revision/生效区间), sanitized before Record.
func priceVersionAuditDetail(p *PriceVersionInfo) map[string]any {
	// extra_rates 解码成 map 再入 detail：SanitizeDetail 只递归 map/slice，
	// 不透明 json.RawMessage 会让敏感键绕过脱敏（评审轮1 finding）。
	var extra any = json.RawMessage(p.ExtraRates)
	var extraObj map[string]any
	if err := json.Unmarshal(p.ExtraRates, &extraObj); err == nil && extraObj != nil {
		extra = extraObj
	}
	d := map[string]any{
		"model_id": p.ModelID, "kind": p.Kind, "unit": p.Unit,
		"input_micros_per_mtok":       p.InputPerMtok,
		"cache_read_micros_per_mtok":  p.CacheReadPerMtok,
		"cache_write_micros_per_mtok": p.CacheWritePerMtok,
		"output_micros_per_mtok":      p.OutputPerMtok,
		"extra_rates":                 extra,
		"revision":                    p.Revision,
		"effective_from":              p.EffectiveFrom,
	}
	if p.Currency != "" {
		d["currency"] = p.Currency
	}
	if p.EffectiveTo != nil {
		d["effective_to"] = *p.EffectiveTo
	}
	return d
}

// samePriceVersionContent decides duplicate-replay vs conflict: rates、
// 生效区间、币种、extra_rates 任一不一致即冲突（revision 取错），不是重放。
func samePriceVersionContent(existing, candidate *PriceVersionInfo) bool {
	if existing.Kind != candidate.Kind || existing.Unit != candidate.Unit ||
		existing.Currency != candidate.Currency ||
		existing.InputPerMtok != candidate.InputPerMtok ||
		existing.CacheReadPerMtok != candidate.CacheReadPerMtok ||
		existing.CacheWritePerMtok != candidate.CacheWritePerMtok ||
		existing.OutputPerMtok != candidate.OutputPerMtok ||
		existing.Revision != candidate.Revision {
		return false
	}
	if !pgTimeEqual(existing.EffectiveFrom, candidate.EffectiveFrom) {
		return false
	}
	if (existing.EffectiveTo == nil) != (candidate.EffectiveTo == nil) {
		return false
	}
	if existing.EffectiveTo != nil && !pgTimeEqual(*existing.EffectiveTo, *candidate.EffectiveTo) {
		return false
	}
	return jsonDeepEqual(existing.ExtraRates, candidate.ExtraRates)
}

// pgTimeEqual compares instants at PG timestamptz precision: PostgreSQL
// rounds fractional seconds to the nearest microsecond on input with rint
// (round HALF TO EVEN), so a stored row holds rint(candidate, 1µs) —
// mirroring that rounding on both sides makes an identical replay compare
// equal even when the candidate carries a sub-microsecond tail
// (评审轮1/轮2 finding：duplicate 误判；time.Round 是 half-away-from-zero,
// 在半微秒边界与 rint 不一致).
func pgTimeEqual(a, b time.Time) bool {
	return pgRoundMicros(a) == pgRoundMicros(b)
}

// pgRoundMicros mirrors PG's rint() on the µs grid (round half to even).
// 整数算术，不走 float64：纪元年份的 UnixNano ≈ 1.8e18 超出 float64 精确
// 整数范围（间距 256ns），math.RoundToEven(float64(ns)/1000) 会把一部分
// 亚微秒尾数舍错（实测 ~1.3% 偏差）。
func pgRoundMicros(t time.Time) int64 {
	ns := t.UnixNano()
	us := ns / 1000
	rem := ns % 1000
	if rem < 0 { // 负值先归一到 floor 语义
		us--
		rem += 1000
	}
	switch {
	case rem > 500:
		us++
	case rem == 500 && us%2 != 0: // 恰好半微秒：舍到偶数微秒（rint）
		us++
	}
	return us * 1000
}

// jsonDeepEqual compares two JSON documents by value (key order insensitive).
// UseNumber 保住大整数（micros 价目可能超 float64 精确范围），不经 float64
// 坍缩（评审轮2 finding）；数值叶子按 PG jsonb 的规范化口径比较——jsonb
// 入库即把 1e2 规范为 100、1.5e-7 规范为 0.00000015，字节相同的指数形
// extra_rates 重放必须判 duplicate（评审轮3 finding）。
func jsonDeepEqual(a, b json.RawMessage) bool {
	decode := func(raw json.RawMessage) (any, bool) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		return v, true
	}
	va, oka := decode(a)
	vb, okb := decode(b)
	if !oka || !okb {
		return false
	}
	return jsonValueEqual(va, vb)
}

// jsonValueEqual recursively compares two UseNumber-decoded JSON values;
// json.Number leaves compare numerically (big.Rat 精确处理指数/小数形式),
// everything else structurally.
func jsonValueEqual(a, b any) bool {
	if na, ok := a.(json.Number); ok {
		nb, ok := b.(json.Number)
		if !ok {
			return false
		}
		return jsonNumberEqual(na, nb)
	}
	switch ta := a.(type) {
	case map[string]any:
		tb, ok := b.(map[string]any)
		if !ok || len(ta) != len(tb) {
			return false
		}
		for k, va := range ta {
			vb, ok := tb[k]
			if !ok || !jsonValueEqual(va, vb) {
				return false
			}
		}
		return true
	case []any:
		tb, ok := b.([]any)
		if !ok || len(ta) != len(tb) {
			return false
		}
		for i := range ta {
			if !jsonValueEqual(ta[i], tb[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// jsonNumberEqual compares two JSON numbers by exact rational value; SetString
// 解析失败（非数值）退化为字符串比较。
func jsonNumberEqual(a, b json.Number) bool {
	ra, oka := new(big.Rat).SetString(string(a))
	rb, okb := new(big.Rat).SetString(string(b))
	if !oka || !okb {
		return a == b
	}
	return ra.Cmp(rb) == 0
}
