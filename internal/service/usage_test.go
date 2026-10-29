package service

import (
	"context"
	"errors"
	"testing"

	"github.com/yunhou/users/internal/model"
)

// mockUsageRepo records the arguments of the last call and returns canned
// results — the SQL itself is exercised by the repo-layer tests against a
// real Postgres; here we test window arithmetic and parameter validation.
type mockUsageRepo struct {
	inserted    int
	insertErr   error
	activeTotal int
	activeErr   error
	groups      []model.ActiveStatsRow
	duration    []model.UsageDurationRow
	newUsers    []model.NewUsersRow

	gotFrom, gotTo  string
	gotGranularity  string
	gotGroupBy      string
	gotTimezone     string
	insertCallCount int
}

func (m *mockUsageRepo) InsertBeats(_ context.Context, _, _ string, _ []model.UsageBeat) (int, error) {
	m.insertCallCount++
	return m.inserted, m.insertErr
}

func (m *mockUsageRepo) CountActiveUsers(_ context.Context, from, to, timezone string) (int, error) {
	m.gotFrom, m.gotTo, m.gotTimezone = from, to, timezone
	return m.activeTotal, m.activeErr
}

func (m *mockUsageRepo) CountActiveUsersGrouped(_ context.Context, from, to, groupBy, timezone string) ([]model.ActiveStatsRow, error) {
	m.gotFrom, m.gotTo, m.gotGroupBy, m.gotTimezone = from, to, groupBy, timezone
	return m.groups, nil
}

func (m *mockUsageRepo) UsageDurationStats(_ context.Context, from, to, granularity, groupBy, timezone string) ([]model.UsageDurationRow, error) {
	m.gotFrom, m.gotTo, m.gotGranularity, m.gotGroupBy, m.gotTimezone = from, to, granularity, groupBy, timezone
	return m.duration, nil
}

func (m *mockUsageRepo) CountNewUsers(_ context.Context, from, to, timezone string) ([]model.NewUsersRow, error) {
	m.gotFrom, m.gotTo, m.gotTimezone = from, to, timezone
	return m.newUsers, nil
}

