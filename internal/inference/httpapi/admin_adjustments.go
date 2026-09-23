// admin_adjustments.go — 运营钱包调整与 PAYG 发布配置端点（Task 14 创建；
// Task 15 只扩展不重建）。全部挂在 billing:adjust 权限下（router 装配）。
//
//   POST /admin/wallet/adjustments  运营调整（赠送 bonus 发放 /
//      现金补偿 / 扣回）：inference_adjustments + 钱包分录同事务，幂等键
//      防重复补偿投递；人员及服务双重归因（设计 §9.2）。
//   POST /admin/wallet/reversals    冲正一条钱包分录（追加冲正，
//      不重写已入账事实）。
//   GET  /admin/wallet              派生余额 + 近期分录（只读）。
//   PUT  /admin/payg-config         发布 PAYG 默认策略/模型集合。
//   GET  /admin/payg-config         读取 PAYG 发布配置。
//
// 金额一律 DecimalInt64（十进制整数字符串）+ ISO-4217 币种；赠送来源
// (bonus) 永不走现金退款路径。

package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
	"github.com/yunhou/users/internal/inference/postgres"
)

// AdminWalletStore is the persistence surface the operator wallet endpoints
// need; satisfied by inference/postgres.Store.
type AdminWalletStore interface {
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error)
	// EnsureBillingAccount idempotently establishes the personal account —
	// the adjustment surface must reach accounts that never purchased.
	EnsureBillingAccount(ctx context.Context, userID string) (*domain.BillingAccount, error)
	GetBillingAccountByID(ctx context.Context, id string) (*domain.BillingAccount, error)
	WalletBalance(ctx context.Context, accountID, currency string, at time.Time) (*postgres.WalletBalanceView, error)
	ListWalletEntries(ctx context.Context, accountID, currency string, afterID int64, limit int) ([]postgres.WalletEntry, error)
	ApplyWalletAdjustmentTx(ctx context.Context, w domain.UnitOfWork, cmd postgres.WalletAdjustmentCommand) (*postgres.Adjustment, error)
	GetAdjustmentByIdempotencyKey(ctx context.Context, key string) (*postgres.Adjustment, error)
	ReverseWalletEntryTx(ctx context.Context, w domain.UnitOfWork, accountID string, entryID int64, operatorSubject string) error
	GetPAYGConfig(ctx context.Context) (*postgres.PAYGConfig, error)
	PutPAYGConfig(ctx context.Context, policyVersionID string, modelIDs []string, updatedBy string) (*postgres.PAYGConfig, error)
}

// adjustmentAuditor combines post-hoc and transactional audit recording
// (the postgres store implements both; unit tests may pass nil).
type adjustmentAuditor interface {
	management.AuditRecorder
	management.AuditTxRecorder
}

// AdminAdjustmentsHandler exposes the operator wallet surface.
type AdminAdjustmentsHandler struct {
	store AdminWalletStore
	clock domain.Clock
	// audit 是补偿/冲正/PAYG 发布的追加审计通道（调整与冲正同事务落库—
	// 审计与效果同生共死）；nil 仅存在于不演练审计的单测。
	audit adjustmentAuditor
	// ops 提供补偿列表的只读视图（nil = 列表端点响亮报错而非静默空表）。
	ops *management.OperationsService
}

// NewAdminAdjustmentsHandler builds the handler; a nil clock uses UTC.
// audit and ops may be nil (unit tests only); production wires the store.
func NewAdminAdjustmentsHandler(store AdminWalletStore, clock domain.Clock, audit adjustmentAuditor, ops *management.OperationsService) *AdminAdjustmentsHandler {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &AdminAdjustmentsHandler{store: store, clock: clock, audit: audit, ops: ops}
}

// Register mounts the endpoints; the caller wraps the group with the
// billing:adjust authorization middleware.
func (h *AdminAdjustmentsHandler) Register(g *gin.RouterGroup) {
	g.POST("/wallet/adjustments", h.Adjust)
	g.POST("/wallet/reversals", h.Reverse)
	g.GET("/wallet/adjustments", h.ListAdjustments)
	g.GET("/wallet", h.Get)
	g.GET("/payg-config", h.GetPAYG)
	g.PUT("/payg-config", h.PutPAYG)
}

