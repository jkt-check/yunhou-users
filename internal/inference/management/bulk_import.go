// bulk_import.go — 运营批量导入（Task 15，设计 §5/§9.2）。
//
// 语义（控制者决定 8，本任务的选定口径）：
//   - dry_run=true：只校验 + 预演逐项结果（would_insert / would_skip /
//     error），一行不写。部分错误的预览只存在于 dry-run 响应中。
//   - dry_run=false（commit）：**全部有效才落库**——任一 item 出错则整个
//     任务 400，一行不写（绝不半发布）。不存在"只发布有效子集"的隐式
//     路径；运营要导入子集就删掉错误项重新提交（新 task_id）。
//   - 幂等任务 ID：commit 与目录写入同一事务，inference_bulk_imports 的
//     task_id 唯一键兜底；同文档重复提交重放已记录的结果（replayed=true），
//     不重复创建；同 task_id 异文档是键复用 → 409（M-4，migration 039
//     document_hash 比对，与 wallet adjustments 同键异载荷同口径）。
//   - 大小上限：条目总数 ≤ MaxBulkImportItems（防御性上限；HTTP 层另有
//     全局 1 MiB body 上限）。
//   - 落库即草稿：providers/models/deployments 均以 draft 进入（模型默认
//     不可售），运营补齐授权/价格后走既有 catalog publish 才生效；已存在
//     的自然键跳过不覆盖（与 env 兼容导入同一幂等口径）。

package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// MaxBulkImportItems caps the total item count of one import document
// (providers + models + deployments + routes).
const MaxBulkImportItems = 200

// Bulk item kinds (stable wire tokens).
const (
	BulkKindProvider   = "provider"
	BulkKindModel      = "model"
	BulkKindDeployment = "deployment"
	BulkKindRoute      = "route"
)

// Per-item outcomes.
const (
	BulkItemInserted = "inserted"
	BulkItemSkipped  = "skipped" // 自然键已存在，未触碰（幂等信号）
	BulkItemError    = "error"
	// dry-run 预演状态（不落库）。
	BulkItemWouldInsert = "would_insert"
	BulkItemWouldSkip   = "would_skip"
)

// BulkProvider is one provider entry of the import document.
type BulkProvider struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
	AccessType  string `json:"access_type"` // official_api | oauth_connector | self_hosted
	Status      string `json:"status"`      // 默认 active
}

// BulkDeployment is one deployment entry nested under a model.
type BulkDeployment struct {
	ProviderCode     string `json:"provider_code"`
	UpstreamModel    string `json:"upstream_model"`
	BaseURL          string `json:"base_url"`
	Protocol         string `json:"protocol"`
	Region           string `json:"region"`
	ConnectTimeoutMs int64  `json:"connect_timeout_ms"`
	RequestTimeoutMs int64  `json:"request_timeout_ms"`
}

// BulkModel is one model entry plus its deployments. Routes are derived:
// every imported deployment gets a default route (priority 0, weight 1,
// round_robin) — same shape as the env compatibility import.
type BulkModel struct {
	ID                string           `json:"id"`
	DisplayName       string           `json:"display_name"`
	ModelVersion      string           `json:"model_version"`
	Aliases           []string         `json:"aliases"`
	InputModalities   []string         `json:"input_modalities"`
	OutputModalities  []string         `json:"output_modalities"`
	ContextTokens     int              `json:"context_tokens"`
	MaxOutputTokens   int              `json:"max_output_tokens"`
	Protocols         []string         `json:"protocols"`
	SupportsTools     bool             `json:"supports_tools"`
	SupportsReasoning bool             `json:"supports_reasoning"`
	Deployments       []BulkDeployment `json:"deployments"`
}

// BulkCatalog is the decoded import document.
type BulkCatalog struct {
	Providers []BulkProvider `json:"providers"`
	Models    []BulkModel    `json:"models"`
}

// TotalItems counts every addressable item (providers + models +
// deployments + derived routes).
func (d BulkCatalog) TotalItems() int {
	n := len(d.Providers) + len(d.Models)
	for _, m := range d.Models {
		n += 2 * len(m.Deployments) // deployment + derived route
	}
	return n
}

