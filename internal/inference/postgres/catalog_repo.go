package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"

	"github.com/yunhou/users/internal/inference/catalog"
	"github.com/yunhou/users/internal/inference/domain"
)

// catalog_repo.go — 表组 inference_models/providers/deployments/
// model_routes/config_revisions (migration 024).

var _ catalog.Store = (*Store)(nil)

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
	err := s.db.QueryRowxContext(ctx,
		`INSERT INTO inference_models
		 (id, display_name, lifecycle, model_version, aliases,
		  input_modalities, output_modalities, context_tokens, max_output_tokens,
		  protocols, supports_tools, supports_reasoning)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING created_at, updated_at`,
		m.ID, m.DisplayName, string(m.Lifecycle), m.ModelVersion, strArr(m.Aliases),
		strArr(m.InputModalities), strArr(m.OutputModalities), m.ContextTokens, m.MaxOutputTokens,
		strArr(protocols), m.SupportsTools, m.SupportsReasoning).
		Scan(&m.CreatedAt, &m.UpdatedAt)
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
	return listModelsQ(ctx, s.db, filter)
}

// querier is the shared query surface of *sqlx.DB and *sqlx.Tx (sqlx's own
// ExtContext omits GetContext/SelectContext), so the standalone store and
// the publish transaction share one set of query helpers.
type querier interface {
	sqlx.ExtContext
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
}

