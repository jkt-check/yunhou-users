// entitlement_admin.go — 存量权益增补 admin 面（entitlement-model-amendment
// 需求，deploy 侧 PR #345）：R1 单权益修订、R2 批量增补（模型发布收尾）、
// R3 只读、R4 审计（复用 inference_audit_log 运营审计面，同事务落库）。
//
// 语义红线（需求 §5，本文件逐条落实）：
//   - 显式集合：model_ids 空数组 = 无任何模型；增补只产出显式集合，永不
//     引入 NULL/省略 = 全放行的写法。
//   - 消费主体连续：只改 model_ids；ID/anchor/有效期不动，used/reserved
//     窗口不清零（由 ReviseEntitlementTx 的 UPDATE 语句保证，本层不动 SQL）。
//   - 仅 active 可修订：access.Upgrade 守卫；批量选择器命中的非 active 行
//     不修改、计入 skipped 明细（AC6），绝不复活退役权益。
//   - 乐观锁：stale revision → CodeConflict，不强制覆盖；R2 中冲突行进
//     Conflicts/errors[] 明细，不置整体失败（N6 迁移工具先例）。
//   - 生效即时：Resolve 每请求查库，本层不引入任何权益缓存。
package management

import (
	"context"
	"strings"

	"github.com/yunhou/users/internal/inference/access"
	"github.com/yunhou/users/internal/inference/domain"
)

// R2 批量选择器键（wire 形状：all_active / source_plan:<plan_id>）。
const (
	SelectorAllActive  = "all_active"
	SelectorSourcePlan = "source_plan"
)

// R2 批量动作。
const (
	AmendActionAdd    = "add"
	AmendActionRemove = "remove"
)

// maxAmendSkippedDetail 是 skipped_detail 明细的硬上限（all_active 在大型
// 部署可能命中成千上万行；计数字段始终是权威全量，明细只作排障抽样）。
const maxAmendSkippedDetail = 500

// maxAmendSample 是 dry_run 抽样行数上限。
const maxAmendSample = 50

// defaultAmendSample 是 dry_run 默认抽样行数。
const defaultAmendSample = 10

// EntitlementListFilter 是 R3 列表的可选过滤（空值 = 不过滤）。
type EntitlementListFilter struct {
	BillingAccountID string
	Status           string // domain.EntitlementStatus；空 = 全部状态
	Limit            int    // <=0 = 不分页上限（handler 层已给默认值）
	Offset           int
}

// EntitlementOperator 是修订操作的归因双腿（JWT 用户 + 服务身份），由
// httpapi 层从授权中间件上下文转换而来（management 不 import credentials，
// 避免与 credentials→management 的既有依赖成环）。
type EntitlementOperator struct {
	UserID string
	AppID  string
}

// EntitlementAdminStore 是本服务对持久层的最小依赖（postgres.Store 实现）。
type EntitlementAdminStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	GetEntitlement(ctx context.Context, id string) (*domain.Entitlement, error)
	GetEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string) (*domain.Entitlement, error)
	ReviseEntitlementTx(ctx context.Context, w domain.UnitOfWork, id string, patch domain.EntitlementPatch) (*domain.Entitlement, error)
	ListEntitlementsAdmin(ctx context.Context, f EntitlementListFilter) ([]domain.Entitlement, error)
	ListEntitlementsForAmend(ctx context.Context, selectorKey, selectorVal string) ([]domain.Entitlement, error)
}

// EntitlementAdminService 承载 R1/R2/R3 的全部业务逻辑；handler 只做
// HTTP↔服务转换。
type EntitlementAdminService struct {
	store EntitlementAdminStore
	audit AuditRecorder
}

// NewEntitlementAdminService wires the service. audit 为 nil 时一切写操作
// fail-closed（无审计的权益变更绝不落库，对齐 grantAuditSupport 先例）。
func NewEntitlementAdminService(store EntitlementAdminStore, audit AuditRecorder) *EntitlementAdminService {
	return &EntitlementAdminService{store: store, audit: audit}
}

// txAuditSupport 解析同事务审计通道：变更与审计行同事务提交或回滚，缺
// tx 能力即拒绝变更（评审先例：AdminAuthHandler.grantAuditSupport）。
func (s *EntitlementAdminService) txAuditSupport() (AuditTxRecorder, error) {
	txAudit, ok := s.audit.(AuditTxRecorder)
	if !ok {
		return nil, domain.NewError(domain.CodeInternal, "audit recorder lacks transactional support")
	}
	return txAudit, nil
}

// --- R3 只读 ---