// BulkItemResult is the per-item outcome — the "逐项错误" surface. NaturalKey
// identifies the item across replays (provider code / model id /
// provider|upstream_model|base_url / model|deployment-key).
type BulkItemResult struct {
	Kind       string `json:"kind"`
	NaturalKey string `json:"natural_key"`
	Status     string `json:"status"`          // inserted|skipped|error|would_insert|would_skip
	ID         string `json:"id,omitempty"`    // 落库行 ID（插入/既有）
	Error      string `json:"error,omitempty"` // 逐项错误
}

// BulkImportResult is the assembled result of one import run (dry-run or
// committed). Committed results are persisted (inference_bulk_imports) and
// replayed verbatim on task_id resubmission — but ONLY when the resubmitted
// document hashes to the same digest the task row recorded (migration 039,
// M-4): same task_id + different document is caller task_id reuse, 409.
type BulkImportResult struct {
	TaskID    string           `json:"task_id"`
	DryRun    bool             `json:"dry_run"`
	Replayed  bool             `json:"replayed"`
	Committed bool             `json:"committed"`
	Items     []BulkItemResult `json:"items"`
	// 计数摘要（不含 error 项的细分见 Items）。
	Inserted int `json:"inserted"`
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
	// DocumentHash is the sha256 digest of the committed document
	// (BulkDocumentHash). Populated on replay reads; compared against the
	// incoming document before a stored result may be replayed. Never
	// serialized — internal integrity metadata only.
	DocumentHash string `json:"-"`
}

// BulkDocumentHash is the canonical digest of an import document, recorded
// on the task row at commit time and re-checked on replay. json.Marshal of
// the struct is deterministic (no maps in the document shape), so the same
// logical document always hashes identically regardless of request wire
// formatting (key order, whitespace). The write path (postgres
// ApplyBulkImportTx) and the replay check both call THIS function — one
// computation, no drift.
func BulkDocumentHash(doc *BulkCatalog) string {
	raw, err := json.Marshal(doc)
	if err != nil {
		// BulkCatalog is marshal-able by construction; a failure here is a
		// programmer error, not caller input. Panic surfaces it in tests.
		panic("bulk import: marshal document for hash: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// HasErrors reports whether any item failed validation/resolution.
func (r *BulkImportResult) HasErrors() bool { return r.Errors > 0 }

// BulkImportStore is the persistence surface; satisfied by
// inference/postgres.Store. Lookups feed dry-run previews;
// ApplyBulkImportTx is the atomic commit (task row + catalog rows in ONE
// transaction).
type BulkImportStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error)
	GetModel(ctx context.Context, id string) (*domain.Model, error)
	FindDeployment(ctx context.Context, providerID, upstreamModel, baseURL string) (*domain.Deployment, error)
	ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error)
	// GetBulkImportByTaskID re-reads a committed task (幂等重放).
	GetBulkImportByTaskID(ctx context.Context, taskID string) (*BulkImportResult, error)
	// ApplyBulkImportTx applies a fully-validated import inside one
	// transaction and records the task row. It re-checks natural keys under
	// the transaction (a racing import is absorbed by ON CONFLICT /
	// unique-violation → skipped), and inserts the task row LAST: a task_id
	// conflict surfaces as CodeConflict so the caller replays the recorded
	// result instead of double-applying.
	ApplyBulkImportTx(ctx context.Context, w domain.UnitOfWork, taskID, actor string, plan *BulkCatalog) (*BulkImportResult, error)
}

// BulkImportService validates and applies catalog bulk imports.
type BulkImportService struct {
	store      BulkImportStore
	recorder   AuditRecorder
	validateURL func(ctx context.Context, rawURL string) error
}

// NewBulkImportService builds the service. validateURL may be nil (egress
// checks disabled — tests only); recorder may be nil (unit tests).
func NewBulkImportService(store BulkImportStore, recorder AuditRecorder, validateURL func(context.Context, string) error) *BulkImportService {
	return &BulkImportService{store: store, recorder: recorder, validateURL: validateURL}
}

// taskIDPattern bounds the operator-supplied idempotency task ID (the DB
// CHECK mirrors it).
var taskIDPattern = "^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$"

