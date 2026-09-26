// policy_repo.go — 配额策略管理面(spec
// 2026-09-26-admin-quota-policies-design.md)的持久层:list/详情投影、
// referenced_by 统计、max+1 派生、draft 编辑、publish(同事务
// supersede,Q3 + 迁移 040 部分唯一索引兜底)、retire。
package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// policyToInfo 把行投影为管理层形状(microcredit → int64)。
func policyToInfo(p *PolicyVersion) *management.QuotaPolicyInfo {
	var five, weekly, monthly, tpm *int64
	if p.FiveHourLimit != nil {
		v := int64(*p.FiveHourLimit)
		five = &v
	}
	if p.WeeklyLimit != nil {
		v := int64(*p.WeeklyLimit)
		weekly = &v
	}
	if p.MonthlyLimit != nil {
		v := int64(*p.MonthlyLimit)
		monthly = &v
	}
	if p.TPMLimit != nil {
		v := *p.TPMLimit
		tpm = &v
	}
	return &management.QuotaPolicyInfo{
		ID: p.ID, Name: p.Name, Revision: p.Revision, ModelIDs: p.ModelIDs,
		FiveHourLimit: five, WeeklyLimit: weekly, MonthlyLimit: monthly,
		RPMLimit: p.RPMLimit, TPMLimit: tpm, ConcurrencyLimit: p.ConcurrencyLimit,
		OveragePolicy: p.OveragePolicy, Status: p.Status,
		CreatedAt: p.CreatedAt, PublishedAt: p.PublishedAt,
	}
}

// GetQuotaPolicyVersion loads one revision as the management projection.
func (s *Store) GetQuotaPolicyVersion(ctx context.Context, id string) (*management.QuotaPolicyInfo, error) {
	p, err := s.GetPolicyVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	return policyToInfo(p), nil
}

// ListQuotaPolicies returns the filtered page ordered by
// (name ASC, revision DESC). Limit <= 0 uses a defensive default.
func (s *Store) ListQuotaPolicies(ctx context.Context, f management.QuotaPolicyFilter) ([]management.QuotaPolicyInfo, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, name, revision, model_ids,
		five_hour_limit_micros, weekly_limit_micros, monthly_limit_micros,
		rpm_limit, tpm_limit, concurrency_limit, overage_policy, status,
		created_at, published_at
		FROM inference_policy_versions WHERE TRUE`
	args := []any{}
	if f.Name != "" {
		args = append(args, f.Name)
		query += ` AND name = $` + strconv.Itoa(len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		query += ` AND status = $` + strconv.Itoa(len(args))
	}
	query += ` ORDER BY name ASC, revision DESC LIMIT ` + strconv.Itoa(limit) + ` OFFSET ` + strconv.Itoa(f.Offset)

	rows, err := s.db.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, mapError("list policy versions", err)
	}
	defer rows.Close()
	out := []management.QuotaPolicyInfo{}
	for rows.Next() {
		var row struct {
			ID        string         `db:"id"`
			Name      string         `db:"name"`
			Revision  int            `db:"revision"`
			ModelIDs  pq.StringArray `db:"model_ids"`
			FiveHour  sql.NullInt64  `db:"five_hour_limit_micros"`
			Weekly    sql.NullInt64  `db:"weekly_limit_micros"`
			Monthly   sql.NullInt64  `db:"monthly_limit_micros"`
			RPM       sql.NullInt64  `db:"rpm_limit"`
			TPM       sql.NullInt64  `db:"tpm_limit"`
			Conc      sql.NullInt64  `db:"concurrency_limit"`
			Overage   string         `db:"overage_policy"`
			Status    string         `db:"status"`
			CreatedAt time.Time      `db:"created_at"`
			Published *time.Time     `db:"published_at"`
		}
		if err := rows.StructScan(&row); err != nil {
			return nil, mapError("list policy versions scan", err)
		}
		out = append(out, *policyToInfo(&PolicyVersion{
			ID: row.ID, Name: row.Name, Revision: row.Revision, ModelIDs: []string(row.ModelIDs),
			FiveHourLimit: microFromNull(row.FiveHour), WeeklyLimit: microFromNull(row.Weekly),
			MonthlyLimit: microFromNull(row.Monthly),
			RPMLimit:     intFromNull(row.RPM), TPMLimit: int64FromNull(row.TPM),
			ConcurrencyLimit: intFromNull(row.Conc),
			OveragePolicy:    row.Overage, Status: row.Status,
			CreatedAt: row.CreatedAt, PublishedAt: row.Published,
		}))
	}
	return out, rows.Err()
}

// CountActiveEntitlementsByPolicy counts entitlements pinning this version
// that are usable NOW (status='active' AND 窗口未关闭) — retire 保护规则与
// 详情页 referenced_by 的唯一数据源。
func (s *Store) CountActiveEntitlementsByPolicy(ctx context.Context, policyVersionID string) (int, error) {
	var n int
	if err := s.db.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM inference_entitlements
		 WHERE policy_version_id = $1 AND status = 'active'
		   AND (effective_to IS NULL OR effective_to > now())`,
		policyVersionID).Scan(&n); err != nil {
		return 0, mapError("count active entitlements by policy", err)
	}
	return n, nil
}

