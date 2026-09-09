// user_wallet.go — 客户钱包端点（Task 14；设计 §7.3 钱包章节）。
//
//   GET  /user/wallet          余额总览（账本派生，无缓存余额；按币种隔离）
//   GET  /user/wallet/entries  分录流水（keyset 分页）
//   PUT  /user/wallet/overage  显式开启/关闭套餐外消费 + UTC 自然月支出上限
//                              （默认关；修改写 inference_wallet_audits）
//   POST /user/wallet/payg     无套餐按量：按运营发布配置创建显式 PAYG 权益
//
// 归属一律来自 JWT 身份（userIDOf），请求体不得伪造账户；金额字段遵循
// DecimalInt64 约定（十进制整数字符串）。

package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/accounting"
	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/postgres"
)

// UserWalletStore is the persistence surface the customer wallet endpoints
// need; satisfied by inference/postgres.Store.
type UserWalletStore interface {
	GetBillingAccountByUser(ctx context.Context, userID string) (*domain.BillingAccount, error)
	EnsureBillingAccount(ctx context.Context, userID string) (*domain.BillingAccount, error)
	Begin(ctx context.Context) (domain.UnitOfWork, error)
	ListWalletBalances(ctx context.Context, accountID string, at time.Time) ([]postgres.WalletBalanceView, error)
	WalletBalance(ctx context.Context, accountID, currency string, at time.Time) (*postgres.WalletBalanceView, error)
	ListWalletEntries(ctx context.Context, accountID, currency string, afterID int64, limit int) ([]postgres.WalletEntry, error)
	SetOverageTx(ctx context.Context, w domain.UnitOfWork, accountID, currency string, enabled bool, limit *int64, changedBy string) (*postgres.Wallet, error)
	EnsurePAYGEntitlementTx(ctx context.Context, w domain.UnitOfWork, accountID string, at time.Time) (*domain.Entitlement, error)
	GetLatestEntitlementBySource(ctx context.Context, sourceType domain.EntitlementSource, sourceID string) (*domain.Entitlement, error)
}

// UserWalletHandler serves the customer wallet surface.
type UserWalletHandler struct {
	store UserWalletStore
	clock domain.Clock
}

// NewUserWalletHandler builds the handler; a nil clock uses the system
// clock (UTC).
func NewUserWalletHandler(store UserWalletStore, clock domain.Clock) *UserWalletHandler {
	if clock == nil {
		clock = domain.SystemClock{}
	}
	return &UserWalletHandler{store: store, clock: clock}
}

// Register mounts the endpoints on a JWT-authenticated group.
func (h *UserWalletHandler) Register(g *gin.RouterGroup) {
	g.GET("/wallet", h.Get)
	g.GET("/wallet/entries", h.Entries)
	g.PUT("/wallet/overage", h.SetOverage)
	g.POST("/wallet/payg", h.EnablePAYG)
}

// walletJSON is one currency's derived wallet view. All amounts are
// DecimalInt64 strings (十进制整数字符串，无浮点).
type walletJSON struct {
	Currency                string  `json:"currency"`
	Unit                    string  `json:"unit"` // micromoney
	CashAvailableMicros     string  `json:"cash_available_micros"`
	BonusAvailableMicros    string  `json:"bonus_available_micros"`
	CashHeldMicros          string  `json:"cash_held_micros"`
	BonusHeldMicros         string  `json:"bonus_held_micros"`
	TotalAvailableMicros    string  `json:"total_available_micros"`
	OverageEnabled          bool    `json:"overage_enabled"`
	MonthlySpendLimitMicros *string `json:"monthly_spend_limit_micros"`
	MonthSpentMicros        string  `json:"month_spent_micros"`
	MonthStart              string  `json:"month_start"`
	MonthEnd                string  `json:"month_end"`
}