// validateTaskID enforces the task-id shape at the boundary.
func validateTaskID(taskID string) error {
	if taskID == "" || len(taskID) > 128 {
		return domain.NewError(domain.CodeInvalidInput, "bulk import: task_id required (1..128 chars)")
	}
	for i, r := range taskID {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == ':' || r == '-'
		if !ok || (i == 0 && (r == '.' || r == '_' || r == ':' || r == '-')) {
			return domain.NewError(domain.CodeInvalidInput,
				"bulk import: task_id must match "+taskIDPattern)
		}
	}
	return nil
}

// validateDocument runs the pure per-item validation (no DB): shape checks
// via the SAME validators as operator CRUD (catalog.Validate*), cross-
// reference resolution inside the document, and the size cap. Every issue
// lands on its own item — validation never fails fast on the first error
// (逐项错误).
func (s *BulkImportService) validateDocument(ctx context.Context, doc *BulkCatalog) []BulkItemResult {
	items := []BulkItemResult{}
	if doc.TotalItems() > MaxBulkImportItems {
		items = append(items, BulkItemResult{
			Kind: "document", NaturalKey: "-", Status: BulkItemError,
			Error: fmt.Sprintf("item count %d exceeds limit %d", doc.TotalItems(), MaxBulkImportItems),
		})
		return items
	}

	providerCodes := map[string]bool{}
	for _, p := range doc.Providers {
		key := p.Code
		dm := &domain.Provider{
			Code: p.Code, DisplayName: p.DisplayName,
			AccessType: domain.AccessType(p.AccessType),
			Status:     p.Status,
		}
		if dm.Status == "" {
			dm.Status = "active"
		}
		item := BulkItemResult{Kind: BulkKindProvider, NaturalKey: key}
		switch {
		case providerCodes[p.Code]:
			item.Status, item.Error = BulkItemError, "duplicate provider code in document"
		default:
			if err := catalog.ValidateProvider(dm); err != nil {
				item.Status, item.Error = BulkItemError, err.Error()
			} else {
				item.Status = BulkItemWouldInsert
				providerCodes[p.Code] = true
			}
		}
		items = append(items, item)
	}

	for _, m := range doc.Models {
		dm := &domain.Model{
			ID: m.ID, DisplayName: m.DisplayName, Lifecycle: domain.LifecycleDraft,
			ModelVersion: m.ModelVersion, Aliases: m.Aliases,
			InputModalities: m.InputModalities, OutputModalities: m.OutputModalities,
			ContextTokens: m.ContextTokens, MaxOutputTokens: m.MaxOutputTokens,
			SupportsTools: m.SupportsTools, SupportsReasoning: m.SupportsReasoning,
		}
		if len(dm.InputModalities) == 0 {
			dm.InputModalities = []string{"text"}
		}
		if len(dm.OutputModalities) == 0 {
			dm.OutputModalities = []string{"text"}
		}
		for _, p := range m.Protocols {
			dm.Protocols = append(dm.Protocols, domain.Protocol(p))
		}
		mItem := BulkItemResult{Kind: BulkKindModel, NaturalKey: m.ID, Status: BulkItemWouldInsert}
		if err := catalog.ValidateModel(dm); err != nil {
			mItem.Status, mItem.Error = BulkItemError, err.Error()
		}
		items = append(items, mItem)

		seen := map[string]bool{}
		for _, d := range m.Deployments {
			key := d.ProviderCode + "|" + d.UpstreamModel + "|" + d.BaseURL
			dd := &domain.Deployment{
				// 解析发生在存在性检查阶段；纯校验只要求语法占位值。
				ProviderID:    "00000000-0000-0000-0000-000000000000",
				UpstreamModel: d.UpstreamModel, BaseURL: d.BaseURL,
				Protocol:       domain.Protocol(d.Protocol),
				Region:         d.Region,
				ConnectTimeout: time.Duration(d.ConnectTimeoutMs) * time.Millisecond,
				RequestTimeout: time.Duration(d.RequestTimeoutMs) * time.Millisecond,
				Status:         domain.DeploymentDraft,
			}
			if dd.ConnectTimeout == 0 {
				dd.ConnectTimeout = 5 * time.Second
			}
			if dd.RequestTimeout == 0 {
				dd.RequestTimeout = 600 * time.Second
			}
			item := BulkItemResult{Kind: BulkKindDeployment, NaturalKey: key, Status: BulkItemWouldInsert}
			switch {
			case seen[key]:
				item.Status, item.Error = BulkItemError, "duplicate deployment in document"
			case !providerCodes[d.ProviderCode]:
				item.Status, item.Error = BulkItemError, "references provider not listed in this document; existing catalog providers are not auto-resolved — list the provider in the document too (it will be reused as skipped, not duplicated)"
			default:
				if err := catalog.ValidateDeployment(dd); err != nil {
					item.Status, item.Error = BulkItemError, err.Error()
				} else if err := catalog.ValidateDeploymentRecoveryWindow(dd); err != nil {
					item.Status, item.Error = BulkItemError, err.Error()
				} else if err := s.validateDeploymentURL(ctx, d.BaseURL); err != nil {
					item.Status, item.Error = BulkItemError, err.Error()
				}
			}
			seen[key] = true
			items = append(items, item)
			items = append(items, BulkItemResult{
				Kind: BulkKindRoute, NaturalKey: m.ID + "|" + key, Status: BulkItemWouldInsert,
			})
		}
	}
	return items
}