// MaxPolicyRevisionTx reads MAX(revision) of one name inside the caller's
// transaction (0 when none) — create 的服务端 max+1 派生。
func (s *Store) MaxPolicyRevisionTx(ctx context.Context, w domain.UnitOfWork, name string) (int, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return 0, err
	}
	var max sql.NullInt64
	if err := tx.QueryRowxContext(ctx,
		`SELECT MAX(revision) FROM inference_policy_versions WHERE name = $1`,
		name).Scan(&max); err != nil {
		return 0, mapError("max policy revision", err)
	}
	if !max.Valid {
		return 0, nil
	}
	return int(max.Int64), nil
}

// InsertQuotaPolicyTx appends the new draft revision inside the caller's
// transaction and fills ID/CreatedAt.UNIQUE(name,revision) 与「每 name 单
// published」部分索引的冲突都映射 CodeConflict。
func (s *Store) InsertQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, p *management.QuotaPolicyInfo) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	err = tx.QueryRowxContext(ctx,
		`INSERT INTO inference_policy_versions
		 (name, revision, model_ids,
		  five_hour_limit_micros, weekly_limit_micros, monthly_limit_micros,
		  rpm_limit, tpm_limit, concurrency_limit, overage_policy, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at`,
		p.Name, p.Revision, strArr(p.ModelIDs),
		i64OrNil(p.FiveHourLimit), i64OrNil(p.WeeklyLimit), i64OrNil(p.MonthlyLimit),
		intPtr(p.RPMLimit), int64Ptr(p.TPMLimit), intPtr(p.ConcurrencyLimit),
		p.OveragePolicy, "draft").
		Scan(&p.ID, &p.CreatedAt)
	return mapError("insert quota policy", err)
}