func TestUsageService_ActiveStatsWindows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		granularity string
		wantFrom    string
		wantTo      string
	}{
		{"day", "2026-09-04", "2026-09-04"},
		// 7 / 30 个自然日的闭区间窗口(含 date 当天):date-6 / date-29
		{"week", "2026-08-29", "2026-09-04"},
		{"month", "2026-08-06", "2026-09-04"},
	}
	for _, tc := range cases {
		t.Run(tc.granularity, func(t *testing.T) {
			repo := &mockUsageRepo{activeTotal: 7}
			svc := NewUsageService(repo)
			stats, err := svc.ActiveStats(context.Background(), "2026-09-04", tc.granularity, "", UsageDefaultTimezone)
			if err != nil {
				t.Fatalf("ActiveStats: %v", err)
			}
			if stats.Total != 7 {
				t.Errorf("Total = %d, want 7", stats.Total)
			}
			if stats.From != tc.wantFrom || stats.To != tc.wantTo {
				t.Errorf("window = [%s, %s], want [%s, %s]", stats.From, stats.To, tc.wantFrom, tc.wantTo)
			}
			if repo.gotFrom != tc.wantFrom || repo.gotTo != tc.wantTo {
				t.Errorf("repo got [%s, %s], want [%s, %s]", repo.gotFrom, repo.gotTo, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestUsageService_ActiveStatsGrouped(t *testing.T) {
	t.Parallel()

	repo := &mockUsageRepo{groups: []model.ActiveStatsRow{
		{Group: "macos", Users: 5},
		{Group: "windows", Users: 3},
	}}
	svc := NewUsageService(repo)
	stats, err := svc.ActiveStats(context.Background(), "2026-09-04", "day", "platform", UsageDefaultTimezone)
	if err != nil {
		t.Fatalf("ActiveStats: %v", err)
	}
	// Total is the SUM of group rows (a user on two platforms counts twice —
	// documented group_by semantics, not a bug).
	if stats.Total != 8 {
		t.Errorf("Total = %d, want 8", stats.Total)
	}
	if len(stats.Groups) != 2 || stats.Groups[0].Group != "macos" {
		t.Errorf("Groups = %+v", stats.Groups)
	}
	if repo.gotGroupBy != "platform" {
		t.Errorf("repo groupBy = %q, want platform", repo.gotGroupBy)
	}
}

func TestUsageService_ActiveStatsValidation(t *testing.T) {
	t.Parallel()

	svc := NewUsageService(&mockUsageRepo{})
	for _, args := range [][3]string{
		{"not-a-date", "day", ""},
		{"2026-02-30", "day", ""},
		{"2026-09-04", "year", ""},
		{"2026-09-04", "day", "nickname"}, // PII dimension must never be groupable
	} {
		_, err := svc.ActiveStats(context.Background(), args[0], args[1], args[2], UsageDefaultTimezone)
		if !errors.Is(err, ErrUsageInvalidParam) {
			t.Errorf("ActiveStats(%v): err = %v, want ErrUsageInvalidParam", args, err)
		}
	}
}

func TestUsageService_DurationStats(t *testing.T) {
	t.Parallel()

	repo := &mockUsageRepo{duration: []model.UsageDurationRow{
		{Period: "2026-09-03", TotalSeconds: 1000, ActiveUsers: 4},
		{Period: "2026-09-04", TotalSeconds: 300, ActiveUsers: 0}, // defensive: zero-users row
	}}
	svc := NewUsageService(repo)
	rows, err := svc.DurationStats(context.Background(), "2026-09-03", "2026-09-04", "day", "", UsageDefaultTimezone)
	if err != nil {
		t.Fatalf("DurationStats: %v", err)
	}
	if rows[0].PerUserSeconds != 250 {
		t.Errorf("PerUserSeconds = %d, want 250", rows[0].PerUserSeconds)
	}
	if rows[1].PerUserSeconds != 0 {
		t.Errorf("zero-users PerUserSeconds = %d, want 0 (no div-by-zero)", rows[1].PerUserSeconds)
	}
	if repo.gotGranularity != "day" {
		t.Errorf("repo granularity = %q, want day", repo.gotGranularity)
	}
}

func TestUsageService_DurationStatsValidation(t *testing.T) {
	t.Parallel()

	svc := NewUsageService(&mockUsageRepo{})
	for _, args := range [][4]string{
		{"2026-09-04", "2026-09-03", "day", ""},      // to before from
		{"2026-09-01", "2026-09-04", "hour", ""},     // bad granularity
		{"2026-09-01", "2026-09-04", "day", "email"}, // bad group_by
		{"bad", "2026-09-04", "day", ""},             // bad from
		{"2025-01-01", "2026-09-04", "day", ""},      // range > 366 days
	} {
		_, err := svc.DurationStats(context.Background(), args[0], args[1], args[2], args[3], UsageDefaultTimezone)
		if !errors.Is(err, ErrUsageInvalidParam) {
			t.Errorf("DurationStats(%v): err = %v, want ErrUsageInvalidParam", args, err)
		}
	}
}

func TestUsageService_NewUsersStats(t *testing.T) {
	t.Parallel()

	repo := &mockUsageRepo{newUsers: []model.NewUsersRow{{Date: "2026-09-04", Users: 3}}}
	svc := NewUsageService(repo)
	rows, err := svc.NewUsersStats(context.Background(), "2026-09-01", "2026-09-04", UsageDefaultTimezone)
	if err != nil {
		t.Fatalf("NewUsersStats: %v", err)
	}
	if len(rows) != 1 || rows[0].Users != 3 {
		t.Errorf("rows = %+v", rows)
	}
	if repo.gotFrom != "2026-09-01" || repo.gotTo != "2026-09-04" {
		t.Errorf("repo got [%s, %s]", repo.gotFrom, repo.gotTo)
	}

	if _, err := svc.NewUsersStats(context.Background(), "2026-09-04", "2026-09-01", UsageDefaultTimezone); !errors.Is(err, ErrUsageInvalidParam) {
		t.Errorf("reversed range: err = %v, want ErrUsageInvalidParam", err)
	}
}

func TestUsageService_RecordBeatsPassthrough(t *testing.T) {
	t.Parallel()

	repo := &mockUsageRepo{inserted: 2}
	svc := NewUsageService(repo)
	n, err := svc.RecordBeats(context.Background(), "user-1", "yunhou-website", []model.UsageBeat{{}, {}})
	if err != nil {
		t.Fatalf("RecordBeats: %v", err)
	}
	if n != 2 || repo.insertCallCount != 1 {
		t.Errorf("inserted = %d, calls = %d; want 2 and 1", n, repo.insertCallCount)
	}
}

func TestUsageService_StatsTimezone(t *testing.T) {
	t.Parallel()

	repo := &mockUsageRepo{}
	svc := NewUsageService(repo)
	if _, err := svc.ActiveStats(context.Background(), "2026-09-04", "day", "", "Asia/Shanghai"); err != nil {
		t.Fatalf("ActiveStats with valid timezone: %v", err)
	}
	if repo.gotTimezone != "Asia/Shanghai" {
		t.Errorf("repo timezone = %q, want Asia/Shanghai", repo.gotTimezone)
	}

	for name, call := range map[string]func() error{
		"active": func() error {
			_, err := svc.ActiveStats(context.Background(), "2026-09-04", "day", "", "Invalid/Zone")
			return err
		},
		"duration": func() error {
			_, err := svc.DurationStats(context.Background(), "2026-09-01", "2026-09-04", "day", "", "Invalid/Zone")
			return err
		},
		"new users": func() error {
			_, err := svc.NewUsersStats(context.Background(), "2026-09-01", "2026-09-04", "Invalid/Zone")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrUsageInvalidParam) {
				t.Errorf("invalid timezone error = %v, want ErrUsageInvalidParam", err)
			}
		})
	}

	for _, timezone := range []string{"", "Local"} {
		if _, err := svc.ActiveStats(context.Background(), "2026-09-04", "day", "", timezone); !errors.Is(err, ErrUsageInvalidParam) {
			t.Errorf("timezone %q error = %v, want ErrUsageInvalidParam", timezone, err)
		}
	}
}