func toWalletJSON(v postgres.WalletBalanceView) walletJSON {
	out := walletJSON{
		Currency:             v.Wallet.Currency,
		Unit:                 "micromoney",
		CashAvailableMicros:  strconv.FormatInt(v.Balance.CashAvailable, 10),
		BonusAvailableMicros: strconv.FormatInt(v.Balance.BonusAvailable, 10),
		CashHeldMicros:       strconv.FormatInt(v.Balance.CashHeld, 10),
		BonusHeldMicros:      strconv.FormatInt(v.Balance.BonusHeld, 10),
		// 总额可能为负（现金退款落在已消费资金上）——如实呈现。
		TotalAvailableMicros: strconv.FormatInt(v.Balance.CashAvailable+v.Balance.BonusAvailable, 10),
		OverageEnabled:       v.Wallet.OverageEnabled,
		MonthSpentMicros:     strconv.FormatInt(v.MonthSpentMicros, 10),
		MonthStart:           v.MonthStart.UTC().Format(time.RFC3339),
		MonthEnd:             v.MonthEnd.UTC().Format(time.RFC3339),
	}
	if v.Wallet.MonthlySpendLimitMicros != nil {
		s := strconv.FormatInt(*v.Wallet.MonthlySpendLimitMicros, 10)
		out.MonthlySpendLimitMicros = &s
	}
	return out
}

// paygJSON is the account's pay-as-you-go status.
type paygJSON struct {
	Enabled         bool     `json:"enabled"`
	EntitlementID   string   `json:"entitlement_id,omitempty"`
	ModelIDs        []string `json:"model_ids,omitempty"`
	PolicyVersionID string   `json:"policy_version_id,omitempty"`
}

// Get handles GET /user/wallet. A missing billing account renders the zero
// state (no wallets, PAYG off) — reads never create rows.
func (h *UserWalletHandler) Get(c *gin.Context) {
	ctx := c.Request.Context()
	now := h.clock.Now()
	resp := gin.H{
		"server_time": now.UTC().Format(time.RFC3339),
		"wallets":     []walletJSON{},
		"payg":        paygJSON{Enabled: false},
	}
	acct, err := h.store.GetBillingAccountByUser(ctx, userIDOf(c))
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			ok(c, resp)
			return
		}
		fail(c, err)
		return
	}
	views, err := h.store.ListWalletBalances(ctx, acct.ID, now)
	if err != nil {
		fail(c, err)
		return
	}
	wallets := make([]walletJSON, 0, len(views))
	for _, v := range views {
		wallets = append(wallets, toWalletJSON(v))
	}
	resp["wallets"] = wallets
	if ent, err := h.store.GetLatestEntitlementBySource(ctx, domain.SourcePAYG,
		accounting.PAYGEntitlementSourceID(acct.ID)); err == nil && ent.Status == domain.EntitlementActive {
		resp["payg"] = paygJSON{
			Enabled: true, EntitlementID: ent.ID,
			ModelIDs: ent.ModelIDs, PolicyVersionID: ent.PolicyVersionID,
		}
	}
	ok(c, resp)
}

