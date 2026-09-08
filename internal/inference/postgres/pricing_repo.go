package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// pricing_repo.go — 表组 inference_price_versions/policy_versions
// (migration 026)。价格与策略不可变版本化：只 INSERT 新版本，不 UPDATE
// 已生效行（运营修改价格只影响生效后的请求，设计 §7.1）。

// PriceKind mirrors the DB CHECK on inference_price_versions.kind.
type PriceKind = string

const (
	PriceSaleCredit   PriceKind = "sale_credit"
	PriceSaleMoney    PriceKind = "sale_money"
	PriceUpstreamCost PriceKind = "upstream_cost"
)

// PriceVersion is the stored shape of one immutable price revision.
// Rates are micro-units per 1M tokens (unit=microcredit for the credit
// price list, unit=micromoney+currency for money lists).
type PriceVersion struct {
	ID                string
	ModelID           string
	Kind              PriceKind
	Unit              string // "microcredit" | "micromoney"
	Currency          string // ISO-4217; empty for microcredit
	InputPerMtok      int64
	CacheReadPerMtok  int64
	CacheWritePerMtok int64
	OutputPerMtok     int64
	ExtraRates        domain.ExtensionConfig
	Revision          int
	EffectiveFrom     time.Time
	EffectiveTo       *time.Time
	CreatedAt         time.Time
}

// InsertPriceVersion appends one immutable price version.
func (s *Store) InsertPriceVersion(ctx context.Context, p *PriceVersion) error {
	extra := p.ExtraRates.Raw
	if len(extra) == 0 {
		extra = json.RawMessage(`{"schema_version":1}`)
	}
	var currency interface{}
	if p.Currency != "" {
		currency = p.Currency
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_price_versions
		 (model_id, kind, unit, currency,
		  input_micros_per_mtok, cache_read_micros_per_mtok,
		  cache_write_micros_per_mtok, output_micros_per_mtok,
		  extra_rates, revision, effective_from, effective_to)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING id, created_at`,
		p.ModelID, p.Kind, p.Unit, currency,
		p.InputPerMtok, p.CacheReadPerMtok, p.CacheWritePerMtok, p.OutputPerMtok,
		extra, p.Revision, p.EffectiveFrom, p.EffectiveTo).
		Scan(&p.ID, &p.CreatedAt)
	return mapError("insert price version", err)
}

// LatestPriceVersion returns the revision effective at `at`.
func (s *Store) LatestPriceVersion(ctx context.Context, modelID, kind string, at time.Time) (*PriceVersion, error) {
	var row struct {
		ID         string          `db:"id"`
		ModelID    string          `db:"model_id"`
		Kind       string          `db:"kind"`
		Unit       string          `db:"unit"`
		Currency   sql.NullString  `db:"currency"`
		Input      int64           `db:"input_micros_per_mtok"`
		CacheRead  int64           `db:"cache_read_micros_per_mtok"`
		CacheWrite int64           `db:"cache_write_micros_per_mtok"`
		Output     int64           `db:"output_micros_per_mtok"`
		Extra      json.RawMessage `db:"extra_rates"`
		Revision   int             `db:"revision"`
		From       time.Time       `db:"effective_from"`
		To         *time.Time      `db:"effective_to"`
		CreatedAt  time.Time       `db:"created_at"`
	}
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_price_versions
		 WHERE model_id = $1 AND kind = $2
		   AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		 ORDER BY revision DESC LIMIT 1`, modelID, kind, at)
	if err != nil {
		return nil, mapError("latest price version", err)
	}
	return &PriceVersion{
		ID: row.ID, ModelID: row.ModelID, Kind: row.Kind, Unit: row.Unit,
		Currency:     stringFromNull(row.Currency),
		InputPerMtok: row.Input, CacheReadPerMtok: row.CacheRead,
		CacheWritePerMtok: row.CacheWrite, OutputPerMtok: row.Output,
		ExtraRates: domain.ExtensionConfig{SchemaVersion: 1, Raw: row.Extra},
		Revision:   row.Revision, EffectiveFrom: row.From, EffectiveTo: row.To,
		CreatedAt: row.CreatedAt,
	}, nil
}

// PolicyVersion is the stored shape of one immutable quota policy
// revision (设计 §5 PolicyVersion: 模型集合、三个窗口限额、RPM/TPM/并发
// 及超额策略). A nil window limit means the window is DISABLED — never
// read as unlimited (设计 §9.2: 不能把缺失解释为无限额度).
type PolicyVersion struct {
	ID               string
	Name             string
	Revision         int
	ModelIDs         []string
	FiveHourLimit    *domain.Microcredit
	WeeklyLimit      *domain.Microcredit
	MonthlyLimit     *domain.Microcredit
	RPMLimit         *int
	TPMLimit         *int64
	ConcurrencyLimit *int
	OveragePolicy    string // reject | clamp_if_declared | allow_overage
	Status           string // draft | published | retired
	CreatedAt        time.Time
	PublishedAt      *time.Time
}

// InsertPolicyVersion appends one immutable policy revision
// (UNIQUE(name, revision)).
func (s *Store) InsertPolicyVersion(ctx context.Context, p *PolicyVersion) error {
	overage := p.OveragePolicy
	if overage == "" {
		overage = "reject"
	}
	status := p.Status
	if status == "" {
		status = "draft"
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_policy_versions
		 (name, revision, model_ids,
		  five_hour_limit_micros, weekly_limit_micros, monthly_limit_micros,
		  rpm_limit, tpm_limit, concurrency_limit, overage_policy, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 RETURNING id, created_at`,
		p.Name, p.Revision, strArr(p.ModelIDs),
		microPtr(p.FiveHourLimit), microPtr(p.WeeklyLimit), microPtr(p.MonthlyLimit),
		intPtr(p.RPMLimit), int64Ptr(p.TPMLimit), intPtr(p.ConcurrencyLimit),
		overage, status).
		Scan(&p.ID, &p.CreatedAt)
	return mapError("insert policy version", err)
}

// GetPolicyVersion loads one immutable revision by id.
func (s *Store) GetPolicyVersion(ctx context.Context, id string) (*PolicyVersion, error) {
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
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_policy_versions WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get policy version", err)
	}
	return &PolicyVersion{
		ID: row.ID, Name: row.Name, Revision: row.Revision, ModelIDs: []string(row.ModelIDs),
		FiveHourLimit: microFromNull(row.FiveHour), WeeklyLimit: microFromNull(row.Weekly),
		MonthlyLimit: microFromNull(row.Monthly),
		RPMLimit:     intFromNull(row.RPM), TPMLimit: int64FromNull(row.TPM),
		ConcurrencyLimit: intFromNull(row.Conc),
		OveragePolicy:    row.Overage, Status: row.Status,
		CreatedAt: row.CreatedAt, PublishedAt: row.Published,
	}, nil
}

func stringFromNull(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}

func int64Ptr(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func int64FromNull(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}
