package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/quota"
)

// quota_repo.go — 表组 inference_quota_windows/reservations (migration 026)
// + domain.QuotaStore 的 Reserve/窗口激活部分（Release 见
// reservation_repo.go，并发租约见 lease_repo.go）。
//
// 事务契约（任务书）：Reserve/Release 与 SettlementStore.Settle 共享同一
// 个 UnitOfWork（同一个 *sqlx.Tx）。三窗口与 Key 预算在一次 Reserve 内
// 全部成功才提交；任何一步失败整个事务回滚，不遗留部分占用。
//
// 固定锁序（Task 7 控制者裁决，全部写路径一致，防死锁环）：
// 账户（FOR UPDATE）→ 请求行 → 窗口（five_hour→weekly→monthly）→ Key 预算
// → 预占/租约行。检查并创建请求/预占后提交；网络调用在提交之后（不持有
// DB 事务等待上游，设计 §7.2）。

// InsertQuotaWindow creates one window row. The EXCLUDE constraint forbids
// overlapping active windows per (entitlement, kind) — CodeConflict.
func insertQuotaWindow(ctx context.Context, ex sqlxExecutor, w *domain.QuotaWindow) error {
	if w.ID == "" {
		w.ID = uuid.NewString()
	}
	state := "active"
	if w.Voided {
		state = "voided"
	}
	err := ex.QueryRowxContext(ctx,
		`INSERT INTO inference_quota_windows
		 (id, entitlement_id, kind, window_start, window_end,
		  limit_micros, used_micros, reserved_micros, state)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 RETURNING created_at`,
		w.ID, w.EntitlementID, string(w.Kind), w.Start, w.End,
		int64(w.Limit), int64(w.Used), int64(w.Reserved), state).
		Scan(&w.CreatedAt)
	return mapError("insert quota window", err)
}

// InsertQuotaWindow inserts outside an open UnitOfWork.
func (s *Store) InsertQuotaWindow(ctx context.Context, w *domain.QuotaWindow) error {
	return insertQuotaWindow(ctx, s.db, w)
}

