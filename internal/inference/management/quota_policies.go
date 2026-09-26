// quota_policies.go — 配额策略(inference_policy_versions)运营生命周期
// 服务(spec: docs/superpowers/specs/2026-09-26-admin-quota-policies-design.md;
// 需求: yunhou-users-quota-policies-api-requirements.md)。
//
// 口径(Q1–Q4 已拍板):
//   - 权限过渡期挂 models:manage(未来收敛 quota:manage,文档标注);
//   - publish 同事务将同名旧 published 转 superseded(Q3),读侧(新发放
//     引用)只认 published;迁移 040 在 DB 层兜底「每 name 至多一条
//     published」;
//   - retire 遇 active 权益引用默认 409 + referenced_by,?force=true 放行
//     (Q1);已引用权益继续按 pinned 版本执行(pin 不变性,验收 A13);
//   - 不提供物理删除(Q4),retire 即审计可回滚的终态;
//   - 写 + 审计同事务(RecordTx,price-versions 同款 fail-closed);
//     publish/retire 对同 id 重复调用幂等返回当前状态、不产生第二条审计。
package management

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/yunhou/users/internal/inference/domain"
)

// QuotaPolicyInfo 是策略版本在管理层面的投影(与 postgres.PolicyVersion
// 同形,微元限额用 int64 指针;NULL = 该项不启用)。
type QuotaPolicyInfo struct {
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
	CreatedAt        time.Time
	PublishedAt      *time.Time
}

// QuotaPolicyFilter narrows the admin list query (name 精确 + status + 分页).
type QuotaPolicyFilter struct {
	Name   string
	Status string
	Limit  int
	Offset int
}

// QuotaPolicyStore is the persistence surface QuotaPolicyService needs;
// satisfied by inference/postgres.Store.
type QuotaPolicyStore interface {
	GetModel(ctx context.Context, id string) (*domain.Model, error)
	GetQuotaPolicyVersion(ctx context.Context, id string) (*QuotaPolicyInfo, error)
	ListQuotaPolicies(ctx context.Context, f QuotaPolicyFilter) ([]QuotaPolicyInfo, error)
	// CountActiveEntitlementsByPolicy counts active entitlements pinning this
	// version (status='active' AND 未过 effective_to 窗口) — retire 保护
	// 规则与详情页 referenced_by 的唯一数据源。
	CountActiveEntitlementsByPolicy(ctx context.Context, policyVersionID string) (int, error)
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	// MaxPolicyRevisionTx reads MAX(revision) of one name inside the caller's
	// UnitOfWork (0 when none) — create 的服务端 revision 派生(max+1)。
	MaxPolicyRevisionTx(ctx context.Context, w domain.UnitOfWork, name string) (int, error)
	// InsertQuotaPolicyTx appends the new revision inside the caller's
	// UnitOfWork(写 + 审计同事务)and fills ID/CreatedAt.
	InsertQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, p *QuotaPolicyInfo) error
	// UpdateDraftQuotaPolicyTx replaces the mutable content of a DRAFT row;
	// 0 rows (非 draft)→ CodeConflict。
	UpdateDraftQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, p *QuotaPolicyInfo) error
	// PublishQuotaPolicyTx atomically supersedes the name's current
	// published row (Q3) and marks the target draft published at `at`;
	// 目标非 draft → CodeConflict。
	PublishQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, id, name string, at time.Time) error
	// RetireQuotaPolicyTx marks draft/published/superseded → retired;
	// 已 retired → CodeConflict。
	RetireQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, id string) error
	// GetQuotaPolicyForUpdateTx locks the policy row (FOR UPDATE) inside the
	// caller's transaction — retire 引用保护的 TOCTOU 闭合:行锁与并发
	// grant 的 FK KEY SHARE 互斥,事务内计数在提交前不失效。
	GetQuotaPolicyForUpdateTx(ctx context.Context, w domain.UnitOfWork, id string) (*QuotaPolicyInfo, error)
	// CountActiveEntitlementsByPolicyTx is the reference count inside the
	// caller's transaction (与行锁配套使用).
	CountActiveEntitlementsByPolicyTx(ctx context.Context, w domain.UnitOfWork, policyVersionID string) (int, error)
}

// CreateQuotaPolicyInput is the payload of POST /admin/quota-policies after
// strict decode; revision 服务端派生,不入请求。
type CreateQuotaPolicyInput struct {
	Name             string
	ModelIDs         []string
	FiveHourLimit    *int64
	WeeklyLimit      *int64
	MonthlyLimit     *int64
	RPMLimit         *int
	TPMLimit         *int64
	ConcurrencyLimit *int
	OveragePolicy    string
	Reason           string
}