// Get returns one entitlement by id（不存在 → CodeNotFound → 404）。
func (s *EntitlementAdminService) Get(ctx context.Context, id string) (*domain.Entitlement, error) {
	return s.store.GetEntitlement(ctx, id)
}

// List returns entitlements by filter（R3 列表；不过滤任何状态，operator
// 核对增补结果需要看到 revoked/expired 行）。
func (s *EntitlementAdminService) List(ctx context.Context, f EntitlementListFilter) ([]domain.Entitlement, error) {
	return s.store.ListEntitlementsAdmin(ctx, f)
}

// --- R1 单权益修订 ---

// ReviseInput 是 R1 端点的入参（至少 add/remove 其一非空；reason 必填）。
type ReviseInput struct {
	AddModelIDs    []string
	RemoveModelIDs []string
	Reason         string
}

// ReviseResult：Changed=false 表示集合本无变化（幂等短路，未调 Upgrade、
// 未写库、未写审计）；Entitlement 始终携带最新视图。
type ReviseResult struct {
	Changed     bool
	Entitlement *domain.Entitlement
}

// Revise executes R1：同一事务内 读→算→Upgrade→落库→审计。幂等：已含/
// 已不含目标模型 = Changed=false 短路返回（端点层语义 200，不调 Upgrade
// ——Upgrade 的 no-op 拒绝是防调用方漏判的第二道闸，二者不矛盾）。
func (s *EntitlementAdminService) Revise(ctx context.Context, op EntitlementOperator, id string, in ReviseInput) (*ReviseResult, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	add := normalizeModelIDs(in.AddModelIDs)
	remove := normalizeModelIDs(in.RemoveModelIDs)
	if len(add) == 0 && len(remove) == 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "add_model_ids / remove_model_ids 至少其一非空")
	}
	if dup := intersectModelIDs(add, remove); dup != "" {
		return nil, domain.NewError(domain.CodeInvalidInput,
			"add_model_ids 与 remove_model_ids 不得含同一模型: "+dup)
	}
	txAudit, err := s.txAuditSupport()
	if err != nil {
		return nil, err
	}

	uow, err := s.store.Begin(ctx)
	if err != nil {
		return nil, err
	}
	ent, err := s.store.GetEntitlementTx(ctx, uow, id)
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	newSet, changed := reviseModelSet(ent.ModelIDs, add, remove)
	if !changed {
		_ = uow.Rollback(ctx)
		return &ReviseResult{Changed: false, Entitlement: ent}, nil
	}
	patch, err := access.Upgrade(ent, ent.PolicyVersionID, newSet)
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	revised, err := s.store.ReviseEntitlementTx(ctx, uow, id, patch)
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	if err := txAudit.RecordTx(ctx, uow, AuditEvent{
		Action: "entitlement.revise", ObjectType: "entitlement", ObjectID: id,
		Reason: reason, ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: SanitizeDetail(reviseAuditDetail(ent, revised, nil)),
	}); err != nil {
		_ = uow.Rollback(ctx)
		return nil, domain.WrapError(domain.CodeInternal, "audit write failed", err)
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	return &ReviseResult{Changed: true, Entitlement: revised}, nil
}

// --- R2 批量增补 ---

// AmendRequest 是 R2 端点的入参。DryRun 默认 true（handler 层保证零值
// 语义外的显式默认）；SampleN <=0 回落默认 10，上限 50。
type AmendRequest struct {
	ModelID  string
	Action   string // add | remove
	Selector string // all_active | source_plan:<plan_id>
	Reason   string
	DryRun   bool
	SampleN  int
}

// AmendRowInfo 是 errors[] / skipped_detail 的行明细（N6 迁移工具
// rowError 先例的权益版）。
type AmendRowInfo struct {
	EntitlementID    string `json:"entitlement_id"`
	BillingAccountID string `json:"billing_account_id"`
	Reason           string `json:"reason"`
}

// AmendSample 是 dry_run 抽样行（扫描顺序前 N 行，含变更前后集合）。
type AmendSample struct {
	EntitlementID    string   `json:"entitlement_id"`
	BillingAccountID string   `json:"billing_account_id"`
	Before           []string `json:"before_model_ids"`
	After            []string `json:"after_model_ids,omitempty"`
	Note             string   `json:"note"`
}