// listModelsQ is the ListModels query shared by the standalone store and
// the publish transaction (安全审查 M-3: publish drains inside its snapshot).
func listModelsQ(ctx context.Context, q querier, filter domain.ModelFilter) ([]domain.Model, error) {
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
	if err := q.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("list models", err)
	}
	out := make([]domain.Model, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// UpdateModel applies a full-field edit guarded by the optimistic-lock
// token m.UpdatedAt (设计 §5: 更新采用乐观锁版本号). The row's updated_at is
// the version: it is compared in the WHERE clause and bumped on success, so
// a concurrent edit between read and write turns into CodeConflict instead
// of a silent last-writer-wins.
func (s *Store) UpdateModel(ctx context.Context, m *domain.Model) error {
	protocols := make([]string, 0, len(m.Protocols))
	for _, p := range m.Protocols {
		protocols = append(protocols, string(p))
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_models SET
		   display_name=$2, lifecycle=$3, model_version=$4, aliases=$5,
		   input_modalities=$6, output_modalities=$7, context_tokens=$8,
		   max_output_tokens=$9, protocols=$10, supports_tools=$11,
		   supports_reasoning=$12, updated_at=now()
		 WHERE id=$1 AND updated_at=$13`,
		m.ID, m.DisplayName, string(m.Lifecycle), m.ModelVersion, strArr(m.Aliases),
		strArr(m.InputModalities), strArr(m.OutputModalities), m.ContextTokens, m.MaxOutputTokens,
		strArr(protocols), m.SupportsTools, m.SupportsReasoning, m.UpdatedAt)
	if err != nil {
		return mapError("update model", err)
	}
	if err := ensureUpdated(s, ctx, res, "inference_models", m.ID); err != nil {
		return err
	}
	return s.db.GetContext(ctx, &m.UpdatedAt,
		`SELECT updated_at FROM inference_models WHERE id=$1`, m.ID)
}

// mapDeleteFK 把删除路径上的 23503 外键违反映射成 409 conflict 而不是
// mapError 的 400（安全审查 M-10）：模型/供应商/部署被历史事实（价格、
// 用量、路由……）引用时，正确处置是 retire/disable 而不是删除——400 会
// 误导操作员反复修正请求而不是改走生命周期/停用路径。其余错误照常走
// mapError。
func mapDeleteFK(op, hint string, err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23503" {
		return domain.NewError(domain.CodeConflict,
			op+": still referenced by "+pqErr.Table+" — "+hint)
	}
	return mapError(op, err)
}

// DeleteModel removes a model and its route rows in one transaction.
func (s *Store) DeleteModel(ctx context.Context, id string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return mapError("delete model: begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM inference_model_routes WHERE model_id=$1`, id); err != nil {
		return mapError("delete model: routes", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM inference_models WHERE id=$1`, id)
	if err != nil {
		// 模型仍被价格/用量等历史事实引用：引导操作员走 retire 而非删除。
		return mapDeleteFK("delete model",
			"set its lifecycle to retired instead of deleting (dependent facts must stay navigable)", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("delete model", sql.ErrNoRows)
	}
	return mapError("delete model: commit", tx.Commit())
}

// ensureUpdated converts an optimistic-lock UPDATE result into a domain
// error: 0 rows means either the row is gone (NotFound) or the token was
// stale (Conflict). Existence is re-checked so the two stay distinguishable.
// table is a fixed literal chosen by the caller, never user input.
func ensureUpdated(s *Store, ctx context.Context, res sql.Result, table, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return mapError("update "+table, err)
	}
	if n > 0 {
		return nil
	}
	var exists bool
	if err := s.db.GetContext(ctx, &exists,
		`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id=$1)`, id); err == nil && exists {
		return domain.NewError(domain.CodeConflict,
			table+" was modified concurrently (stale version token)")
	}
	return domain.NewError(domain.CodeNotFound, table+" not found: "+id)
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

// insertConfigRevisionQ is the INSERT shared by the standalone path and the
// publish transaction (安全审查 M-2): it appends an immutable draft revision
// and fills rev.ID/rev.CreatedAt from the RETURNING clause.
func insertConfigRevisionQ(ctx context.Context, q querier, rev *domain.ConfigRevision) error {
	payload := rev.Payload.Raw
	if len(payload) == 0 {
		payload = json.RawMessage(`{"schema_version":1}`)
	}
	return q.QueryRowxContext(ctx,
		`INSERT INTO inference_config_revisions (scope, revision, payload, status, created_by)
		 VALUES ($1,$2,$3, COALESCE(NULLIF($4,''),'draft'), $5)
		 RETURNING id, created_at`,
		string(rev.Scope), rev.Revision, payload, string(rev.Status), rev.CreatedBy).
		Scan(&rev.ID, &rev.CreatedAt)
}

func (s *Store) InsertConfigRevision(ctx context.Context, rev *domain.ConfigRevision) error {
	return mapError("insert config revision", insertConfigRevisionQ(ctx, s.db, rev))
}

// activateRevisionQ switches the scope's active pointer: the previous active
// revision is superseded and this one becomes published+active in one
// statement pair guarded by the partial unique index. Shared by the
// standalone ActivateRevision and the publish transaction.
func activateRevisionQ(ctx context.Context, q querier, scope domain.ConfigScope, revision int) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE inference_config_revisions
		 SET is_active = false, status = 'superseded'
		 WHERE scope = $1 AND is_active`, string(scope)); err != nil {
		return mapError("activate revision: supersede", err)
	}
	res, err := q.ExecContext(ctx,
		`UPDATE inference_config_revisions
		 SET is_active = true, status = 'published', published_at = now()
		 WHERE scope = $1 AND revision = $2`, string(scope), revision)
	if err != nil {
		return mapError("activate revision: publish", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("activate revision", errNoSuchRevision(scope, revision))
	}
	return nil
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
	if err := activateRevisionQ(ctx, tx, scope, revision); err != nil {
		return err
	}
	return mapError("activate revision: commit", tx.Commit())
}

// publishTx is the single implementation of catalog.PublishTx: one live
// REPEATABLE READ transaction carrying the whole publish — every drain read
// sees the same snapshot (安全审查 M-3) and the revision insert + activation
// commit or roll back together (安全审查 M-2).
type publishTx struct {
	tx   *sqlx.Tx
	done bool
}

// BeginPublish opens the transaction of one publish at REPEATABLE READ
// (安全审查 M-3): the catalog drain (models/providers/deployments/routes +
// latest revision) and the insert+activate all share one snapshot, so a
// concurrent edit mid-drain cannot produce a self-contradictory payload.
// Racing publishers are separated by UNIQUE(scope, revision) — a second
// publisher's insert blocks until the first commits and then fails with
// CodeConflict, which is why no advisory lock is needed.
func (s *Store) BeginPublish(ctx context.Context) (catalog.PublishTx, error) {
	tx, err := s.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, mapError("begin publish", err)
	}
	return &publishTx{tx: tx}, nil
}

// ListModels/ListProviders/ListDeployments/ListRoutes drain the catalog
// inside the publish snapshot — same queries as the standalone store.
func (t *publishTx) ListModels(ctx context.Context, filter domain.ModelFilter) ([]domain.Model, error) {
	return listModelsQ(ctx, t.tx, filter)
}

func (t *publishTx) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	return listProvidersQ(ctx, t.tx, afterID, limit)
}

func (t *publishTx) ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error) {
	return listDeploymentsQ(ctx, t.tx, f)
}