// UpdateQuotaPolicyInput is PATCH /admin/quota-policies/:id after presence
// decoding: Set 记录请求体里出现的字段(JSON 字段名),缺席字段保留原值,
// 显式 null(Set 有键、指针 nil)清空该限制项。
type UpdateQuotaPolicyInput struct {
	Reason           string
	Set              map[string]bool
	ModelIDs         []string
	FiveHourLimit    *int64
	WeeklyLimit      *int64
	MonthlyLimit     *int64
	RPMLimit         *int
	TPMLimit         *int64
	ConcurrencyLimit *int
	OveragePolicy    string
}

// QuotaPolicyRetireConflictError reports a retire blocked by live
// entitlement references (Q1 默认口径);handler 据此返回 409 +
// data.referenced_by。
type QuotaPolicyRetireConflictError struct {
	Info         *QuotaPolicyInfo
	ReferencedBy int
}

func (e *QuotaPolicyRetireConflictError) Error() string {
	return fmt.Sprintf("quota policy %s r%d is referenced by %d active entitlement(s); use ?force=true to retire anyway (存量权益继续按 pinned 版本执行)",
		e.Info.Name, e.Info.Revision, e.ReferencedBy)
}

func (e *QuotaPolicyRetireConflictError) Unwrap() error {
	return domain.NewError(domain.CodeConflict, e.Error())
}

// QuotaPolicyService is the operator-facing quota-policy surface.
type QuotaPolicyService struct {
	store QuotaPolicyStore
	audit AuditRecorder
	clock domain.Clock
}

// NewQuotaPolicyService builds the service; nil clock uses UTC, nil audit
// skips the audit trail (unit tests only — production always wires one).
func NewQuotaPolicyService(store QuotaPolicyStore, audit AuditRecorder, clock domain.Clock) *QuotaPolicyService {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &QuotaPolicyService{store: store, audit: audit, clock: clock}
}

var quotaPolicyNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var quotaPolicyOveragePolicies = map[string]bool{
	"reject": true, "clamp_if_declared": true, "allow_overage": true,
}

// validatePolicyContent 是 create/update 合并后内容的统一校验:至少一项
// 限制、每项 >0、overage 枚举、model_ids 非空且存在于 catalog。
func (s *QuotaPolicyService) validatePolicyContent(ctx context.Context, p *QuotaPolicyInfo) error {
	limits := []struct {
		name string
		v    *int64
	}{
		{"five_hour_limit_micros", p.FiveHourLimit},
		{"weekly_limit_micros", p.WeeklyLimit},
		{"monthly_limit_micros", p.MonthlyLimit},
		{"tpm_limit", p.TPMLimit},
	}
	anyLimit := false
	for _, l := range limits {
		if l.v == nil {
			continue
		}
		if *l.v <= 0 {
			return domain.NewError(domain.CodeInvalidInput, l.name+" must be a positive integer or null")
		}
		anyLimit = true
	}
	if p.RPMLimit != nil {
		if *p.RPMLimit <= 0 {
			return domain.NewError(domain.CodeInvalidInput, "rpm_limit must be a positive integer or null")
		}
		anyLimit = true
	}
	if p.ConcurrencyLimit != nil {
		if *p.ConcurrencyLimit <= 0 {
			return domain.NewError(domain.CodeInvalidInput, "concurrency_limit must be a positive integer or null")
		}
		anyLimit = true
	}
	if !anyLimit {
		return domain.NewError(domain.CodeInvalidInput, "策略无任何限制项:至少一项 limit 必须非 null")
	}
	if !quotaPolicyOveragePolicies[p.OveragePolicy] {
		return domain.NewError(domain.CodeInvalidInput, "unknown overage_policy "+p.OveragePolicy)
	}
	if len(p.ModelIDs) == 0 {
		return domain.NewError(domain.CodeInvalidInput, "model_ids must be non-empty")
	}
	for _, id := range p.ModelIDs {
		if _, err := s.store.GetModel(ctx, id); err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound {
				return domain.NewError(domain.CodeInvalidInput, "model not found: "+id)
			}
			return err
		}
	}
	return nil
}

// txAudit records the audit row INSIDE the caller's transaction (写 + 审计
// 同事务;记录器不支持事务写入时整个变更失败,fail-closed)。
func (s *QuotaPolicyService) txAudit(ctx context.Context, w domain.UnitOfWork, actor, action, objectID, reason string, detail map[string]any) error {
	if s.audit == nil {
		return nil
	}
	txRec, ok := s.audit.(AuditTxRecorder)
	if !ok {
		return domain.NewError(domain.CodeInternal, "quota policy: audit recorder lacks transactional support")
	}
	userID, appID := ParseActor(actor)
	return txRec.RecordTx(ctx, w, AuditEvent{
		Action: action, ObjectType: "quota_policy", ObjectID: objectID,
		Reason: reason, ActorUser: userID, ActorApp: appID,
		Detail: SanitizeDetail(detail),
	})
}