// AmendReport 是 R2 的审计文档（dry_run 与正式执行同形）。dry_run 下
// Amended 是「将影响行数」；正式执行下 Errors 收集行级失败（含乐观锁
// 冲突行，reason 前缀 "conflict:" 并同时计入 Conflicts），批次始终收敛
// 返回，调用方解析 body 而非依赖 HTTP 状态码。
type AmendReport struct {
	DryRun   bool   `json:"dry_run"`
	ModelID  string `json:"model_id"`
	Action   string `json:"action"`
	Selector string `json:"selector"`

	Scanned   int `json:"scanned"`
	Amended   int `json:"amended"`
	Skipped   int `json:"skipped"`
	Conflicts int `json:"conflicts"`

	Errors                 []AmendRowInfo `json:"errors"`
	SkippedDetail          []AmendRowInfo `json:"skipped_detail"`
	SkippedDetailTruncated bool           `json:"skipped_detail_truncated,omitempty"`
	Samples                []AmendSample  `json:"samples"`
}

// AmendModels executes R2。selector 解析：all_active（全部 active 权益）/
// source_plan:<plan_id>（该套餐来源订阅的权益，含非 active 行——它们不被
// 修改，进 skipped 明细，AC6）。行级粒度事务（行级乐观锁），不要求全批量
// 单事务。
func (s *EntitlementAdminService) AmendModels(ctx context.Context, op EntitlementOperator, in AmendRequest) (*AmendReport, error) {
	modelID := strings.TrimSpace(in.ModelID)
	if modelID == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "model_id is required")
	}
	if in.Action != AmendActionAdd && in.Action != AmendActionRemove {
		return nil, domain.NewError(domain.CodeInvalidInput, "action must be add or remove")
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, domain.NewError(domain.CodeInvalidInput, "reason is required")
	}
	key, val, err := parseAmendSelector(in.Selector)
	if err != nil {
		return nil, err
	}
	sampleN := in.SampleN
	if sampleN <= 0 {
		sampleN = defaultAmendSample
	}
	if sampleN > maxAmendSample {
		sampleN = maxAmendSample
	}

	rep := &AmendReport{
		DryRun: in.DryRun, ModelID: modelID, Action: in.Action, Selector: in.Selector,
		Errors: []AmendRowInfo{}, SkippedDetail: []AmendRowInfo{}, Samples: []AmendSample{},
	}

	rows, err := s.store.ListEntitlementsForAmend(ctx, key, val)
	if err != nil {
		return nil, err
	}
	rep.Scanned = len(rows)

	var add, remove []string
	if in.Action == AmendActionAdd {
		add = []string{modelID}
	} else {
		remove = []string{modelID}
	}

	for i := range rows {
		ent := &rows[i]
		note, after := "", []string(nil)
		switch {
		case ent.Status != domain.EntitlementActive:
			// 绝不复活退役权益（红线 3）；命中的非 active 行进 skipped 明细。
			note = "not active: " + string(ent.Status)
			rep.Skipped++
			rep.appendSkipped(ent, note)
		default:
			newSet, changed := reviseModelSet(ent.ModelIDs, add, remove)
			if !changed {
				if in.Action == AmendActionAdd {
					note = "already contains model"
				} else {
					note = "model not in set"
				}
				rep.Skipped++
				rep.appendSkipped(ent, note)
				break
			}
			after = newSet
			if in.DryRun {
				note = "would amend"
				rep.Amended++
				break
			}
			// 正式执行：行级独立事务（读→Upgrade→落库→审计，同 R1 路径）。
			_, rerr := s.reviseOneTx(ctx, op, ent, newSet, reason, "entitlement.amend", map[string]any{
				"batch_action": in.Action, "batch_selector": in.Selector, "batch_model_id": modelID,
			})
			switch {
			case rerr == nil:
				note = "amended"
				rep.Amended++
			case domain.CodeOf(rerr) == domain.CodeConflict:
				note = "conflict: " + rerr.Error()
				rep.Conflicts++
				rep.Errors = append(rep.Errors, AmendRowInfo{ent.ID, ent.BillingAccountID, note})
			default:
				note = "error: " + rerr.Error()
				rep.Errors = append(rep.Errors, AmendRowInfo{ent.ID, ent.BillingAccountID, note})
			}
		}
		if len(rep.Samples) < sampleN {
			rep.Samples = append(rep.Samples, AmendSample{
				EntitlementID: ent.ID, BillingAccountID: ent.BillingAccountID,
				Before: ent.ModelIDs, After: after, Note: note,
			})
		}
	}
	return rep, nil
}

