package postgres

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// quota_repo.go — 表组 inference_quota_windows/reservations/
// concurrency_leases (migration 026) + domain.QuotaStore 实现。
//
// 事务契约（任务书）：Reserve/Release 与 SettlementStore.Settle 共享同一
// 个 UnitOfWork（同一个 *sqlx.Tx）。三窗口与 Key 预算在一次 Reserve 内
// 全部成功才提交；任何一步失败整个事务回滚，不遗留部分占用。

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

// windowOrderKey ranks holds into the fixed lock order (设计 §7.2: 原子
// 检查并锁定适用窗口，顺序固定). The key budget is locked BEFORE windows
// (both are account-local rows; windows are the hottest rows).
func windowOrderKey(k domain.ReservationTargetKind) int {
	switch k {
	case domain.TargetKeyBudget:
		return 0
	case domain.TargetWindowFiveHour:
		return 1
	case domain.TargetWindowWeekly:
		return 2
	case domain.TargetWindowMonthly:
		return 3
	}
	return 9
}

// Reserve implements domain.QuotaStore. Steps inside the shared tx:
//
//  1. SELECT ... FOR UPDATE the billing account row (anchor lock).
//  2. Key budget hold: conditional UPDATE guarded by budget_limit.
//  3. Window holds in fixed order: conditional UPDATE guarded by
//     used+reserved+amount <= limit.
//  4. INSERT the request (status=reserved) and one reservation row per hold.
//
// Any failure returns an error and the caller rolls the uow back — no
// partial holds survive (设计 §7.2).
func (s *Store) Reserve(ctx context.Context, w domain.UnitOfWork, cmd domain.ReserveCommand) (*domain.Admission, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}

	// 1. Anchor lock on the consumption owner.
	var status string
	if err := tx.QueryRowxContext(ctx,
		`SELECT status FROM inference_billing_accounts WHERE id = $1 FOR UPDATE`,
		cmd.Request.BillingAccountID).Scan(&status); err != nil {
		return nil, mapError("reserve: lock account", err)
	}
	if status != "active" {
		return nil, domain.NewError(domain.CodeInvalidKey, "billing account not active: "+status)
	}

	holds := append([]domain.HoldSpec(nil), cmd.Holds...)
	sort.SliceStable(holds, func(i, j int) bool {
		return windowOrderKey(holds[i].TargetKind) < windowOrderKey(holds[j].TargetKind)
	})

	var blocked []domain.WindowBlock
	keyBudgetBlocked := false
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
					// the block detail, then fail the whole reserve.
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
		return nil, domain.NewQuotaExceeded(blocked, keyBudgetBlocked)
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

// Release implements domain.QuotaStore: every held reservation of the
// request is released inside the shared tx and the request moves to
// 'released' (设计 §7.2 reserved → released, 确认没有发生上游消费).
func (s *Store) Release(ctx context.Context, w domain.UnitOfWork, requestID string) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	var rows []struct {
		ID         string  `db:"id"`
		TargetKind string  `db:"target_kind"`
		WindowID   *string `db:"window_id"`
		APIKeyID   *string `db:"api_key_id"`
		Amount     int64   `db:"amount_micros"`
	}
	if err := tx.SelectContext(ctx, &rows,
		`SELECT id, target_kind, window_id, api_key_id, amount_micros
		 FROM inference_reservations
		 WHERE request_id = $1 AND state = 'held'
		 ORDER BY target_kind`, requestID); err != nil {
		return mapError("release: load reservations", err)
	}
	if len(rows) == 0 {
		return mapError("release: no held reservations", sql.ErrNoRows)
	}
	for _, r := range rows {
		switch r.TargetKind {
		case string(domain.TargetKeyBudget):
			if _, err := tx.ExecContext(ctx,
				`UPDATE inference_api_keys
				 SET budget_used_micros = budget_used_micros - $2, updated_at = now()
				 WHERE id = $1`, *r.APIKeyID, r.Amount); err != nil {
				return mapError("release: key budget", err)
			}
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE inference_quota_windows
				 SET reserved_micros = reserved_micros - $2
				 WHERE id = $1`, *r.WindowID, r.Amount); err != nil {
				return mapError("release: window", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE inference_reservations
			 SET state = 'released', released_at = now() WHERE id = $1`, r.ID); err != nil {
			return mapError("release: reservation", err)
		}
	}
	if err := s.updateRequestStatus(ctx, tx, requestID, domain.ReqReleased, ""); err != nil {
		return err
	}
	return nil
}

// InsertConcurrencyLease persists one lease (骨架; Task 7 实现续租与回收).
func (s *Store) InsertConcurrencyLease(ctx context.Context, scope string, scopeID, requestID, ownerToken string, fencing int64, expiresAt time.Time) (string, error) {
	id := uuid.NewString()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inference_concurrency_leases
		 (id, scope, scope_id, request_id, owner_token, fencing_token, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, scope, scopeID, requestID, ownerToken, fencing, expiresAt)
	if err != nil {
		return "", mapError("insert lease", err)
	}
	return id, nil
}