// Create appends a new draft revision:revision 服务端派生(max+1,事务内),
// 并发同 name 撞 UNIQUE(name,revision) → 409(不跳号/重号);overage 缺省
// reject。
func (s *QuotaPolicyService) Create(ctx context.Context, actor string, in CreateQuotaPolicyInput) (*QuotaPolicyInfo, error) {
	if !quotaPolicyNamePattern.MatchString(in.Name) {
		return nil, domain.NewError(domain.CodeInvalidInput, "invalid name (must match ^[a-z0-9][a-z0-9-]{0,62}$)")
	}
	if in.Reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	overage := in.OveragePolicy
	if overage == "" {
		overage = "reject"
	}
	candidate := &QuotaPolicyInfo{
		Name: in.Name, ModelIDs: in.ModelIDs,
		FiveHourLimit: in.FiveHourLimit, WeeklyLimit: in.WeeklyLimit, MonthlyLimit: in.MonthlyLimit,
		RPMLimit: in.RPMLimit, TPMLimit: in.TPMLimit, ConcurrencyLimit: in.ConcurrencyLimit,
		OveragePolicy: overage, Status: "draft",
	}
	if err := s.validatePolicyContent(ctx, candidate); err != nil {
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
	maxRev, err := s.store.MaxPolicyRevisionTx(ctx, uow, in.Name)
	if err != nil {
		return nil, err
	}
	if maxRev+1 > math.MaxInt32 {
		return nil, domain.NewError(domain.CodeInvalidInput, "revision out of range")
	}
	candidate.Revision = maxRev + 1
	if err := s.store.InsertQuotaPolicyTx(ctx, uow, candidate); err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			// 并发同 name 创建撞唯一键:输家 409,不得跳号/重号。
			return nil, domain.NewError(domain.CodeConflict, "quota policy revision conflict for (name, revision)")
		}
		return nil, err
	}
	if err := s.txAudit(ctx, uow, actor, "quota_policy.create", candidate.ID, in.Reason, map[string]any{
		"name": candidate.Name, "revision": candidate.Revision,
		"status_from": "", "status_to": "draft",
		"model_ids": candidate.ModelIDs, "overage_policy": candidate.OveragePolicy,
	}); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "quota policy audit write failed", err)
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return candidate, nil
}

// Update edits a DRAFT revision:presence 语义(缺席保留、显式 null 清空),
// 空修改 400;非 draft → 409「已发布策略不可变,请出新 revision」;合并后
// 重走内容校验。
func (s *QuotaPolicyService) Update(ctx context.Context, actor, id string, in UpdateQuotaPolicyInput) (*QuotaPolicyInfo, error) {
	if in.Reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	cur, err := s.store.GetQuotaPolicyVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != "draft" {
		return nil, domain.NewError(domain.CodeConflict,
			"quota policy is "+cur.Status+" — 已发布策略不可变,请出新 revision")
	}
	if len(in.Set) == 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "empty update: at least one mutable field is required")
	}
	merged := *cur
	if in.Set["model_ids"] {
		merged.ModelIDs = in.ModelIDs
	}
	if in.Set["five_hour_limit_micros"] {
		merged.FiveHourLimit = in.FiveHourLimit
	}
	if in.Set["weekly_limit_micros"] {
		merged.WeeklyLimit = in.WeeklyLimit
	}
	if in.Set["monthly_limit_micros"] {
		merged.MonthlyLimit = in.MonthlyLimit
	}
	if in.Set["rpm_limit"] {
		merged.RPMLimit = in.RPMLimit
	}
	if in.Set["tpm_limit"] {
		merged.TPMLimit = in.TPMLimit
	}
	if in.Set["concurrency_limit"] {
		merged.ConcurrencyLimit = in.ConcurrencyLimit
	}
	if in.Set["overage_policy"] {
		merged.OveragePolicy = in.OveragePolicy
	}
	if err := s.validatePolicyContent(ctx, &merged); err != nil {
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
	if err := s.store.UpdateDraftQuotaPolicyTx(ctx, uow, &merged); err != nil {
		return nil, err
	}
	if err := s.txAudit(ctx, uow, actor, "quota_policy.update", merged.ID, in.Reason, map[string]any{
		"name": merged.Name, "revision": merged.Revision,
		"status_from": cur.Status, "status_to": merged.Status,
		"changed_fields": setKeys(in.Set),
	}); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "quota policy audit write failed", err)
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	return &merged, nil
}