// reviseOneTx 是 R1/R2 共享的单行修订事务：以调用方已读的权益行计算
// patch（集合变化已判定），落库 + 审计同事务。R2 调用前已在批外读过行
// （ListEntitlementsForAmend），乐观锁以该行的 revision 为期望——批扫描
// 与行修订之间的并发修改在此被 CodeConflict 捕获并计入 Conflicts，而非
// 静默覆盖（红线 4）。
func (s *EntitlementAdminService) reviseOneTx(ctx context.Context, op EntitlementOperator, ent *domain.Entitlement, newSet []string, reason, action string, extraDetail map[string]any) (*domain.Entitlement, error) {
	txAudit, err := s.txAuditSupport()
	if err != nil {
		return nil, err
	}
	patch, err := access.Upgrade(ent, ent.PolicyVersionID, newSet)
	if err != nil {
		return nil, err
	}
	uow, err := s.store.Begin(ctx)
	if err != nil {
		return nil, err
	}
	revised, err := s.store.ReviseEntitlementTx(ctx, uow, ent.ID, patch)
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	if err := txAudit.RecordTx(ctx, uow, AuditEvent{
		Action: action, ObjectType: "entitlement", ObjectID: ent.ID,
		Reason: reason, ActorUser: op.UserID, ActorApp: op.AppID,
		Detail: SanitizeDetail(reviseAuditDetail(ent, revised, extraDetail)),
	}); err != nil {
		_ = uow.Rollback(ctx)
		return nil, domain.WrapError(domain.CodeInternal, "audit write failed", err)
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	return revised, nil
}

// reviseAuditDetail 组装 R4 要求的归因明细：前后 model_ids、前后
// policy_version_id、前后 revision（时间戳由审计表自带）。
func reviseAuditDetail(before, after *domain.Entitlement, extra map[string]any) map[string]any {
	d := map[string]any{
		"before_model_ids":         before.ModelIDs,
		"after_model_ids":          after.ModelIDs,
		"before_policy_version_id": before.PolicyVersionID,
		"after_policy_version_id":  after.PolicyVersionID,
		"before_revision":          before.Revision,
		"after_revision":           after.Revision,
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// appendSkipped 追加 skipped 明细（超上限置 truncated 标记，计数字段始终
// 是权威全量）。
func (r *AmendReport) appendSkipped(ent *domain.Entitlement, note string) {
	if len(r.SkippedDetail) >= maxAmendSkippedDetail {
		r.SkippedDetailTruncated = true
		return
	}
	r.SkippedDetail = append(r.SkippedDetail, AmendRowInfo{ent.ID, ent.BillingAccountID, note})
}

// parseAmendSelector 解析 R2 选择器：all_active / source_plan:<plan_id>。
func parseAmendSelector(sel string) (key, val string, err error) {
	sel = strings.TrimSpace(sel)
	switch {
	case sel == SelectorAllActive:
		return SelectorAllActive, "", nil
	case strings.HasPrefix(sel, SelectorSourcePlan+":"):
		planID := strings.TrimSpace(strings.TrimPrefix(sel, SelectorSourcePlan+":"))
		if planID == "" {
			return "", "", domain.NewError(domain.CodeInvalidInput, "source_plan selector requires a plan id")
		}
		return SelectorSourcePlan, planID, nil
	default:
		return "", "", domain.NewError(domain.CodeInvalidInput,
			"selector must be all_active or source_plan:<plan_id>")
	}
}

// reviseModelSet 计算新显式集合：从 current 中减去 remove、并入 add，
// 保序（既有顺序优先，add 追加在后），add/remove 已在外层 normalize。
// 返回 changed=false 表示集合无变化（幂等短路）。
func reviseModelSet(current, add, remove []string) ([]string, bool) {
	removeSet := make(map[string]struct{}, len(remove))
	for _, id := range remove {
		removeSet[id] = struct{}{}
	}
	changed := false
	out := make([]string, 0, len(current)+len(add))
	inSet := make(map[string]struct{}, len(current)+len(add))
	for _, id := range current {
		if _, ok := removeSet[id]; ok {
			changed = true
			continue
		}
		out = append(out, id)
		inSet[id] = struct{}{}
	}
	for _, id := range add {
		if _, ok := inSet[id]; ok {
			continue
		}
		inSet[id] = struct{}{}
		out = append(out, id)
		changed = true
	}
	return out, changed
}

// normalizeModelIDs 去空白、去空串、去重（保序）。显式集合语义：返回值
// 永远是显式列表，不存在 nil = 全放行。
func normalizeModelIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// intersectModelIDs 返回 add 与 remove 的第一个交集（空串 = 无交集）。
func intersectModelIDs(a, b []string) string {
	set := make(map[string]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := set[id]; ok {
			return id
		}
	}
	return ""
}
