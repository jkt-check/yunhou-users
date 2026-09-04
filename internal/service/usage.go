package service

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// UsageService backs the usage-analytics feature
// (2026-09-04-usage-analytics-design.md): login-user heartbeats (5min
// cadence, offline-buffered, idempotent) and the three admin stats reads.
// Field-level beat validation happens at the handler boundary; this layer
// owns stats-parameter validation (dates, granularity, group_by) and the
// DAU/WAU/MAU window arithmetic.
type UsageService struct {
	usageRepo repo.UsageRepo
}

func NewUsageService(usageRepo repo.UsageRepo) *UsageService {
	return &UsageService{usageRepo: usageRepo}
}

// usageDateLayout is the client-local calendar date format (local_date).
const usageDateLayout = "2006-01-02"

// usageMaxRangeDays bounds any stats query window so a typo'd range can't
// turn into a full-table scan over the raw events table (MVP aggregates
// straight off usage_events — no rollup table yet).
const usageMaxRangeDays = 366

// RecordBeats persists one heartbeat batch. Returns the number of rows
// actually written (duplicates swallowed by the idempotency constraint are
// excluded). Identity (userID/appID) comes from the JWT, never the body.
func (s *UsageService) RecordBeats(ctx context.Context, userID, appID string, beats []model.UsageBeat) (int, error) {
	return s.usageRepo.InsertBeats(ctx, userID, appID, beats)
}

// parseUsageDate validates a YYYY-MM-DD calendar date. time.Parse rejects
// impossible dates (2026-02-30) already.
func parseUsageDate(s string) (time.Time, error) {
	d, err := time.Parse(usageDateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: date %q must be YYYY-MM-DD", ErrUsageInvalidParam, s)
	}
	return d, nil
}

func formatUsageDate(t time.Time) string { return t.Format(usageDateLayout) }

// usageWindow resolves granularity into the [from, to] local_date window:
// day = date itself (DAU), week = [date-6, date] (WAU), month = [date-29,
// date] (MAU). Windows are inclusive calendar-day ranges.
func usageWindow(date time.Time, granularity string) (from, to string, err error) {
	to = formatUsageDate(date)
	switch granularity {
	case "day":
		return to, to, nil
	case "week":
		return formatUsageDate(date.AddDate(0, 0, -6)), to, nil
	case "month":
		return formatUsageDate(date.AddDate(0, 0, -29)), to, nil
	default:
		return "", "", fmt.Errorf("%w: granularity must be day|week|month", ErrUsageInvalidParam)
	}
}

// validateUsageGroupBy whitelists the group_by dimension. The value becomes
// a SQL column name in the repo layer, so it must be a closed enum.
func validateUsageGroupBy(groupBy string) error {
	if groupBy == "" || slices.Contains([]string{"platform", "app_version"}, groupBy) {
		return nil
	}
	return fmt.Errorf("%w: group_by must be platform|app_version", ErrUsageInvalidParam)
}

// validateUsageRange parses [from, to], enforces order, and caps the span.
func validateUsageRange(from, to string) (time.Time, time.Time, error) {
	f, err := parseUsageDate(from)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	t, err := parseUsageDate(to)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if t.Before(f) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: to must not be before from", ErrUsageInvalidParam)
	}
	if t.Sub(f) > usageMaxRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: range must not exceed %d days", ErrUsageInvalidParam, usageMaxRangeDays)
	}
	return f, t, nil
}

// ActiveStats answers /admin/stats/active: distinct login users with ≥1
// heartbeat in the window, optionally broken down by platform/app_version.
func (s *UsageService) ActiveStats(ctx context.Context, date, granularity, groupBy string) (*model.ActiveStats, error) {
	d, err := parseUsageDate(date)
	if err != nil {
		return nil, err
	}
	if err := validateUsageGroupBy(groupBy); err != nil {
		return nil, err
	}
	from, to, err := usageWindow(d, granularity)
	if err != nil {
		return nil, err
	}

	out := &model.ActiveStats{
		Date:        formatUsageDate(d),
		From:        from,
		To:          to,
		Granularity: granularity,
		GroupBy:     groupBy,
	}
	if groupBy == "" {
		total, err := s.usageRepo.CountActiveUsers(ctx, from, to)
		if err != nil {
			return nil, err
		}
		out.Total = total
		return out, nil
	}
	groups, err := s.usageRepo.CountActiveUsersGrouped(ctx, from, to, groupBy)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		out.Total += g.Users
	}
	out.Groups = groups
	return out, nil
}

// DurationStats answers /admin/stats/usage-duration: SUM(active_seconds)
// and distinct users per day or month in [from, to], optionally grouped.
// PerUserSeconds is total/users — average app-alive seconds per active user
// in the period.
func (s *UsageService) DurationStats(ctx context.Context, from, to, granularity, groupBy string) ([]model.UsageDurationRow, error) {
	if _, _, err := validateUsageRange(from, to); err != nil {
		return nil, err
	}
	if granularity != "day" && granularity != "month" {
		return nil, fmt.Errorf("%w: granularity must be day|month", ErrUsageInvalidParam)
	}
	if err := validateUsageGroupBy(groupBy); err != nil {
		return nil, err
	}
	rows, err := s.usageRepo.UsageDurationStats(ctx, from, to, granularity, groupBy)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ActiveUsers > 0 {
			rows[i].PerUserSeconds = rows[i].TotalSeconds / int64(rows[i].ActiveUsers)
		}
	}
	return rows, nil
}

// NewUsersStats answers /admin/stats/new-users: signups per calendar day in
// [from, to], bucketed by users.created_at in the server's timezone (note
// the deliberately different day boundary from the local_date metrics).
func (s *UsageService) NewUsersStats(ctx context.Context, from, to string) ([]model.NewUsersRow, error) {
	if _, _, err := validateUsageRange(from, to); err != nil {
		return nil, err
	}
	return s.usageRepo.CountNewUsers(ctx, from, to)
}
