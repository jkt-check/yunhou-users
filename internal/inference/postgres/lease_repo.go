package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/yunhou/users/internal/inference/domain"
)

// lease_repo.go — inference_concurrency_leases (migration 026) 的数据库租
// 约协议（Task 7：续租、所有权、fencing token；超时回收不得与仍活跃请求
// 重叠授权）。
//
// 协议：
//   - 获取：同事务内先 pg_advisory_xact_lock(scope:scope_id) 序列化同 scope
//     的获取/回收；回收已到期租约（held→expired）；活跃（held 且未到期）
//     数量达到上限 → CodeInsufficientCapacity（附最早恢复时刻）。fencing
//     token 取该 scope 历史最大值 + 1 —— 跨回收单调递增。终态行由
//     DeleteTerminalLeases 定期收割（保留窗口 7 天，挂 upstream_health
//     轮次）；行被删后 fencing 号可重新开始，这不破坏安全性：续租/释放/
//     使用点检查都按行级 (id, owner, fencing) 精确匹配，已删行的所有权
//     断言永远失败。
//   - 所有权：(owner_token, fencing_token) 二元组。续租/释放都要求精确匹配
//     且 state='held'；不匹配或已迁移 → CodeConflict（失去所有权/被 fence）。
//   - 超时回收本身不证明旧请求已死（设计 §7.2: 租约到期本身不能证明没有消
//     费），所以回收后旧持有者绝不能继续自认为被授权：新租约的 fencing token
//     严格更大，旧持有者的 Renew/Release/Check 全部失败；下游资源使用点前
//     调用 CheckLease（状态+所有权+未过期）即不会与仍活跃的旧请求产生重叠
//     授权。fencing token 是获取历史的单调序号与所有权防伪证据，不是多槽位
//     scope 内的互斥门。

// leaseRow is the storage row of inference_concurrency_leases.
type leaseRow struct {
	ID        string     `db:"id"`
	Scope     string     `db:"scope"`
	ScopeID   string     `db:"scope_id"`
	RequestID string     `db:"request_id"`
	Owner     string     `db:"owner_token"`
	Fencing   int64      `db:"fencing_token"`
	State     string     `db:"state"`
	Acquired  time.Time  `db:"acquired_at"`
	Expires   time.Time  `db:"expires_at"`
	Released  *time.Time `db:"released_at"`
}

func (r leaseRow) toDomain() *domain.ConcurrencyLease {
	return &domain.ConcurrencyLease{
		ID: r.ID, Scope: domain.LeaseScope(r.Scope), ScopeID: r.ScopeID,
		RequestID: r.RequestID, OwnerToken: r.Owner, FencingToken: r.Fencing,
		State: domain.LeaseState(r.State), AcquiredAt: r.Acquired,
		ExpiresAt: r.Expires, ReleasedAt: r.Released,
	}
}

// scopeLockKey is the advisory-lock subject of one (scope, scope_id) pair.
func scopeLockKey(scope domain.LeaseScope, scopeID string) string {
	return "inference_lease:" + string(scope) + ":" + scopeID
}

// AcquireLeaseTx takes one concurrency lease inside the shared transaction.
// The per-scope advisory xact lock serializes acquisition and reclamation,
// so the count check and the fencing-token allocation are race-free across
// service instances sharing the database (Task 7 跨实例并发控制).
func (s *Store) AcquireLeaseTx(ctx context.Context, w domain.UnitOfWork, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	if cmd.Limit <= 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "lease: limit must be > 0")
	}
	if cmd.TTL <= 0 {
		return nil, domain.NewError(domain.CodeInvalidInput, "lease: TTL must be > 0")
	}
	if cmd.OwnerToken == "" {
		cmd.OwnerToken = uuid.NewString()
	}
	now := cmd.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// Serialize all lease traffic of this scope for the tx lifetime.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1))`, scopeLockKey(cmd.Scope, cmd.ScopeID)); err != nil {
		return nil, mapError("lease: scope lock", err)
	}

	// Reclaim timed-out leases. This NEVER asserts the old request is dead
	// — the fencing protocol below is what keeps authorizations disjoint.
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_concurrency_leases SET state = 'expired'
		 WHERE scope = $1 AND scope_id = $2 AND state = 'held' AND expires_at <= $3`,
		string(cmd.Scope), cmd.ScopeID, now); err != nil {
		return nil, mapError("lease: reclaim expired", err)
	}

	// 聚合拆成两条查询（评审修复批次8）：held 计数/最早恢复时刻走 026 的
	// 部分索引（WHERE state = 'held' 可直接命中，FILTER 写法不能）；fencing
	// 最大值走 035 的 (scope, scope_id, fencing_token DESC) 索引首行。两条
	// 都不必再扫描该 scope 的终态历史行。
	var live int
	var earliestExpiry *time.Time
	if err := tx.QueryRowxContext(ctx,
		`SELECT COUNT(*), MIN(expires_at)
		 FROM inference_concurrency_leases
		 WHERE scope = $1 AND scope_id = $2 AND state = 'held'`,
		string(cmd.Scope), cmd.ScopeID).
		Scan(&live, &earliestExpiry); err != nil {
		return nil, mapError("lease: count held", err)
	}
	var maxFencing int64
	if err := tx.QueryRowxContext(ctx,
		`SELECT COALESCE(MAX(fencing_token), 0)
		 FROM inference_concurrency_leases
		 WHERE scope = $1 AND scope_id = $2`,
		string(cmd.Scope), cmd.ScopeID).
		Scan(&maxFencing); err != nil {
		return nil, mapError("lease: max fencing", err)
	}
	if live >= cmd.Limit {
		msg := fmt.Sprintf("lease: scope %s/%s at concurrency limit %d", cmd.Scope, cmd.ScopeID, cmd.Limit)
		if earliestExpiry != nil {
			msg += fmt.Sprintf(" (earliest slot frees at %s)", earliestExpiry.Format(time.RFC3339))
		}
		return nil, domain.NewError(domain.CodeInsufficientCapacity, msg)
	}

	row := leaseRow{
		ID: uuid.NewString(), Scope: string(cmd.Scope), ScopeID: cmd.ScopeID,
		RequestID: cmd.RequestID, Owner: cmd.OwnerToken,
		Fencing: maxFencing + 1, State: string(domain.LeaseHeld),
		Acquired: now, Expires: now.Add(cmd.TTL),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inference_concurrency_leases
		 (id, scope, scope_id, request_id, owner_token, fencing_token, state, acquired_at, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'held',$7,$8)`,
		row.ID, row.Scope, row.ScopeID, row.RequestID, row.Owner, row.Fencing,
		row.Acquired, row.Expires); err != nil {
		return nil, mapError("lease: insert", err)
	}
	return row.toDomain(), nil
}