// validateDeploymentURL applies the egress policy（与 operator CRUD 同一规
// 则：catalog.ValidateDeployment 先要求绝对 http(s) URL；此处再过 SSRF/
// egress 策略；未配置策略时拒绝——fail closed）。
func (s *BulkImportService) validateDeploymentURL(ctx context.Context, baseURL string) error {
	if s.validateURL == nil {
		return domain.NewError(domain.CodeInvalidInput, "egress validation not configured")
	}
	if err := s.validateURL(ctx, baseURL); err != nil {
		return domain.NewError(domain.CodeInvalidInput, "deployment base_url rejected by egress policy: "+err.Error())
	}
	return nil
}

// Import validates the document and either previews (dry-run) or atomically
// commits it. Commit requires every item valid (绝不半发布) and is idempotent
// on task_id (重复提交不重复创建).
func (s *BulkImportService) Import(ctx context.Context, actor, taskID string, doc *BulkCatalog, dryRun bool) (*BulkImportResult, error) {
	if err := validateTaskID(taskID); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, domain.NewError(domain.CodeInvalidInput, "bulk import: document required")
	}

	items := s.validateDocument(ctx, doc)
	res := &BulkImportResult{TaskID: taskID, DryRun: dryRun, Items: items}
	for _, it := range items {
		if it.Status == BulkItemError {
			res.Errors++
		}
	}
	if res.HasErrors() {
		// dry-run 与 commit 的逐项错误同一形状；commit 不落任何行。
		return res, nil
	}

	if dryRun {
		// 存在性预演：已存在的自然键标记 would_skip（不写库）。
		s.markExisting(ctx, doc, res)
		return res, nil
	}

	// 幂等重放快速路径：任务已提交过则原样返回已记录结果——但仅当
	// 重放文档与任务行记录的摘要用同一函数算出一致（migration 039，M-4）；
	// 同 task_id 异文档是调用方键复用，409，不得静默返回旧任务的结果。
	docHash := BulkDocumentHash(doc)
	if stored, err := s.store.GetBulkImportByTaskID(ctx, taskID); err == nil {
		if err := checkReplayDocument(taskID, stored, docHash); err != nil {
			return nil, err
		}
		stored.Replayed = true
		return stored, nil
	} else if domain.CodeOf(err) != domain.CodeNotFound {
		return nil, err
	}

	uow, err := s.store.Begin(ctx)
	if err != nil {
		return nil, err
	}
	committed, err := s.store.ApplyBulkImportTx(ctx, uow, taskID, actor, doc)
	if err != nil {
		_ = uow.Rollback(ctx)
		if domain.CodeOf(err) == domain.CodeConflict {
			// 并发撞 task_id：赢家已提交（或即将），重读已记录结果——
			// 与快速路径同一摘要校验（并发重放不同文档同样是键复用）。
			stored, rerr := s.store.GetBulkImportByTaskID(ctx, taskID)
			if rerr != nil {
				return nil, domain.WrapError(domain.CodeConflict,
					"bulk import: task_id committed concurrently; re-read failed", rerr)
			}
			if err := checkReplayDocument(taskID, stored, docHash); err != nil {
				return nil, err
			}
			stored.Replayed = true
			return stored, nil
		}
		return nil, err
	}
	if s.recorder != nil {
		// 同事务审计（与 wallet.adjust 齐平）：审计行与导入效果同生共死；
		// 记录器不支持事务写入时整个导入失败（fail-closed，不落下无审计
		// 的目录变更）。
		txRec, ok := s.recorder.(AuditTxRecorder)
		if !ok {
			_ = uow.Rollback(ctx)
			return nil, domain.NewError(domain.CodeInternal, "bulk import: audit recorder lacks transactional support")
		}
		userID, appID := ParseActor(actor)
		if err := txRec.RecordTx(ctx, uow, AuditEvent{
			Action: "catalog.bulk_import", ObjectType: "bulk_import", ObjectID: taskID,
			ActorUser: userID, ActorApp: appID,
			Detail: SanitizeDetail(map[string]any{
				"inserted": committed.Inserted, "skipped": committed.Skipped,
				"item_count": len(committed.Items),
			}),
		}); err != nil {
			_ = uow.Rollback(ctx)
			return nil, domain.WrapError(domain.CodeInternal, "bulk import audit write failed", err)
		}
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	committed.Committed = true
	return committed, nil
}

