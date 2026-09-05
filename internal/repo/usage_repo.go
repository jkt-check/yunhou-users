package repo

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/yunhou/users/internal/model"
)

// UsageRepo is the storage surface for the usage-analytics feature
// (2026-09-04-usage-analytics-design.md): heartbeat writes plus the three
// admin aggregation reads. All date parameters are client-local calendar
// dates (YYYY-MM-DD strings matching usage_events.local_date), except
// CountNewUsers which derives its day boundary from users.created_at in the
// server's timezone.
//
// groupBy values ("platform" / "app_version") are validated by the service
// layer before reaching here; the repo still switches on a closed set and
// never interpolates caller input into SQL (Design Principles: parameterized
// queries only — the column name itself can't be a bind parameter, so the
// whitelist switch IS the parameterization).
type UsageRepo interface {
	// InsertBeats writes a heartbeat batch for one user. Duplicate
	// (user_id, client_event_id) rows are silently swallowed by
	// ON CONFLICT DO NOTHING — client retries and offline-buffer replays
	// must not double-count. Returns the number of rows actually inserted
	// (duplicates excluded) so the response can tell live beats from replays.
	InsertBeats(ctx context.Context, userID, appID string, beats []model.UsageBeat) (int, error)
	// CountActiveUsers returns distinct users with ≥1 beat in [from, to].
	CountActiveUsers(ctx context.Context, from, to string) (int, error)
	// CountActiveUsersGrouped breaks the same distinct-user count down by
	// platform or app_version.
	CountActiveUsersGrouped(ctx context.Context, from, to, groupBy string) ([]model.ActiveStatsRow, error)
	// UsageDurationStats aggregates SUM(active_seconds) and distinct users
	// per day (granularity="day") or month ("month"), optionally split by
	// groupBy. PerUserSeconds is derived by the caller (service).
	UsageDurationStats(ctx context.Context, from, to, granularity, groupBy string) ([]model.UsageDurationRow, error)
	// CountNewUsers groups users.created_at by calendar day in [from, to]
	// (server timezone — see model.NewUsersRow).
	CountNewUsers(ctx context.Context, from, to string) ([]model.NewUsersRow, error)
}

type usageRepo struct{ db *sqlx.DB }

func NewUsageRepo(db *sqlx.DB) *usageRepo { return &usageRepo{db: db} }

// usageGroupColumn maps the API's group_by enum to its column. Anything
// else is rejected — the column name goes into SQL text, so it must come
// from this closed set, never from the request.
func usageGroupColumn(groupBy string) (string, error) {
	switch groupBy {
	case "platform":
		return "platform", nil
	case "app_version":
		return "app_version", nil
	default:
		return "", fmt.Errorf("unsupported group_by %q", groupBy)
	}
}

func (r *usageRepo) InsertBeats(ctx context.Context, userID, appID string, beats []model.UsageBeat) (int, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after Commit

	inserted := 0
	const stmt = `INSERT INTO usage_events
		(user_id, app_id, client_event_id, occurred_at, local_date, active_seconds, platform, app_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, client_event_id) DO NOTHING`
	for _, b := range beats {
		res, err := tx.ExecContext(ctx, stmt,
			userID, appID, b.ClientEventID, b.OccurredAt, b.LocalDate,
			b.ActiveSeconds, b.Platform, b.AppVersion)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil {
			inserted += int(n)
		}
	}
	return inserted, tx.Commit()
}

func (r *usageRepo) CountActiveUsers(ctx context.Context, from, to string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT user_id) FROM usage_events WHERE local_date BETWEEN $1 AND $2`,
		from, to).Scan(&n)
	return n, err
}

// activeRow mirrors model.ActiveStatsRow with db tags for StructScan.
type activeRow struct {
	Group string `db:"grp"`
	Users int    `db:"users"`
}

func (r *usageRepo) CountActiveUsersGrouped(ctx context.Context, from, to, groupBy string) ([]model.ActiveStatsRow, error) {
	col, err := usageGroupColumn(groupBy)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + col + ` AS grp, COUNT(DISTINCT user_id) AS users
		FROM usage_events WHERE local_date BETWEEN $1 AND $2
		GROUP BY ` + col + ` ORDER BY users DESC, ` + col
	var rows []activeRow
	if err := r.db.SelectContext(ctx, &rows, query, from, to); err != nil {
		return nil, err
	}
	out := make([]model.ActiveStatsRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.ActiveStatsRow{Group: row.Group, Users: row.Users})
	}
	return out, nil
}

// durationRow mirrors model.UsageDurationRow with db tags for StructScan.
// PerUserSeconds is NOT selected — the service derives it so the division
// by zero guard lives in one place.
type durationRow struct {
	Period       string `db:"period"`
	Group        string `db:"grp"`
	TotalSeconds int64  `db:"total_seconds"`
	ActiveUsers  int    `db:"active_users"`
}

func (r *usageRepo) UsageDurationStats(ctx context.Context, from, to, granularity, groupBy string) ([]model.UsageDurationRow, error) {
	var periodExpr string
	switch granularity {
	case "day":
		periodExpr = "local_date::text"
	case "month":
		periodExpr = "to_char(date_trunc('month', local_date), 'YYYY-MM')"
	default:
		return nil, fmt.Errorf("unsupported granularity %q", granularity)
	}

	selectGroup, groupClause, orderGroup := "", "", ""
	if groupBy != "" {
		col, err := usageGroupColumn(groupBy)
		if err != nil {
			return nil, err
		}
		selectGroup = ", " + col + " AS grp"
		groupClause = ", " + col
		orderGroup = ", " + col
	}

	query := `SELECT ` + periodExpr + ` AS period` + selectGroup + `,
		SUM(active_seconds) AS total_seconds, COUNT(DISTINCT user_id) AS active_users
		FROM usage_events WHERE local_date BETWEEN $1 AND $2
		GROUP BY period` + groupClause + ` ORDER BY period` + orderGroup

	var rows []durationRow
	if err := r.db.SelectContext(ctx, &rows, query, from, to); err != nil {
		return nil, err
	}
	out := make([]model.UsageDurationRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.UsageDurationRow{
			Period:       row.Period,
			Group:        row.Group,
			TotalSeconds: row.TotalSeconds,
			ActiveUsers:  row.ActiveUsers,
		})
	}
	return out, nil
}

// newUsersRow mirrors model.NewUsersRow with db tags for StructScan.
type newUsersRow struct {
	Date  string `db:"d"`
	Users int    `db:"users"`
}

func (r *usageRepo) CountNewUsers(ctx context.Context, from, to string) ([]model.NewUsersRow, error) {
	// Half-open range [from, to+1day) on the raw TIMESTAMPTZ keeps the
	// comparison sargable and immune to the server's session timezone —
	// only the day BUCKET label (created_at::date) follows server tz.
	const query = `SELECT created_at::date::text AS d, COUNT(*) AS users
		FROM users
		WHERE created_at >= $1::date AND created_at < $2::date + 1
		GROUP BY created_at::date ORDER BY created_at::date`
	var rows []newUsersRow
	if err := r.db.SelectContext(ctx, &rows, query, from, to); err != nil {
		return nil, err
	}
	out := make([]model.NewUsersRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.NewUsersRow{Date: row.Date, Users: row.Users})
	}
	return out, nil
}