// Publish:draft → published(同事务转同名旧 published 为 superseded,Q3);
// 同 id 重复调用幂等返回当前状态、不产生第二条审计;superseded/retired →
// 409。
func (s *QuotaPolicyService) Publish(ctx context.Context, actor, id, reason string) (*QuotaPolicyInfo, error) {
	if reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	cur, err := s.store.GetQuotaPolicyVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == "published" {
		return cur, nil // 幂等重放(需求 §5.4)
	}
	if cur.Status != "draft" {
		return nil, domain.NewError(domain.CodeConflict,
			"only draft policies can be published (got "+cur.Status+")")
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
	at := s.clock.Now().UTC()
	if err := s.store.PublishQuotaPolicyTx(ctx, uow, cur.ID, cur.Name, at); err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			// 并发双发同 id:输家在事务内撞「非 draft」或部分唯一索引;
			// 重读已被赢家发布则按幂等 200 返回(评审轮1 finding 3/5,
			// 冲突文案固定,不外泄索引名)。
			if again, rerr := s.store.GetQuotaPolicyVersion(ctx, id); rerr == nil && again.Status == "published" {
				return again, nil
			}
			return nil, domain.NewError(domain.CodeConflict, "quota policy publish conflict for (name)")
		}
		return nil, err
	}
	if err := s.txAudit(ctx, uow, actor, "quota_policy.publish", cur.ID, reason, map[string]any{
		"name": cur.Name, "revision": cur.Revision,
		"status_from": "draft", "status_to": "published",
	}); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "quota policy audit write failed", err)
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	published := *cur
	published.Status = "published"
	published.PublishedAt = &at
	return &published, nil
}

// Retire:draft 直接转 retired(等同废弃草稿);retired 幂等返回;
// published/superseded 遇 active 权益引用默认 409 + referenced_by(Q1),
// force=true 放行 —— 存量权益继续按 pinned 版本执行,仅不再允许新发放
// 引用。引用计数在持行锁的事务内做(FOR UPDATE 与并发 grant 的 FK
// KEY SHARE 互斥),审计里的 referenced_by 是退役时刻的真实值。
func (s *QuotaPolicyService) Retire(ctx context.Context, actor, id, reason string, force bool) (*QuotaPolicyInfo, error) {
	if reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	cur, err := s.store.GetQuotaPolicyVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == "retired" {
		return cur, nil // 幂等重放(需求 §5.4)
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
	// 行锁内重读:与并发 retire/grant 定序;锁内状态才参与决策。
	locked, err := s.store.GetQuotaPolicyForUpdateTx(ctx, uow, id)
	if err != nil {
		return nil, err
	}
	if locked.Status == "retired" {
		return locked, nil // 并发 retire 先到者已生效,后来者幂等
	}
	referencedBy := 0
	if locked.Status != "draft" {
		referencedBy, err = s.store.CountActiveEntitlementsByPolicyTx(ctx, uow, id)
		if err != nil {
			return nil, err
		}
		if referencedBy > 0 && !force {
			return nil, &QuotaPolicyRetireConflictError{Info: locked, ReferencedBy: referencedBy}
		}
	}
	if err := s.store.RetireQuotaPolicyTx(ctx, uow, locked.ID); err != nil {
		return nil, err
	}
	if err := s.txAudit(ctx, uow, actor, "quota_policy.retire", locked.ID, reason, map[string]any{
		"name": locked.Name, "revision": locked.Revision,
		"status_from": locked.Status, "status_to": "retired",
		"referenced_by": referencedBy, "force": force,
	}); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "quota policy audit write failed", err)
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, domain.WrapError(domain.CodeInternal, "commit transaction", err)
	}
	committed = true
	retired := *locked
	retired.Status = "retired"
	return &retired, nil
}

// Get loads one revision with its referenced_by count (详情页)。
func (s *QuotaPolicyService) Get(ctx context.Context, id string) (*QuotaPolicyInfo, int, error) {
	info, err := s.store.GetQuotaPolicyVersion(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	n, err := s.store.CountActiveEntitlementsByPolicy(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	return info, n, nil
}

// List returns the filtered page; the handler owns query-parameter
// validation (status 白名单、offset >= 0、limit 钳制).
func (s *QuotaPolicyService) List(ctx context.Context, f QuotaPolicyFilter) ([]QuotaPolicyInfo, error) {
	return s.store.ListQuotaPolicies(ctx, f)
}

func setKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	return keys
}