func (t *publishTx) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	return listRoutesQ(ctx, t.tx, modelID)
}

func (t *publishTx) LatestRevision(ctx context.Context, scope domain.ConfigScope) (int, error) {
	var n int
	err := t.tx.GetContext(ctx, &n,
		`SELECT COALESCE(MAX(revision), 0) FROM inference_config_revisions WHERE scope = $1`,
		string(scope))
	return n, mapError("latest revision", err)
}

// GetRevision loads one immutable revision (payload included) inside the
// publish transaction — Rollback re-reads its target here so the target
// read and the insert+activate share one snapshot.
func (t *publishTx) GetRevision(ctx context.Context, scope domain.ConfigScope, revision int) (*domain.ConfigRevision, error) {
	var row revisionRow
	err := t.tx.GetContext(ctx, &row,
		`SELECT * FROM inference_config_revisions WHERE scope = $1 AND revision = $2`,
		string(scope), revision)
	if err != nil {
		return nil, mapError("get revision", err)
	}
	return row.toDomain(), nil
}

func (t *publishTx) InsertAndActivateRevision(ctx context.Context, rev *domain.ConfigRevision) error {
	if err := insertConfigRevisionQ(ctx, t.tx, rev); err != nil {
		return mapError("insert config revision", err) // UNIQUE(scope, revision) → CodeConflict on racing publishers
	}
	return activateRevisionQ(ctx, t.tx, rev.Scope, rev.Revision)
}

func (t *publishTx) Commit(ctx context.Context) error {
	if t.done {
		return domain.NewError(domain.CodeConflict, "publish transaction already finished")
	}
	t.done = true
	return mapError("publish: commit", t.tx.Commit())
}

func (t *publishTx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	t.done = true
	return t.tx.Rollback()
}

// ActiveRevision implements domain.CatalogReader.
func (s *Store) ActiveRevision(ctx context.Context, scope domain.ConfigScope) (*domain.ConfigRevision, error) {
	return s.GetRevisionByPredicate(ctx, scope, `is_active`)
}

