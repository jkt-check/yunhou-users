package service

import (
	"context"
	"fmt"
	"time"

	"github.com/yunhou/users/internal/repo"
)

// AdminOpsService backs GET /admin/ops/metrics (dashboard-admin-api spec
// §2). It resolves the requested IANA timezone into UTC boundary instants
// (day = tz-local 00:00, week = tz-local Monday 00:00 matching PG
// date_trunc('week'), month = tz-local 1st 00:00) and delegates the four
// count/sum queries to the repo. SQL never does timezone conversion.
type AdminOpsService struct {
	repo repo.AdminUsersRepo
}

func NewAdminOpsService(r repo.AdminUsersRepo) *AdminOpsService {
	return &AdminOpsService{repo: r}
}

// AdminOpsDefaultTZ is the spec default for the tz query param.
const AdminOpsDefaultTZ = "Asia/Shanghai"

// AdminOpsCountBucket is the total/today/week/month JSON shape shared by
// the three count metrics.
type AdminOpsCountBucket struct {
	Total int64 `json:"total"`
	Today int64 `json:"today"`
	Week  int64 `json:"week"`
	Month int64 `json:"month"`
}

// AdminOpsRevenueBucket is the same shape for revenue (元).
type AdminOpsRevenueBucket struct {
	Total float64 `json:"total"`
	Today float64 `json:"today"`
	Week  float64 `json:"week"`
	Month float64 `json:"month"`
}

// AdminOpsMetrics is the response data payload of GET /admin/ops/metrics.
// No downloads field — nginx-log download counts stay on the SSH path.
type AdminOpsMetrics struct {
	Users     AdminOpsCountBucket `json:"users"`
	PaidUsers struct {
		Cumulative AdminOpsCountBucket `json:"cumulative"`
		Active     AdminOpsCountBucket `json:"active"`
	} `json:"paidUsers"`
	Revenue AdminOpsRevenueBucket `json:"revenue"`
}

// Metrics validates tz (time.LoadLocation) and assembles the metric
// buckets. An unloadable tz is a 400 (AdminParamError), never a 500.
func (s *AdminOpsService) Metrics(ctx context.Context, tz string) (*AdminOpsMetrics, error) {
	if tz == "" {
		tz = AdminOpsDefaultTZ
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, &AdminParamError{Reason: fmt.Sprintf("invalid tz: %q", tz)}
	}

	dayStart, weekStart, monthStart := opsWindowBounds(time.Now(), loc)

	users, err := s.repo.OpsUserCounts(ctx, dayStart, weekStart, monthStart)
	if err != nil {
		return nil, fmt.Errorf("ops users: %w", err)
	}
	paidCum, err := s.repo.OpsPaidUsersCumulative(ctx, dayStart, weekStart, monthStart)
	if err != nil {
		return nil, fmt.Errorf("ops paid users cumulative: %w", err)
	}
	paidAct, err := s.repo.OpsPaidUsersActive(ctx, dayStart, weekStart, monthStart)
	if err != nil {
		return nil, fmt.Errorf("ops paid users active: %w", err)
	}
	revenue, err := s.repo.OpsRevenue(ctx, dayStart, weekStart, monthStart)
	if err != nil {
		return nil, fmt.Errorf("ops revenue: %w", err)
	}

	out := &AdminOpsMetrics{
		Users:   AdminOpsCountBucket(users),
		Revenue: AdminOpsRevenueBucket(revenue),
	}
	out.PaidUsers.Cumulative = AdminOpsCountBucket(paidCum)
	out.PaidUsers.Active = AdminOpsCountBucket(paidAct)
	return out, nil
}

// opsWindowBounds computes the today/this-week/this-month start instants
// in loc and returns them as UTC. Week starts Monday, matching PG
// date_trunc('week') in the dashboard's original SQL.
func opsWindowBounds(now time.Time, loc *time.Location) (dayStart, weekStart, monthStart time.Time) {
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	// time.Weekday: Sunday=0..Saturday=6; shift to Monday=0..Sunday=6 so the
	// offset back to Monday is just the shifted weekday.
	mondayOffset := (int(local.Weekday()) + 6) % 7
	week := day.AddDate(0, 0, -mondayOffset)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	return day.UTC(), week.UTC(), month.UTC()
}