// checkReplayDocument gates replay of a committed task: the stored digest
// must match the incoming document's digest. Empty stored digest means a
// pre-039 legacy row — no digest was recorded, replay as before (mirrors the
// admin idempotency request_hash NULL rule). A recorded digest that differs
// is caller task_id reuse, not a benign retry → 409 (aligned with the
// wallet-adjustments / VIP payload-mismatch surfaces).
func checkReplayDocument(taskID string, stored *BulkImportResult, docHash string) error {
	if stored.DocumentHash == "" || stored.DocumentHash == docHash {
		return nil
	}
	return domain.NewError(domain.CodeConflict,
		"bulk import: task_id "+taskID+" already committed with a different document")
}

// markExisting annotates dry-run items with the would_skip status by probing
// natural keys read-only. A probe error leaves the item at would_insert —
// dry-run is a preview, not a promise; the commit path re-checks under the
// transaction.
func (s *BulkImportService) markExisting(ctx context.Context, doc *BulkCatalog, res *BulkImportResult) {
	providerIDs := map[string]string{}
	for _, p := range doc.Providers {
		if existing, err := s.store.GetProviderByCode(ctx, p.Code); err == nil {
			providerIDs[p.Code] = existing.ID
			res.mark(BulkKindProvider, p.Code, BulkItemWouldSkip, existing.ID)
		}
	}
	for _, m := range doc.Models {
		if _, err := s.store.GetModel(ctx, m.ID); err == nil {
			res.mark(BulkKindModel, m.ID, BulkItemWouldSkip, m.ID)
		}
		routeHave := map[string]bool{}
		if routes, err := s.store.ListRoutes(ctx, m.ID); err == nil {
			for _, r := range routes {
				routeHave[r.DeploymentID] = true
			}
		}
		for _, d := range m.Deployments {
			key := d.ProviderCode + "|" + d.UpstreamModel + "|" + d.BaseURL
			pid := providerIDs[d.ProviderCode]
			if pid == "" {
				continue // dry-run 不知道新 provider 的 ID；commit 时解析
			}
			if existing, err := s.store.FindDeployment(ctx, pid, d.UpstreamModel, d.BaseURL); err == nil {
				res.mark(BulkKindDeployment, key, BulkItemWouldSkip, existing.ID)
				if routeHave[existing.ID] {
					res.mark(BulkKindRoute, m.ID+"|"+key, BulkItemWouldSkip, "")
				}
			}
		}
	}
	for _, it := range res.Items {
		switch it.Status {
		case BulkItemWouldSkip:
			res.Skipped++
		case BulkItemWouldInsert:
			res.Inserted++
		}
	}
}

// mark updates one item in place (keyed by kind + natural key, first match
// not yet finalised).
func (r *BulkImportResult) mark(kind, naturalKey, status, id string) {
	for i := range r.Items {
		if r.Items[i].Kind == kind && r.Items[i].NaturalKey == naturalKey &&
			(r.Items[i].Status == BulkItemWouldInsert || r.Items[i].Status == BulkItemWouldSkip) {
			r.Items[i].Status = status
			r.Items[i].ID = id
			return
		}
	}
}