// revisionRow is the shared SELECT shape of all config-revision reads.
type revisionRow struct {
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

func (r revisionRow) toDomain() *domain.ConfigRevision {
	return &domain.ConfigRevision{
		ID: r.ID, Scope: domain.ConfigScope(r.Scope), Revision: r.Revision,
		Payload: domain.ExtensionConfig{SchemaVersion: 1, Raw: r.Payload},
		Status:  domain.RevisionStatus(r.Status), IsActive: r.IsActive,
		PublishedAt: r.PublishedAt, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}
}

// GetRevisionByPredicate loads one revision scoped by a fixed predicate
// fragment ("is_active" or "revision = $2"). Fragments are literals chosen
// by the caller, never user input.
func (s *Store) GetRevisionByPredicate(ctx context.Context, scope domain.ConfigScope, predicate string) (*domain.ConfigRevision, error) {
	var row revisionRow
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_config_revisions WHERE scope = $1 AND `+predicate,
		string(scope))
	if err != nil {
		return nil, mapError("get revision", err)
	}
	return row.toDomain(), nil
}

// GetRevision returns one immutable revision by scope+revision number.
func (s *Store) GetRevision(ctx context.Context, scope domain.ConfigScope, revision int) (*domain.ConfigRevision, error) {
	var row revisionRow
	err := s.db.GetContext(ctx, &row,
		`SELECT * FROM inference_config_revisions WHERE scope = $1 AND revision = $2`,
		string(scope), revision)
	if err != nil {
		return nil, mapError("get revision", err)
	}
	return row.toDomain(), nil
}

// revisionMetaColumns is the metadata-only projection of
// inference_config_revisions: everything except the payload blob (安全审查
// M-1 — 修订列表/活跃修订探针不得拉大 JSONB 快照体).
const revisionMetaColumns = `id, scope, revision, status, is_active, published_at, created_by, created_at`

// revisionMetaRow is the metadata-only scan shape: payload excluded.
type revisionMetaRow struct {
	ID          int64      `db:"id"`
	Scope       string     `db:"scope"`
	Revision    int        `db:"revision"`
	Status      string     `db:"status"`
	IsActive    bool       `db:"is_active"`
	PublishedAt *time.Time `db:"published_at"`
	CreatedBy   string     `db:"created_by"`
	CreatedAt   time.Time  `db:"created_at"`
}

func (r revisionMetaRow) toDomain() domain.RevisionMeta {
	return domain.RevisionMeta{
		ID: r.ID, Scope: domain.ConfigScope(r.Scope), Revision: r.Revision,
		Status: domain.RevisionStatus(r.Status), IsActive: r.IsActive,
		PublishedAt: r.PublishedAt, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}
}

// ListRevisionMetas lists revision metadata of a scope, newest first,
// keyset-paginated by revision number (afterRevision is the cursor; <= 0
// starts from the newest). Revisions are immutable — this is the
// audit/history view used by rollback; the payload blob is never selected.
func (s *Store) ListRevisionMetas(ctx context.Context, scope domain.ConfigScope, afterRevision, limit int) ([]domain.RevisionMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + revisionMetaColumns + ` FROM inference_config_revisions WHERE scope = $1`
	args := []any{string(scope)}
	if afterRevision > 0 {
		args = append(args, afterRevision)
		query += ` AND revision < $2`
	}
	query += ` ORDER BY revision DESC LIMIT ` + itoa(limit)
	var rows []revisionMetaRow
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("list revision metas", err)
	}
	out := make([]domain.RevisionMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// ActiveRevisionMeta loads the active revision's metadata without the
// payload blob (安全审查 M-1).
func (s *Store) ActiveRevisionMeta(ctx context.Context, scope domain.ConfigScope) (*domain.RevisionMeta, error) {
	var row revisionMetaRow
	err := s.db.GetContext(ctx, &row,
		`SELECT `+revisionMetaColumns+` FROM inference_config_revisions WHERE scope = $1 AND is_active`,
		string(scope))
	if err != nil {
		return nil, mapError("active revision meta", err)
	}
	m := row.toDomain()
	return &m, nil
}

// LatestRevision returns the highest revision number of a scope, 0 when the
// scope has none yet. Publish computes next = LatestRevision+1; a concurrent
// publisher loses on the UNIQUE(scope, revision) insert and gets CodeConflict.
func (s *Store) LatestRevision(ctx context.Context, scope domain.ConfigScope) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n,
		`SELECT COALESCE(MAX(revision), 0) FROM inference_config_revisions WHERE scope = $1`,
		string(scope))
	return n, mapError("latest revision", err)
}

// ActiveRevisionHead is the cheap freshness probe of the snapshot cache:
// it reads only id+revision (no payload) so each call can detect a publish
// without paying for the full snapshot row.
func (s *Store) ActiveRevisionHead(ctx context.Context, scope domain.ConfigScope) (id int64, revision int, err error) {
	err = s.db.QueryRowxContext(ctx,
		`SELECT id, revision FROM inference_config_revisions WHERE scope = $1 AND is_active`,
		string(scope)).Scan(&id, &revision)
	return id, revision, mapError("active revision head", err)
}

// --- providers (CRUD remainder) ---

type providerRow struct {
	ID          string    `db:"id"`
	Code        string    `db:"code"`
	DisplayName string    `db:"display_name"`
	AccessType  string    `db:"access_type"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

func (r providerRow) toDomain() domain.Provider {
	return domain.Provider{
		ID: r.ID, Code: r.Code, DisplayName: r.DisplayName,
		AccessType: domain.AccessType(r.AccessType), Status: r.Status,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// ListProviders lists providers keyset-paginated by id.
func (s *Store) ListProviders(ctx context.Context, afterID string, limit int) ([]domain.Provider, error) {
	return listProvidersQ(ctx, s.db, afterID, limit)
}

// listProvidersQ is the ListProviders query shared by the standalone store
// and the publish transaction (安全审查 M-3).
func listProvidersQ(ctx context.Context, q querier, afterID string, limit int) ([]domain.Provider, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT * FROM inference_providers`
	args := []any{}
	if afterID != "" {
		args = append(args, afterID)
		query += ` WHERE id > $1`
	}
	query += ` ORDER BY id LIMIT ` + itoa(limit)
	var rows []providerRow
	err := q.SelectContext(ctx, &rows, query, args...)
	if err != nil {
		return nil, mapError("list providers", err)
	}
	out := make([]domain.Provider, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// GetProvider loads a provider by ID.
func (s *Store) GetProvider(ctx context.Context, id string) (*domain.Provider, error) {
	var r providerRow
	if err := s.db.GetContext(ctx, &r,
		`SELECT * FROM inference_providers WHERE id=$1`, id); err != nil {
		return nil, mapError("get provider", err)
	}
	p := r.toDomain()
	return &p, nil
}

// GetProviderByCode loads a provider by its stable code (env import and
// upsert-by-identity paths).
func (s *Store) GetProviderByCode(ctx context.Context, code string) (*domain.Provider, error) {
	var r providerRow
	if err := s.db.GetContext(ctx, &r,
		`SELECT * FROM inference_providers WHERE code=$1`, code); err != nil {
		return nil, mapError("get provider by code", err)
	}
	p := r.toDomain()
	return &p, nil
}

// UpdateProvider applies an edit guarded by the updated_at optimistic-lock
// token (same contract as UpdateModel).
func (s *Store) UpdateProvider(ctx context.Context, p *domain.Provider) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_providers SET
		   code=$2, display_name=$3, access_type=$4, status=$5, updated_at=now()
		 WHERE id=$1 AND updated_at=$6`,
		p.ID, p.Code, p.DisplayName, string(p.AccessType), p.Status, p.UpdatedAt)
	if err != nil {
		return mapError("update provider", err)
	}
	if err := ensureUpdated(s, ctx, res, "inference_providers", p.ID); err != nil {
		return err
	}
	return s.db.GetContext(ctx, &p.UpdatedAt,
		`SELECT updated_at FROM inference_providers WHERE id=$1`, p.ID)
}

// DeleteProvider refuses while deployments (or any other dependent rows)
// still reference the provider — catalog history must stay navigable. The
// single-statement DELETE leans on the foreign keys as the source of truth:
// it either succeeds or fails with 23503, so the old count-then-delete race
// window is closed (安全审查 M-10). 0 rows → 404.
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inference_providers WHERE id=$1`, id)
	if err != nil {
		return mapDeleteFK("delete provider", "disable it or remove the dependents first", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("delete provider", sql.ErrNoRows)
	}
	return nil
}

// --- deployments (CRUD remainder) ---

// DeploymentFilter is the catalog-package listing filter; it is the same
// shape as domain.DeploymentFilter (kept as an alias so existing callers
// of the postgres type keep compiling).
type DeploymentFilter = domain.DeploymentFilter

// ListDeployments lists deployments keyset-paginated by id.
func (s *Store) ListDeployments(ctx context.Context, f domain.DeploymentFilter) ([]domain.Deployment, error) {
	return listDeploymentsQ(ctx, s.db, f)
}

// listDeploymentsQ is the ListDeployments query shared by the standalone
// store and the publish transaction (安全审查 M-3).
func listDeploymentsQ(ctx context.Context, q querier, f domain.DeploymentFilter) ([]domain.Deployment, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT * FROM inference_deployments`
	args := []any{}
	where := ` WHERE `
	if f.AfterID != "" {
		args = append(args, f.AfterID)
		query += where + `id > $` + itoa(len(args))
		where = ` AND `
	}
	if f.ProviderID != "" {
		args = append(args, f.ProviderID)
		query += where + `provider_id = $` + itoa(len(args))
		where = ` AND `
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		query += where + `status = $` + itoa(len(args))
	}
	query += ` ORDER BY id LIMIT ` + itoa(limit)
	var rows []deploymentRow
	if err := q.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, mapError("list deployments", err)
	}
	out := make([]domain.Deployment, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// FindDeployment locates a deployment by the natural key
// (provider, upstream_model, base_url) — the idempotent env-import probe.
func (s *Store) FindDeployment(ctx context.Context, providerID, upstreamModel, baseURL string) (*domain.Deployment, error) {
	var r deploymentRow
	err := s.db.GetContext(ctx, &r,
		`SELECT * FROM inference_deployments
		 WHERE provider_id=$1 AND upstream_model=$2 AND base_url=$3`,
		providerID, upstreamModel, baseURL)
	if err != nil {
		return nil, mapError("find deployment", err)
	}
	d := r.toDomain()
	return &d, nil
}

// UpdateDeployment applies an edit guarded by the config_version
// optimistic-lock counter (设计 §5): the WHERE pins the caller's version
// and the SET bumps it, so concurrent editors conflict instead of
// overwriting each other.
func (s *Store) UpdateDeployment(ctx context.Context, d *domain.Deployment) error {
	cfg := d.Config.Raw
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{"schema_version":1}`)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_deployments SET
		   provider_id=$2, upstream_model=$3, base_url=$4, protocol=$5,
		   region=$6, connect_timeout_ms=$7, request_timeout_ms=$8,
		   config=$9, status=$10, updated_at=now(), config_version=config_version+1
		 WHERE id=$1 AND config_version=$11`,
		d.ID, d.ProviderID, d.UpstreamModel, d.BaseURL, string(d.Protocol), d.Region,
		int(d.ConnectTimeout/time.Millisecond), int(d.RequestTimeout/time.Millisecond),
		cfg, string(d.Status), d.ConfigVersion)
	if err != nil {
		return mapError("update deployment", err)
	}
	var exists bool
	if n, _ := res.RowsAffected(); n == 0 {
		if err := s.db.GetContext(ctx, &exists,
			`SELECT EXISTS (SELECT 1 FROM inference_deployments WHERE id=$1)`, d.ID); err == nil && exists {
			return domain.NewError(domain.CodeConflict,
				"deployment was modified concurrently (stale config_version)")
		}
		return domain.NewError(domain.CodeNotFound, "deployment not found: "+d.ID)
	}
	return s.db.QueryRowxContext(ctx,
		`SELECT config_version, updated_at FROM inference_deployments WHERE id=$1`,
		d.ID).Scan(&d.ConfigVersion, &d.UpdatedAt)
}

// DeleteDeployment refuses while routes still reference the deployment.
// Single-statement DELETE + FK violation → 409 (no count-then-delete race,
// 安全审查 M-10); 0 rows → 404.
func (s *Store) DeleteDeployment(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inference_deployments WHERE id=$1`, id)
	if err != nil {
		return mapDeleteFK("delete deployment", "disable the routes first", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("delete deployment", sql.ErrNoRows)
	}
	return nil
}

// --- model routes (CRUD remainder) ---

type routeRow struct {
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

func (r routeRow) toDomain() domain.ModelRoute {
	return domain.ModelRoute{
		ID: r.ID, ModelID: r.ModelID, DeploymentID: r.DeploymentID,
		Priority: r.Priority, Weight: r.Weight, Capabilities: []string(r.Capabilities),
		PoolStrategy: domain.PoolStrategy(r.Strategy), Enabled: r.Enabled,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// ListRoutes returns every route of a model, enabled or not (the operator
// view; RoutesForModel is the runtime enabled-only view).
func (s *Store) ListRoutes(ctx context.Context, modelID string) ([]domain.ModelRoute, error) {
	return listRoutesQ(ctx, s.db, modelID)
}

// listRoutesQ is the ListRoutes query shared by the standalone store and
// the publish transaction (安全审查 M-3).
func listRoutesQ(ctx context.Context, q querier, modelID string) ([]domain.ModelRoute, error) {
	var rows []routeRow
	err := q.SelectContext(ctx, &rows,
		`SELECT * FROM inference_model_routes WHERE model_id=$1
		 ORDER BY priority ASC, weight DESC, id`, modelID)
	if err != nil {
		return nil, mapError("list routes", err)
	}
	out := make([]domain.ModelRoute, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// GetRoute loads a route by ID.
func (s *Store) GetRoute(ctx context.Context, id string) (*domain.ModelRoute, error) {
	var r routeRow
	if err := s.db.GetContext(ctx, &r,
		`SELECT * FROM inference_model_routes WHERE id=$1`, id); err != nil {
		return nil, mapError("get route", err)
	}
	route := r.toDomain()
	return &route, nil
}

// UpdateRoute applies an edit guarded by the updated_at optimistic-lock
// token (same contract as UpdateModel).
func (s *Store) UpdateRoute(ctx context.Context, r *domain.ModelRoute) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inference_model_routes SET
		   priority=$2, weight=$3, capabilities=$4, account_pool_strategy=$5,
		   enabled=$6, updated_at=now()
		 WHERE id=$1 AND updated_at=$7`,
		r.ID, r.Priority, r.Weight, strArr(r.Capabilities),
		string(r.PoolStrategy), r.Enabled, r.UpdatedAt)
	if err != nil {
		return mapError("update route", err)
	}
	if err := ensureUpdated(s, ctx, res, "inference_model_routes", r.ID); err != nil {
		return err
	}
	return s.db.GetContext(ctx, &r.UpdatedAt,
		`SELECT updated_at FROM inference_model_routes WHERE id=$1`, r.ID)
}

// DeleteRoute removes a route by ID.
func (s *Store) DeleteRoute(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inference_model_routes WHERE id=$1`, id)
	if err != nil {
		return mapError("delete route", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return mapError("delete route", sql.ErrNoRows)
	}
	return nil
}
