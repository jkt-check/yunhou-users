package model

import "time"

// UsageBeat is one client heartbeat: the minimal non-PII payload kaya sends
// every 5 minutes while a logged-in session is alive (design:
// 2026-09-04-usage-analytics-design.md §3.1). It deliberately carries no
// nickname/email/conversation content — identity comes from the JWT, never
// from the body.
type UsageBeat struct {
	// ClientEventID is a per-beat UUIDv4 minted by the client. Combined with
	// the JWT user_id it forms the idempotency key: offline-buffered beats
	// are replayed in batches and the server's UNIQUE constraint swallows
	// duplicates.
	ClientEventID string `json:"client_event_id"`
	OccurredAt    time.Time `json:"occurred_at"`
	// LocalDate is the CLIENT-local calendar date (YYYY-MM-DD) the beat's
	// active_seconds are attributed to. The server trusts it verbatim — this
	// is an ops-metrics surface, not billing.
	LocalDate     string `json:"local_date"`
	ActiveSeconds int    `json:"active_seconds"`
	Platform      string `json:"platform"` // macos | windows | linux
	AppVersion    string `json:"app_version"`
}

// UsageHeartbeatRequest is the POST /user/usage/heartbeat body: a batch of
// beats (1 when live, more when flushing the offline buffer).
type UsageHeartbeatRequest struct {
	Beats []UsageBeat `json:"beats"`
}

const (
	// UsageMaxBeatsPerRequest bounds one heartbeat batch (offline buffer
	// flush); the client never exceeds it and larger batches are rejected.
	UsageMaxBeatsPerRequest = 100
	// UsageMaxActiveSeconds mirrors the DB CHECK (0–3600). The client caps
	// each beat at its heartbeat interval (300s); 3600 is the defensive
	// ceiling so a buggy/hostile client can't inflate duration stats.
	UsageMaxActiveSeconds = 3600
	// UsageMaxAppVersionLen bounds the app_version string (SemVer strings
	// are ~10 chars; 64 leaves headroom for build metadata).
	UsageMaxAppVersionLen = 64
)

// UsagePlatforms is the closed set the DB CHECK constraint enforces.
var UsagePlatforms = []string{"macos", "windows", "linux"}

// ActiveStatsRow is one group's distinct-active-user count in the
// /admin/stats/active response. Group is empty when no group_by was given.
type ActiveStatsRow struct {
	Group string `json:"group,omitempty"`
	Users int    `json:"users"`
}

// ActiveStats is the /admin/stats/active payload. Day granularity reports
// DAU for Date itself; week = [Date-6, Date], month = [Date-29, Date]
// (rolling windows over client-local local_date).
type ActiveStats struct {
	Date        string           `json:"date"`
	From        string           `json:"from"`
	To          string           `json:"to"`
	Granularity string           `json:"granularity"`
	GroupBy     string           `json:"group_by,omitempty"`
	Total       int              `json:"total"`
	Groups      []ActiveStatsRow `json:"groups,omitempty"`
}

// UsageDurationRow is one period's aggregate in /admin/stats/usage-duration.
// Period is YYYY-MM-DD for granularity=day, YYYY-MM for granularity=month.
type UsageDurationRow struct {
	Period         string `json:"period"`
	Group          string `json:"group,omitempty"`
	TotalSeconds   int64  `json:"total_seconds"`
	ActiveUsers    int    `json:"active_users"`
	PerUserSeconds int64  `json:"per_user_seconds"`
}

// NewUsersRow is one day's signup count in /admin/stats/new-users. Date is
// derived from users.created_at in the SERVER's timezone — unlike the
// local_date-based metrics, which follow the client-local calendar.
type NewUsersRow struct {
	Date  string `json:"date"`
	Users int    `json:"users"`
}
