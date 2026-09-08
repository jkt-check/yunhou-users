package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/domain"
)

// catalog_repo.go — 表组 inference_models/providers/deployments/
// model_routes/config_revisions (migration 024).

type modelRow struct {
	ID               string         `db:"id"`
	DisplayName      string         `db:"display_name"`
	Lifecycle        string         `db:"lifecycle"`
	ModelVersion     string         `db:"model_version"`
	Aliases          pq.StringArray `db:"aliases"`
	InputModalities  pq.StringArray `db:"input_modalities"`
	OutputModalities pq.StringArray `db:"output_modalities"`
	ContextTokens    int            `db:"context_tokens"`
	MaxOutputTokens  int            `db:"max_output_tokens"`
	Protocols        pq.StringArray `db:"protocols"`
	SupportsTools    bool           `db:"supports_tools"`
	ReasoningSupport bool           `db:"supports_reasoning"`
	CreatedAt        time.Time      `db:"created_at"`
	UpdatedAt        time.Time      `db:"updated_at"`
}

func (r modelRow) toDomain() domain.Model {
	protocols := make([]domain.Protocol, 0, len(r.Protocols))
	for _, p := range r.Protocols {
		protocols = append(protocols, domain.Protocol(p))
	}
	return domain.Model{
		ID: r.ID, DisplayName: r.DisplayName, Lifecycle: domain.Lifecycle(r.Lifecycle),
		ModelVersion: r.ModelVersion, Aliases: []string(r.Aliases),
		InputModalities: []string(r.InputModalities), OutputModalities: []string(r.OutputModalities),
		ContextTokens: r.ContextTokens, MaxOutputTokens: r.MaxOutputTokens,
		Protocols: protocols, SupportsTools: r.SupportsTools, SupportsReasoning: r.ReasoningSupport,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// InsertModel creates a catalog model. Lifecycle defaults to draft (新模型
// 默认不可售) when empty.
func (s *Store) InsertModel(ctx context.Context, m *domain.Model) error {
	if m.Lifecycle == "" {
		m.Lifecycle = domain.LifecycleDraft
	}
	protocols := make([]string, 0, len(m.Protocols))
	for _, p := range m.Protocols {
		protocols = append(protocols, string(p))
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inference_models
		 (id, display_name, lifecycle, model_version, aliases,
		  input_modalities, output_modalities, context_tokens, max_output_tokens,
		  protocols, supports_tools, supports_reasoning)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		m.ID, m.DisplayName, string(m.Lifecycle), m.ModelVersion, strArr(m.Aliases),
		strArr(m.InputModalities), strArr(m.OutputModalities), m.ContextTokens, m.MaxOutputTokens,
		strArr(protocols), m.SupportsTools, m.SupportsReasoning)
	return mapError("insert model", err)
}

// GetModel implements domain.CatalogReader.
func (s *Store) GetModel(ctx context.Context, id string) (*domain.Model, error) {
	var r modelRow
	err := s.db.GetContext(ctx, &r, `SELECT * FROM inference_models WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get model", err)
	}
	m := r.toDomain()
	return &m, nil
}

// ListModels implements domain.CatalogReader with keyset pagination on id.
func (s *Store) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT * FROM inference_models WHERE id > $1`
	args := []any{filter.AfterID}
	if filter.Lifecycle != nil {
		query += ` AND lifecycle = $2`
		args = append(args, string(*filter.Lifecycle))
	}
	query += ` ORDER BY id LIMIT ` + itoa(limit)
	var rows []modelRow
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("list models", err)
	}
	out := make([]domain.Model, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// InsertProvider creates an upstream provider row; the DB assigns the ID.
func (s *Store) InsertProvider(ctx context.Context, p *domain.Provider) error {
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_providers (code, display_name, access_type, status)
		 VALUES ($1,$2,$3, COALESCE(NULLIF($4,''),'active'))
		 RETURNING id, created_at, updated_at`,
		p.Code, p.DisplayName, string(p.AccessType), p.Status).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	return mapError("insert provider", err)
}

// InsertDeployment creates an upstream deployment; the DB assigns the ID.
func (s *Store) InsertDeployment(ctx context.Context, d *domain.Deployment) error {
	cfg := d.Config.Raw
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{"schema_version":1}`)
	}
	if d.ConfigVersion == 0 {
		d.ConfigVersion = 1
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_deployments
		 (provider_id, upstream_model, base_url, protocol, region,
		  connect_timeout_ms, request_timeout_ms, config_version, config, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, COALESCE(NULLIF($10,''),'draft'))
		 RETURNING id, created_at, updated_at`,
		d.ProviderID, d.UpstreamModel, d.BaseURL, string(d.Protocol), d.Region,
		int(d.ConnectTimeout/time.Millisecond), int(d.RequestTimeout/time.Millisecond),
		d.ConfigVersion, cfg, string(d.Status)).
		Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	return mapError("insert deployment", err)
}

type deploymentRow struct {
	ID            string          `db:"id"`
	ProviderID    string          `db:"provider_id"`
	UpstreamModel string          `db:"upstream_model"`
	BaseURL       string          `db:"base_url"`
	Protocol      string          `db:"protocol"`
	Region        string          `db:"region"`
	ConnectMs     int             `db:"connect_timeout_ms"`
	RequestMs     int             `db:"request_timeout_ms"`
	ConfigVersion int             `db:"config_version"`
	Config        json.RawMessage `db:"config"`
	Status        string          `db:"status"`
	CreatedAt     time.Time       `db:"created_at"`
	UpdatedAt     time.Time       `db:"updated_at"`
}

func (r deploymentRow) toDomain() domain.Deployment {
	return domain.Deployment{
		ID: r.ID, ProviderID: r.ProviderID, UpstreamModel: r.UpstreamModel,
		BaseURL: r.BaseURL, Protocol: domain.Protocol(r.Protocol), Region: r.Region,
		ConnectTimeout: time.Duration(r.ConnectMs) * time.Millisecond,
		RequestTimeout: time.Duration(r.RequestMs) * time.Millisecond,
		ConfigVersion:  r.ConfigVersion,
		Config:         domain.ExtensionConfig{SchemaVersion: 1, Raw: r.Config},
		Status:         domain.DeploymentStatus(r.Status),
		CreatedAt:      r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// GetDeployment implements domain.CatalogReader.
func (s *Store) GetDeployment(ctx context.Context, id string) (*domain.Deployment, error) {
	var r deploymentRow
	err := s.db.GetContext(ctx, &r, `SELECT * FROM inference_deployments WHERE id = $1`, id)
	if err != nil {
		return nil, mapError("get deployment", err)
	}
	d := r.toDomain()
	return &d, nil
}

// InsertRoute maps a model to a deployment (UNIQUE(model_id, deployment_id)).
func (s *Store) InsertRoute(ctx context.Context, r *domain.ModelRoute) error {
	strategy := string(r.PoolStrategy)
	if strategy == "" {
		strategy = string(domain.PoolRoundRobin)
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_model_routes
		 (model_id, deployment_id, priority, weight, capabilities, account_pool_strategy, enabled)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING id, created_at, updated_at`,
		r.ModelID, r.DeploymentID, r.Priority, r.Weight, strArr(r.Capabilities), strategy, r.Enabled).
		Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
	return mapError("insert route", err)
}

// RoutesForModel implements domain.CatalogReader: enabled routes ordered
// by priority then weight (设计 §5).
func (s *Store) RoutesForModel(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	var rows []struct {
		ID           string         `db:"id"`
		ModelID      string         `db:"model_id"`
		DeploymentID string         `db:"deployment_id"`
		Priority     int            `db:"priority"`
		Weight       int            `db:"weight"`
		Capabilities pq.StringArray `db:"capabilities"`
		Strategy     string         `db:"account_pool_strategy"`
		Enabled      bool           `db:"enabled"`
		CreatedAt    time.Time      `db:"created_at"`
		UpdatedAt    time.Time      `db:"updated_at"`
	}
	err := s.db.SelectContext(ctx, &rows,
		`SELECT * FROM inference_model_routes
		 WHERE model_id = $1 AND enabled
		 ORDER BY priority ASC, weight DESC, id`, modelID)
	if err != nil {
		return nil, mapError("routes for model", err)
	}
	out := make([]domain.ModelRoute, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.ModelRoute{
			ID: r.ID, ModelID: r.ModelID, DeploymentID: r.DeploymentID,
			Priority: r.Priority, Weight: r.Weight, Capabilities: []string(r.Capabilities),
			PoolStrategy: domain.PoolStrategy(r.Strategy), Enabled: r.Enabled,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// InsertConfigRevision appends an immutable draft revision.
func (s *Store) InsertConfigRevision(ctx context.Context, rev *domain.ConfigRevision) error {
	payload := rev.Payload.Raw
	if len(payload) == 0 {
		payload = json.RawMessage(`{"schema_version":1}`)
	}
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_config_revisions (scope, revision, payload, status, created_by)
		 VALUES ($1,$2,$3, COALESCE(NULLIF($4,''),'draft'), $5)
		 RETURNING id, created_at`,
		string(rev.Scope), rev.Revision, payload, string(rev.Status), rev.CreatedBy).
		Scan(&rev.ID, &rev.CreatedAt)
	return mapError("insert config revision", err)
}

// ActivateRevision atomically switches the scope's active revision (设计
// §5: 发布原子切换 active revision): the previous active revision is
// superseded and this one becomes published+active in one statement pair
// guarded by the partial unique index.
func (s *Store) ActivateRevision(ctx context.Context, scope domain.ConfigScope, revision int) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return mapError("activate revision: begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE inference_config_revisions
		 SET is_active = false, status = 'superseded'
		 WHERE scope = $1 AND is_active`, string(scope)); err != nil {
		return mapError("activate revision: supersede", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE inference_config_revisions
		 SET is_active = true, status = 'published', published_at = now()
		 WHERE scope = $1 AND revision = $2`, string(scope), revision)
	if err != nil {
		return mapError("activate revision: publish", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return mapError("activate revision", errNoSuchRevision(scope, revision))
	}
	return mapError("activate revision: commit", tx.Commit())
}

// ActiveRevision implements domain.CatalogReader.
func (s *Store) ActiveRevision(ctx context.Context, scope domain.ConfigScope) (*domain.ConfigRevision, error) {
	var row struct {
		ID          int64           `db:"id"`
		Scope       string          `db:"scope"`
		Revision    int             `db:"revision"`
		Payload     json.RawMessage `db:"payload"`
		Status      string          `db:"status"`
		IsActive    bool            `db:"is_active"`
		PublishedAt *time.Time      `db:"published_at"`
		CreatedBy   string          `db:"created_by"`
		CreatedAt   time.Time       `db:"created_at"`
	}
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_config_revisions WHERE scope = $1 AND is_active`, string(scope))
	if err != nil {
		return nil, mapError("active revision", err)
	}
	return &domain.ConfigRevision{
		ID: row.ID, Scope: domain.ConfigScope(row.Scope), Revision: row.Revision,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: row.Payload},
		Status:  domain.RevisionStatus(row.Status), IsActive: row.IsActive,
		PublishedAt: row.PublishedAt, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt,
	}, nil
}