// LoadWindows implements domain.QuotaStore: read-only, never activates a
// five-hour window (设计 §6).
func (s *Store) LoadWindows(ctx context.Context, entitlementID string, kinds []domain.WindowKind) ([]domain.QuotaWindow, error) {
	if len(kinds) == 0 {
		kinds = domain.WindowOrder
	}
	ks := make([]string, 0, len(kinds))
	for _, k := range kinds {
		ks = append(ks, string(k))
	}
	var rows []windowRow
	err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_quota_windows
		 WHERE entitlement_id = $1 AND state = 'active' AND kind = ANY($2)
		 ORDER BY kind`, entitlementID, pq.Array(ks))
	if err != nil {
		return nil, mapError("load windows", err)
	}
	out := make([]domain.QuotaWindow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

type windowRow struct {
	ID        string    `db:"id"`
	EntitleID string    `db:"entitlement_id"`
	Kind      string    `db:"kind"`
	Start     time.Time `db:"window_start"`
	End       time.Time `db:"window_end"`
	Limit     int64     `db:"limit_micros"`
	Used      int64     `db:"used_micros"`
	Reserved  int64     `db:"reserved_micros"`
	State     string    `db:"state"`
	CreatedAt time.Time `db:"created_at"`
}

func (r windowRow) toDomain() domain.QuotaWindow {
	return domain.QuotaWindow{
		ID: r.ID, EntitlementID: r.EntitleID, Kind: domain.WindowKind(r.Kind),
		Start: r.Start, End: r.End,
		Limit: domain.Microcredit(r.Limit), Used: domain.Microcredit(r.Used),
		Reserved: domain.Microcredit(r.Reserved),
		Voided:   r.State == "voided", CreatedAt: r.CreatedAt,
	}
}

// windowOrderKey ranks holds into the fixed lock order (控制者裁决/Task 7:
// 一个事务内按固定顺序锁定 账户 → 窗口(five_hour→weekly→monthly) → Key 预算).
// The account row is locked first by lockAccountTx; holds then apply in
// this order. Release/Settle walk their per-target updates in the SAME
// order so concurrent transactions can never deadlock in a cycle.
func windowOrderKey(k domain.ReservationTargetKind) int {
	switch k {
	case domain.TargetWindowFiveHour:
		return 0
	case domain.TargetWindowWeekly:
		return 1
	case domain.TargetWindowMonthly:
		return 2
	case domain.TargetKeyBudget:
		return 3
	}
	return 9
}

// lockAccountTx takes the billing-account anchor lock (fixed lock order
// step 1) and verifies the account is active. Same-account admissions and
// releases serialize on this row, which is what makes concurrent
// first-consumption window activation safe (Task 7).
func lockAccountTx(ctx context.Context, tx *sqlx.Tx, accountID string) error {
	var status string
	if err := tx.QueryRowxContext(ctx,
		`SELECT status FROM inference_billing_accounts WHERE id = $1 FOR UPDATE`,
		accountID).Scan(&status); err != nil {
		return mapError("lock account", err)
	}
	if status != "active" {
		return domain.NewError(domain.CodeInvalidKey, "billing account not active: "+status)
	}
	return nil
}

// activationConflict preserves the underlying unique/exclude violation and
// matches quota.ErrWindowActivationConflict for the service retry loop.
type activationConflict struct {
	kind  domain.WindowKind
	start time.Time
	cause error
}

func (e activationConflict) Error() string {
	return fmt.Sprintf("postgres: activate windows: concurrent activation of %s at %s: %v",
		e.kind, e.start.Format(time.RFC3339Nano), e.cause)
}

func (e activationConflict) Unwrap() error { return e.cause }

func (e activationConflict) Is(target error) bool {
	return target == quota.ErrWindowActivationConflict
}

// ActivateWindowsTx resolves — and on first consumption activates — the
// windows for one admission INSIDE the shared transaction, right after the
// account anchor lock. Concurrent first consumptions of the same account
// serialize on the account row, so at most one transaction creates the
// window row and every later transaction reads the committed row and
// reuses it (Task 7: 首次五小时窗口并发初始化). The
// (entitlement_id, kind, window_start) unique key and the no-overlap
// EXCLUDE constraint are the backstop against any path that skipped the
// account lock: a collision surfaces as quota.ErrWindowActivationConflict
// and the caller retries the whole admission.
//
// Disabled windows (no limit in cmd.Limits) are skipped — explicit, never
// unlimited (设计 §9.1). Reads outside admission must use LoadWindows,
// which never activates (设计 §6).
func (s *Store) ActivateWindowsTx(ctx context.Context, w domain.UnitOfWork, cmd quota.ActivateWindowsCommand) ([]domain.QuotaWindow, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	if err := lockAccountTx(ctx, tx, cmd.BillingAccountID); err != nil {
		return nil, err
	}
	var rows []windowRow
	if err := tx.SelectContext(ctx, &rows,
		`SELECT * FROM inference_quota_windows
		 WHERE entitlement_id = $1 AND state = 'active'
		 ORDER BY kind`, cmd.EntitlementID); err != nil {
		return nil, mapError("activate windows: load", err)
	}
	active := make([]domain.QuotaWindow, 0, len(rows))
	for _, r := range rows {
		active = append(active, r.toDomain())
	}
	out := make([]domain.QuotaWindow, 0, len(cmd.Limits))
	for _, kind := range domain.WindowOrder {
		limit, ok := cmd.Limits[kind]
		if !ok {
			continue
		}
		iv, activate, err := quota.ConsumptionWindow(kind, cmd.Anchor, active, cmd.At)
		if err != nil {
			return nil, err
		}
		if activate {
			nw := &domain.QuotaWindow{
				EntitlementID: cmd.EntitlementID, Kind: kind,
				Start: iv.Start, End: iv.End, Limit: limit,
			}
			if err := insertQuotaWindow(ctx, tx, nw); err != nil {
				if domain.CodeOf(err) == domain.CodeConflict {
					return nil, activationConflict{kind: kind, start: iv.Start, cause: err}
				}
				return nil, err
			}
			active = append(active, *nw)
			out = append(out, *nw)
			continue
		}
		for _, a := range active {
			if a.Kind == kind && !a.Voided &&
				(quota.Interval{Start: a.Start.UTC(), End: a.End.UTC()}).Contains(cmd.At) {
				out = append(out, a)
				break
			}
		}
	}
	return out, nil
}

// Reserve implements domain.QuotaStore. Steps inside the shared tx:
//
//  1. SELECT ... FOR UPDATE the billing account row (anchor lock).
//  2. Window holds in fixed order (five_hour → weekly → monthly):
//     conditional UPDATE guarded by used+reserved+amount <= limit.
//  3. Key budget hold: conditional UPDATE guarded by budget_limit.
//  4. INSERT the request (status=reserved) and one reservation row per hold.
//
// Any failure returns an error and the caller rolls the uow back — no
// partial holds survive (设计 §7.2). A rejected admission carries the
// deficit （缺口信息） — the smallest additional credit that would have
// admitted the request; 剩余额度不足默认拒绝，不静默钳制 (§7.2 补充条款).
func (s *Store) Reserve(ctx context.Context, w domain.UnitOfWork, cmd domain.ReserveCommand) (*domain.Admission, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}

	// 1. Anchor lock on the consumption owner.
	if err := lockAccountTx(ctx, tx, cmd.Request.BillingAccountID); err != nil {
		return nil, err
	}

	holds := append([]domain.HoldSpec(nil), cmd.Holds...)
	sort.SliceStable(holds, func(i, j int) bool {
		return windowOrderKey(holds[i].TargetKind) < windowOrderKey(holds[j].TargetKind)
	})

	var blocked []domain.WindowBlock
	keyBudgetBlocked := false
	var deficit domain.Microcredit
	// reserveAmount 是这次请求的单次消费预占上界：同一次消费镜像进
	// 全部目标（三窗口 + Key 预算），请求行只记一次，不累加各 hold
	// （否则 4 目标 = 4 倍膨胀）。holds 等额时即取其一；不等额取最大
	// 作为安全上界。
	var reserveAmount domain.Microcredit

	// 2./3. Apply holds.
	for _, h := range holds {
		if h.Amount <= 0 {
			return nil, domain.WrapError(domain.CodeInvalidInput, "reserve: hold amount must be > 0", domain.ErrNegativeValue)
		}
		switch h.TargetKind {
		case domain.TargetKeyBudget:
			if h.APIKeyID == nil {
				return nil, domain.NewError(domain.CodeInvalidInput, "reserve: key budget hold needs api_key_id")
			}
			var ok bool
			err := tx.QueryRowxContext(ctx,
				`UPDATE inference_api_keys
				 SET budget_used_micros = budget_used_micros + $2, updated_at = now()
				 WHERE id = $1
				   AND (budget_limit_micros IS NULL OR budget_used_micros + $2 <= budget_limit_micros)
				 RETURNING true`, *h.APIKeyID, int64(h.Amount)).Scan(&ok)
			if err != nil {
				if domain.CodeOf(mapError("reserve: key budget", err)) == domain.CodeNotFound {
					keyBudgetBlocked = true
					// 缺口信息：Key 预算还差多少才能放行（累计上限语义，
					// 不按周期清零 — 控制者裁决）。
					var lim sql.NullInt64
					var usedB int64
					if qerr := tx.QueryRowxContext(ctx,
						`SELECT budget_limit_micros, budget_used_micros
						 FROM inference_api_keys WHERE id = $1`, *h.APIKeyID).Scan(&lim, &usedB); qerr == nil && lim.Valid {
						if avail := lim.Int64 - usedB; avail < int64(h.Amount) {
							if d := h.Amount - domain.Microcredit(avail); d > deficit {
								deficit = d
							}
						}
					}
					continue
				}
				return nil, mapError("reserve: key budget", err)
			}
		default:
			if h.WindowID == nil {
				return nil, domain.NewError(domain.CodeInvalidInput, "reserve: window hold needs window_id")
			}
			var wr windowRow
			err := tx.QueryRowxContext(ctx,
				`UPDATE inference_quota_windows
				 SET reserved_micros = reserved_micros + $2
				 WHERE id = $1 AND state = 'active'
				   AND used_micros + reserved_micros + $2 <= limit_micros
				 RETURNING *`, *h.WindowID, int64(h.Amount)).StructScan(&wr)
			if err != nil {
				if domain.CodeOf(mapError("reserve: window", err)) == domain.CodeNotFound {
					// Limit exceeded (or window gone): read the window for
					// the block detail + deficit, then fail the whole reserve.
					var full windowRow
					if qerr := tx.QueryRowxContext(ctx,
						`SELECT * FROM inference_quota_windows WHERE id = $1`, *h.WindowID).StructScan(&full); qerr == nil {
						end := full.End
						blocked = append(blocked, domain.WindowBlock{
							Kind:           domain.WindowKind(full.Kind),
							LimitMicros:    domain.Microcredit(full.Limit),
							UsedMicros:     domain.Microcredit(full.Used),
							ReservedMicros: domain.Microcredit(full.Reserved),
							ResetsAt:       &end,
						})
						if avail := domain.Microcredit(full.Limit - full.Used - full.Reserved); avail < h.Amount {
							if d := h.Amount - avail; d > deficit {
								deficit = d
							}
						}
					}
					continue
				}
				return nil, mapError("reserve: window", err)
			}
		}
		if h.Amount > reserveAmount {
			reserveAmount = h.Amount
		}
	}

	if len(blocked) > 0 || keyBudgetBlocked {
		qe := domain.NewQuotaExceeded(blocked, keyBudgetBlocked)
		if deficit > 0 {
			qe.DeficitMicros = &deficit
		}
		return nil, qe
	}

	// 4. Persist request + reservations.
	req := cmd.Request
	req.Status = domain.ReqReserved
	req.AdmittedAt = &cmd.AdmittedAt
	req.ReservedMicros = &reserveAmount
	for _, h := range holds {
		switch h.TargetKind {
		case domain.TargetWindowFiveHour:
			req.WindowFiveHourID = h.WindowID
		case domain.TargetWindowWeekly:
			req.WindowWeeklyID = h.WindowID
		case domain.TargetWindowMonthly:
			req.WindowMonthlyID = h.WindowID
		}
	}
	if err := insertRequest(ctx, tx, &req); err != nil {
		return nil, err
	}
	reservations := make([]domain.Reservation, 0, len(holds))
	for _, h := range holds {
		r := domain.Reservation{
			ID:         uuid.NewString(),
			RequestID:  req.ID,
			TargetKind: h.TargetKind,
			WindowID:   h.WindowID,
			APIKeyID:   h.APIKeyID,
			Amount:     h.Amount,
			State:      domain.ReservationHeld,
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inference_reservations
			 (id, request_id, target_kind, window_id, api_key_id, amount_micros, state)
			 VALUES ($1,$2,$3,$4,$5,$6,'held')`,
			r.ID, r.RequestID, string(r.TargetKind), strPtr(r.WindowID), strPtr(r.APIKeyID), int64(r.Amount)); err != nil {
			return nil, mapError("reserve: insert reservation", err)
		}
		reservations = append(reservations, r)
	}

	return &domain.Admission{RequestID: req.ID, Holds: reservations}, nil
}