// auditWalletTx appends one audit event inside the mutation's transaction —
// 补偿/冲正的追加审计与效果同生共死（同事务提交或回滚）。nil 记录器仅存
// 在于不演练审计的单测。
func (h *AdminAdjustmentsHandler) auditWalletTx(ctx context.Context, uow domain.UnitOfWork, op credentialsOperator, action, objectID, reason string, detail map[string]any) error {
	if h.audit == nil {
		return nil
	}
	return h.audit.RecordTx(ctx, uow, management.AuditEvent{
		Action: action, ObjectType: "wallet", ObjectID: objectID, Reason: reason,
		ActorUser: op.UserID, ActorApp: op.AppID, Detail: management.SanitizeDetail(detail),
	})
}

// credentialsOperator is the attribute subset of credentials.Operator this
// handler needs (avoid re-deriving it from the gin context deep in helpers).
type credentialsOperator struct{ UserID, AppID string }

// adjIDOf renders the adjustment id for audit detail (nil-safe).
func adjIDOf(adj *postgres.Adjustment) string {
	if adj == nil {
		return ""
	}
	return adj.ID
}

// resolveAccount pins the billing account: billing_account_id directly, or
// user_id resolved to the personal account (设计 §4.2: 首期每用户一个个人
// billing_account). Exactly one selector is required.
func (h *AdminAdjustmentsHandler) resolveAccount(ctx context.Context, accountID, userID string) (*domain.BillingAccount, error) {
	switch {
	case accountID != "" && userID == "":
		return h.store.GetBillingAccountByID(ctx, accountID)
	case userID != "" && accountID == "":
		// 运营面按用户调整时幂等建立账户（客户可能尚未购买过）。
		return h.store.EnsureBillingAccount(ctx, userID)
	default:
		return nil, domain.NewError(domain.CodeInvalidInput,
			"exactly one of billing_account_id / user_id is required")
	}
}

type walletAdjustmentRequest struct {
	BillingAccountID string `json:"billing_account_id"`
	UserID           string `json:"user_id"`
	Currency         string `json:"currency" binding:"required"`
	Source           string `json:"source" binding:"required"`    // cash | bonus
	Direction        string `json:"direction" binding:"required"` // credit | debit
	AmountMicros     string `json:"amount_micros" binding:"required"`
	Reason           string `json:"reason" binding:"required"`
	IdempotencyKey   string `json:"idempotency_key" binding:"required"`
}

// Adjust handles POST /wallet/adjustments. applied=false marks an
// idempotency-key replay — the response says so explicitly rather than
// double-granting silently.
func (h *AdminAdjustmentsHandler) Adjust(c *gin.Context) {
	var req walletAdjustmentRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	amount, err := strconv.ParseInt(req.AmountMicros, 10, 64)
	if err != nil || amount <= 0 {
		fail(c, domain.NewError(domain.CodeInvalidInput, "amount_micros must be a positive decimal integer string"))
		return
	}
	op := OperatorOf(c)
	ctx := c.Request.Context()
	acct, err := h.resolveAccount(ctx, req.BillingAccountID, req.UserID)
	if err != nil {
		fail(c, err)
		return
	}
	uow, err := h.store.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	adj, err := h.store.ApplyWalletAdjustmentTx(ctx, uow, postgres.WalletAdjustmentCommand{
		AccountID:       acct.ID,
		Currency:        req.Currency,
		Source:          accounting.WalletSource(req.Source),
		Direction:       accounting.WalletDirection(req.Direction),
		AmountMicros:    amount,
		Reason:          req.Reason,
		OperatorSubject: "user:" + op.UserID + "@app:" + op.AppID,
		ServiceSubject:  "httpapi/admin_adjustments",
		IdempotencyKey:  req.IdempotencyKey,
	})
	if err != nil {
		_ = uow.Rollback(ctx)
		if domain.CodeOf(err) == domain.CodeConflict {
			// 幂等键重放：首次效果已在；重读存储行作为响应（只生效一次）。
			stored, rerr := h.store.GetAdjustmentByIdempotencyKey(ctx, req.IdempotencyKey)
			if rerr != nil {
				fail(c, rerr)
				return
			}
			c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
				"applied": false, "adjustment": adjustmentJSON(stored),
			}})
			return
		}
		fail(c, err)
		return
	}
	// 追加审计（同事务）：补偿可定位到人员（Task 15；设计 §9.2）。
	if err := h.auditWalletTx(ctx, uow, credentialsOperator{op.UserID, op.AppID},
		"wallet.adjust", acct.ID, req.Reason, map[string]any{
			"adjustment_id":   adjIDOf(adj),
			"direction":       req.Direction,
			"source":          req.Source,
			"amount_micros":   req.AmountMicros,
			"currency":        req.Currency,
			"idempotency_key": req.IdempotencyKey,
		}); err != nil {
		_ = uow.Rollback(ctx)
		fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
		return
	}
	data := gin.H{"applied": true}
	if adj != nil {
		data["adjustment"] = adjustmentJSON(adj)
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": data})
}