// UpdateDraftQuotaPolicyTx replaces the mutable content of a DRAFT row
// (name/revision 不可变);0 rows(非 draft)→ CodeConflict。
func (s *Store) UpdateDraftQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, p *management.QuotaPolicyInfo) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_policy_versions SET
		 model_ids = $2,
		 five_hour_limit_micros = $3, weekly_limit_micros = $4, monthly_limit_micros = $5,
		 rpm_limit = $6, tpm_limit = $7, concurrency_limit = $8,
		 overage_policy = $9
		 WHERE id = $1 AND status = 'draft'`,
		p.ID, strArr(p.ModelIDs),
		i64OrNil(p.FiveHourLimit), i64OrNil(p.WeeklyLimit), i64OrNil(p.MonthlyLimit),
		intPtr(p.RPMLimit), int64Ptr(p.TPMLimit), intPtr(p.ConcurrencyLimit),
		p.OveragePolicy)
	if err != nil {
		return mapError("update draft quota policy", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapError("update draft quota policy rows", err)
	}
	if n == 0 {
		return domain.NewError(domain.CodeConflict, "quota policy is not draft")
	}
	return nil
}

// PublishQuotaPolicyTx atomically:同名当前 published(若有)转 superseded
// (Q3),目标 draft 转 published 并记 published_at;目标非 draft(0 行)→
// CodeConflict。先 supersede 后 publish 的顺序避免撞迁移 040 的部分唯一
// 索引(同一事务内瞬时不出现两条同名 published)。
func (s *Store) PublishQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, id, name string, at time.Time) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_policy_versions SET status = 'superseded'
		 WHERE name = $1 AND status = 'published'`, name); err != nil {
		return mapError("supersede previous published policy", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_policy_versions SET status = 'published', published_at = $2
		 WHERE id = $1 AND status = 'draft'`, id, at)
	if err != nil {
		return mapError("publish quota policy", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapError("publish quota policy rows", err)
	}
	if n == 0 {
		return domain.NewError(domain.CodeConflict, "quota policy is not draft")
	}
	return nil
}

// RetireQuotaPolicyTx marks draft/published/superseded → retired;已
// retired(0 行)→ CodeConflict。
func (s *Store) RetireQuotaPolicyTx(ctx context.Context, w domain.UnitOfWork, id string) error {
	tx, err := sqlTx(w)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_policy_versions SET status = 'retired'
		 WHERE id = $1 AND status IN ('draft', 'published', 'superseded')`, id)
	if err != nil {
		return mapError("retire quota policy", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapError("retire quota policy rows", err)
	}
	if n == 0 {
		return domain.NewError(domain.CodeConflict, "quota policy already retired")
	}
	return nil
}

// i64OrNil converts an optional int64 limit to the driver value.
func i64OrNil(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// GetQuotaPolicyForUpdateTx locks the policy row (SELECT ... FOR UPDATE)
// inside the caller's transaction — retire 的引用保护把「计数」放进同一
// 事务,锁与 FK 的 KEY SHARE 互斥,并发 grant(INSERT 引用本行)被定序,
// 计数结果在提交前不可能失效(评审轮1 TOCTOU 修复)。
func (s *Store) GetQuotaPolicyForUpdateTx(ctx context.Context, w domain.UnitOfWork, id string) (*management.QuotaPolicyInfo, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return nil, err
	}
	var row struct {
		ID        string         `db:"id"`
		Name      string         `db:"name"`
		Revision  int            `db:"revision"`
		ModelIDs  pq.StringArray `db:"model_ids"`
		FiveHour  sql.NullInt64  `db:"five_hour_limit_micros"`
		Weekly    sql.NullInt64  `db:"weekly_limit_micros"`
		Monthly   sql.NullInt64  `db:"monthly_limit_micros"`
		RPM       sql.NullInt64  `db:"rpm_limit"`
		TPM       sql.NullInt64  `db:"tpm_limit"`
		Conc      sql.NullInt64  `db:"concurrency_limit"`
		Overage   string         `db:"overage_policy"`
		Status    string         `db:"status"`
		CreatedAt time.Time      `db:"created_at"`
		Published *time.Time     `db:"published_at"`
	}
	if err := tx.GetContext(ctx, &row,
		`SELECT id, name, revision, model_ids,
		 five_hour_limit_micros, weekly_limit_micros, monthly_limit_micros,
		 rpm_limit, tpm_limit, concurrency_limit, overage_policy, status,
		 created_at, published_at
		 FROM inference_policy_versions WHERE id = $1 FOR UPDATE`, id); err != nil {
		return nil, mapError("lock quota policy", err)
	}
	return policyToInfo(&PolicyVersion{
		ID: row.ID, Name: row.Name, Revision: row.Revision, ModelIDs: []string(row.ModelIDs),
		FiveHourLimit: microFromNull(row.FiveHour), WeeklyLimit: microFromNull(row.Weekly),
		MonthlyLimit: microFromNull(row.Monthly),
		RPMLimit:     intFromNull(row.RPM), TPMLimit: int64FromNull(row.TPM),
		ConcurrencyLimit: intFromNull(row.Conc),
		OveragePolicy:    row.Overage, Status: row.Status,
		CreatedAt: row.CreatedAt, PublishedAt: row.Published,
	}), nil
}

// CountActiveEntitlementsByPolicyTx is CountActiveEntitlementsByPolicy
// inside the caller's transaction (与策略行锁配套,见 GetQuotaPolicyForUpdateTx)。
func (s *Store) CountActiveEntitlementsByPolicyTx(ctx context.Context, w domain.UnitOfWork, policyVersionID string) (int, error) {
	tx, err := sqlTx(w)
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM inference_entitlements
		 WHERE policy_version_id = $1 AND status = 'active'
		   AND (effective_to IS NULL OR effective_to > now())`,
		policyVersionID).Scan(&n); err != nil {
		return 0, mapError("count active entitlements by policy tx", err)
	}
	return n, nil
}