// Entries handles GET /user/wallet/entries?currency=&cursor=&limit= —
// the append-only statement (追加+冲正历史逐字呈现).
func (h *UserWalletHandler) Entries(c *gin.Context) {
	ctx := c.Request.Context()
	currency := c.Query("currency")
	if _, err := domain.NewMoney(0, currency); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "currency is required (ISO-4217 uppercase)"))
		return
	}
	acct, err := h.store.GetBillingAccountByUser(ctx, userIDOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	var afterID int64
	if raw := c.Query("cursor"); raw != "" {
		if afterID, err = strconv.ParseInt(raw, 10, 64); err != nil || afterID < 0 {
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid cursor"))
			return
		}
	}
	limit := parseLimit(c, 50)
	if limit > 100 {
		limit = 100
	}
	entries, err := h.store.ListWalletEntries(ctx, acct.ID, currency, afterID, limit)
	if err != nil {
		fail(c, err)
		return
	}
	type entryJSON struct {
		ID              string  `json:"id"`
		EntryType       string  `json:"entry_type"`
		Direction       string  `json:"direction"`
		Source          string  `json:"source"`
		AmountMicros    string  `json:"amount_micros"`
		Currency        string  `json:"currency"`
		RequestID       *string `json:"request_id"`
		PaymentID       *string `json:"payment_id"`
		RefundID        *string `json:"refund_id"`
		ReversesEntryID *string `json:"reverses_entry_id"`
		CreatedBy       string  `json:"created_by"`
		CreatedAt       string  `json:"created_at"`
	}
	out := make([]entryJSON, 0, len(entries))
	var next *string
	for _, e := range entries {
		j := entryJSON{
			ID:        strconv.FormatInt(e.ID, 10),
			EntryType: e.EntryType, Direction: e.Direction, Source: e.Source,
			AmountMicros: strconv.FormatInt(e.AmountMicros, 10), Currency: e.Currency,
			RequestID: e.RequestID, PaymentID: e.PaymentID, RefundID: e.RefundID,
			CreatedBy: e.CreatedBy,
			CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		}
		if e.ReversesEntryID != nil {
			s := strconv.FormatInt(*e.ReversesEntryID, 10)
			j.ReversesEntryID = &s
		}
		out = append(out, j)
	}
	if len(entries) == limit {
		s := strconv.FormatInt(entries[len(entries)-1].ID, 10)
		next = &s
	}
	ok(c, gin.H{"entries": out, "next_cursor": next})
}

type setOverageRequest struct {
	Currency string `json:"currency" binding:"required"`
	Enabled  bool   `json:"enabled"`
	// 十进制整数字符串（DecimalInt64）；开启时必填。
	MonthlySpendLimitMicros *string `json:"monthly_spend_limit_micros"`
}

// SetOverage handles PUT /user/wallet/overage — the explicit opt-in/out
// and the monthly spend limit (裁决 4：默认关，修改审计).
func (h *UserWalletHandler) SetOverage(c *gin.Context) {
	var req setOverageRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	if _, err := domain.NewMoney(0, req.Currency); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid currency"))
		return
	}
	var limit *int64
	if req.MonthlySpendLimitMicros != nil {
		v, err := strconv.ParseInt(*req.MonthlySpendLimitMicros, 10, 64)
		if err != nil || v < 0 {
			fail(c, domain.NewError(domain.CodeInvalidInput,
				"monthly_spend_limit_micros must be a non-negative decimal integer string"))
			return
		}
		limit = &v
	}
	userID := userIDOf(c)
	ctx := c.Request.Context()
	acct, err := h.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		fail(c, err)
		return
	}
	uow, err := h.store.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	if _, err := h.store.SetOverageTx(ctx, uow, acct.ID, req.Currency, req.Enabled, limit, "user:"+userID); err != nil {
		_ = uow.Rollback(ctx)
		fail(c, err)
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
		return
	}
	view, err := h.store.WalletBalance(ctx, acct.ID, req.Currency, h.clock.Now())
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toWalletJSON(*view))
}

// EnablePAYG handles POST /user/wallet/payg — creates the explicit
// pay-as-you-go entitlement from the operator-published config (裁决 6:
// 必须存在显式 PAYG 权益记录；未发布配置 = 不可开启). Idempotent.
func (h *UserWalletHandler) EnablePAYG(c *gin.Context) {
	userID := userIDOf(c)
	ctx := c.Request.Context()
	acct, err := h.store.EnsureBillingAccount(ctx, userID)
	if err != nil {
		fail(c, err)
		return
	}
	uow, err := h.store.Begin(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	ent, err := h.store.EnsurePAYGEntitlementTx(ctx, uow, acct.ID, h.clock.Now())
	if err != nil {
		_ = uow.Rollback(ctx)
		fail(c, err)
		return
	}
	if err := uow.Commit(ctx); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": paygJSON{
		Enabled: true, EntitlementID: ent.ID,
		ModelIDs: ent.ModelIDs, PolicyVersionID: ent.PolicyVersionID,
	}})
}