// adjustmentJSON renders the stored adjustment row.
func adjustmentJSON(adj *postgres.Adjustment) gin.H {
	return gin.H{
		"id":            adj.ID,
		"amount_micros": strconv.FormatInt(adj.AmountMicros, 10),
		"direction":     adj.Direction,
		"unit":          adj.Unit,
		"currency":      adj.Currency,
		"reason":        adj.Reason,
		"created_at":    adj.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// adjustmentViewsJSON renders the compensation listing (可定位到人员：
// operator_subject / service_subject / idempotency_key 全量呈现).
func adjustmentViewsJSON(views []management.AdjustmentView) []gin.H {
	out := make([]gin.H, 0, len(views))
	for _, v := range views {
		out = append(out, gin.H{
			"id": v.ID, "billing_account_id": v.BillingAccountID,
			"request_id": v.RequestID, "reason": v.Reason,
			"amount_micros": strconv.FormatInt(v.AmountMicros, 10),
			"direction": v.Direction, "unit": v.Unit, "currency": v.Currency,
			"operator_subject": v.OperatorSubject, "service_subject": v.ServiceSubject,
			"idempotency_key": v.IdempotencyKey,
			"created_at":      v.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

type walletReversalRequest struct {
	BillingAccountID string `json:"billing_account_id"`
	UserID           string `json:"user_id"`
	EntryID          string `json:"entry_id" binding:"required"`
}

// Reverse handles POST /wallet/reversals — appends the reversal of one
// wallet entry (一条分录至多一条冲正).
func (h *AdminAdjustmentsHandler) Reverse(c *gin.Context) {
	var req walletReversalRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	entryID, err := strconv.ParseInt(req.EntryID, 10, 64)
	if err != nil || entryID <= 0 {
		fail(c, domain.NewError(domain.CodeInvalidInput, "entry_id must be a positive decimal integer string"))
		return
	}
	op := OperatorOf(c)
	ctx := c.Request.Context()
	acct, err := h.resolveAccount(ctx, req.BillingAccountID, req.UserID)
	if err != nil {
		fail(c, err)
		return
	}
	uow, err := h.store.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	err = h.store.ReverseWalletEntryTx(ctx, uow, acct.ID, entryID, "user:"+op.UserID+"@app:"+op.AppID)
	if err != nil {
		_ = uow.Rollback(ctx)
		if domain.CodeOf(err) == domain.CodeConflict {
			ok(c, gin.H{"reversed": false}) // 已冲正过：幂等重放
			return
		}
		fail(c, err)
		return
	}
	// 追加审计（同事务）：冲正可定位到人员（Task 15）。
	if err := h.auditWalletTx(ctx, uow, credentialsOperator{op.UserID, op.AppID},
		"wallet.reverse", acct.ID, "operator reversal", map[string]any{
			"entry_id": req.EntryID,
		}); err != nil {
		_ = uow.Rollback(ctx)
		fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"reversed": true})
}

// Get handles GET /wallet?billing_account_id=|user_id=&currency= — the
// derived balance plus the recent statement (运营只读面).
func (h *AdminAdjustmentsHandler) Get(c *gin.Context) {
	ctx := c.Request.Context()
	acct, err := h.resolveAccount(ctx, c.Query("billing_account_id"), c.Query("user_id"))
	if err != nil {
		fail(c, err)
		return
	}
	currency := c.Query("currency")
	if _, err := domain.NewMoney(0, currency); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "currency is required (ISO-4217 uppercase)"))
		return
	}
	view, err := h.store.WalletBalance(ctx, acct.ID, currency, h.clock.Now())
	if err != nil {
		fail(c, err)
		return
	}
	entries, err := h.store.ListWalletEntries(ctx, acct.ID, currency, 0, parseLimit(c, 20))
	if err != nil {
		fail(c, err)
		return
	}
	entryIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		entryIDs = append(entryIDs, strconv.FormatInt(e.ID, 10)+":"+e.EntryType+":"+e.Direction+":"+e.Source+":"+strconv.FormatInt(e.AmountMicros, 10))
	}
	ok(c, gin.H{"wallet": toWalletJSON(*view), "recent_entries": entryIDs})
}

// GetPAYG handles GET /payg-config.
func (h *AdminAdjustmentsHandler) GetPAYG(c *gin.Context) {
	cfg, err := h.store.GetPAYGConfig(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{
		"policy_version_id": cfg.PolicyVersionID,
		"model_ids":         cfg.ModelIDs,
		"updated_by":        cfg.UpdatedBy,
		"updated_at":        cfg.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

type paygConfigRequest struct {
	PolicyVersionID string   `json:"policy_version_id" binding:"required"`
	ModelIDs        []string `json:"model_ids"`
}

// PutPAYG handles PUT /payg-config — publishes the PAYG default. The policy
// version must exist and be published (没有配置的商品不可购买，设计 §4.3).
func (h *AdminAdjustmentsHandler) PutPAYG(c *gin.Context) {
	var req paygConfigRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	op := OperatorOf(c)
	cfg, err := h.store.PutPAYGConfig(c.Request.Context(), req.PolicyVersionID, req.ModelIDs,
		"user:"+op.UserID+"@app:"+op.AppID)
	if err != nil {
		fail(c, err)
		return
	}
	// 追加审计（发布配置变更；Task 15）。PutPAYGConfig 是单行 upsert，审计
	// 紧随其后落库；审计失败对运营响亮报错（配置已生效，审计面可查
	// wallet_audits 的开关留痕与此条互补）。
	if h.audit != nil {
		if err := h.audit.Record(c.Request.Context(), management.AuditEvent{
			Action: "payg_config.publish", ObjectType: "payg_config", ObjectID: req.PolicyVersionID,
			ActorUser: op.UserID, ActorApp: op.AppID,
			Detail: management.SanitizeDetail(map[string]any{"model_ids": req.ModelIDs}),
		}); err != nil {
			fail(c, domain.WrapError(domain.CodeInternal, "audit write failed", err))
			return
		}
	}
	ok(c, gin.H{
		"policy_version_id": cfg.PolicyVersionID,
		"model_ids":         cfg.ModelIDs,
		"updated_by":        cfg.UpdatedBy,
		"updated_at":        cfg.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

// ListAdjustments handles GET /wallet/adjustments?billing_account_id=&limit=
// — 补偿追踪（有原因、对象、金额、操作者与幂等键，设计 §9.2 /admin/
// model-adjustments 族）。usage:read 的 /admin/model-adjustments 别名在
// admin_usage.go 挂载（同一服务，同一口径）。
func (h *AdminAdjustmentsHandler) ListAdjustments(c *gin.Context) {
	if h.ops == nil {
		fail(c, domain.NewError(domain.CodeInternal, "adjustments listing not wired"))
		return
	}
	views, err := h.ops.Adjustments(c.Request.Context(), c.Query("billing_account_id"), parseLimit(c, 100))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"adjustments": adjustmentViewsJSON(views)})
}