// AcquireLease acquires a lease in its own transaction (convenience wrapper
// for callers without an open UnitOfWork).
func (s *Store) AcquireLease(ctx context.Context, cmd domain.AcquireLeaseCommand) (*domain.ConcurrencyLease, error) {
	uow, err := s.Begin(ctx)
	if err != nil {
		return nil, err
	}
	l, err := s.AcquireLeaseTx(ctx, uow, cmd)
	if err != nil {
		_ = uow.Rollback(ctx)
		return nil, err
	}
	if err := uow.Commit(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// RenewLease extends a held lease. Ownership is proven by the exact
// (owner_token, fencing_token) pair on a still-held lease; a reclaimed or
// fenced lease fails with CodeConflict — a timed-out holder can never
// reassert authorization (Task 7 fencing).
func (s *Store) RenewLease(ctx context.Context, leaseID, ownerToken string, fencing int64, newExpiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_concurrency_leases
		 SET expires_at = $4
		 WHERE id = $1 AND owner_token = $2 AND fencing_token = $3 AND state = 'held'`,
		leaseID, ownerToken, fencing, newExpiresAt.UTC())
	if err != nil {
		return mapError("lease: renew", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"lease: renew failed — not held by this owner/fencing token (reclaimed or fenced)")
	}
	return nil
}

// ReleaseLease drops a held lease under the same ownership proof. Losing
// ownership (expired/reclaimed/fenced) is a CodeConflict, NOT a silent
// success — the caller learns its authorization was already gone.
func (s *Store) ReleaseLease(ctx context.Context, leaseID, ownerToken string, fencing int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_concurrency_leases
		 SET state = 'released', released_at = now()
		 WHERE id = $1 AND owner_token = $2 AND fencing_token = $3 AND state = 'held'`,
		leaseID, ownerToken, fencing)
	if err != nil {
		return mapError("lease: release", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.NewError(domain.CodeConflict,
			"lease: release failed — not held by this owner/fencing token")
	}
	return nil
}

// DeleteTerminalLeases reaps terminal lease rows (released/expired) whose
// terminal timestamp is older than cutoff (bounded batch). 评审修复批次8：
// 此前无任何 job 删除终态租约，繁忙账户 scope 每请求一行永久累积，准入
// 热路径成本随账户生命周期线性增长。
//
// 安全性：fencing 单调性只需对当前 held 行成立。续租/释放/使用点检查都按
// 行级 (id, owner_token, fencing_token) 精确匹配——终态行被删后旧持有者
// 的所有权断言命中 0 行（CodeConflict）或 load not-found，不可能借复用
// 的 fencing 号重新自认被授权；held 行永不在收割范围内。released 行以
// released_at 为终态时刻；expired 行（超时回收，released_at 为 NULL）以
// expires_at 为终态时刻。
//
// Wired into the upstream-health pass alongside the binding/chain sweeps.
func (s *Store) DeleteTerminalLeases(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inference_concurrency_leases
		  WHERE id IN (SELECT id FROM inference_concurrency_leases
		                WHERE state IN ('released', 'expired')
		                  AND COALESCE(released_at, expires_at) < $1
		                ORDER BY COALESCE(released_at, expires_at) LIMIT $2)`,
		cutoff.UTC(), limit)
	if err != nil {
		return 0, mapError("reap terminal leases", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CheckLease verifies the caller's lease is still a live authorization
// BEFORE the resource is used (e.g. right before upstream dispatch): the
// lease must be held, unexpired, and owned by the exact (owner_token,
// fencing_token) pair. Timeout reclamation flips the old lease to
// 'expired' and hands the slot out under a strictly higher fencing token,
// so a still-active earlier request fails this check at its next use point
// and can never reassert authorization — reclamation therefore never
// produces overlapping authorizations (Task 7 验收). Concurrent LIVE
// leases of a multi-slot scope deliberately do NOT fence each other: the
// token orders acquisition history, it is not a mutual-exclusion gate.
func (s *Store) CheckLease(ctx context.Context, leaseID, ownerToken string, fencing int64, now time.Time) error {
	var row leaseRow
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_concurrency_leases WHERE id = $1`, leaseID)
	if err != nil {
		return mapError("lease: check load", err)
	}
	t := now.UTC()
	if t.IsZero() {
		t = time.Now().UTC()
	}
	if row.State != string(domain.LeaseHeld) || row.Owner != ownerToken || row.Fencing != fencing {
		return domain.NewError(domain.CodeConflict,
			"lease: not held by this owner/fencing token (state="+row.State+")")
	}
	if !row.Expires.After(t) {
		return domain.NewError(domain.CodeConflict, "lease: expired — renew before use or stop")
	}
	return nil
}
