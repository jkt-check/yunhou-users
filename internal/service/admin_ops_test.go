package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/repo"
)

func TestOpsWindowBounds(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	t.Run("day week month boundaries in Asia/Shanghai", func(t *testing.T) {
		// 2026-09-24 03:00 UTC = 2026-09-24 11:00 +08 (a Thursday).
		now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
		day, week, month := opsWindowBounds(now, shanghai)

		// Today 00:00 +08 = 2026-09-23 16:00 UTC.
		if want := time.Date(2026, 9, 23, 16, 0, 0, 0, time.UTC); !day.Equal(want) {
			t.Fatalf("day = %s, want %s", day, want)
		}
		// Week starts Monday 2026-09-21 00:00 +08 = 2026-09-20 16:00 UTC.
		if want := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC); !week.Equal(want) {
			t.Fatalf("week = %s, want %s", week, want)
		}
		// Month starts 2026-09-01 00:00 +08 = 2026-08-31 16:00 UTC.
		if want := time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC); !month.Equal(want) {
			t.Fatalf("month = %s, want %s", month, want)
		}
	})

	t.Run("sunday belongs to the week that started the previous monday", func(t *testing.T) {
		// 2026-09-27 is a Sunday; the week bucket must start 2026-09-21,
		// matching PG date_trunc('week').
		now := time.Date(2026, 9, 27, 12, 0, 0, 0, shanghai)
		_, week, _ := opsWindowBounds(now, shanghai)
		if want := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC); !week.Equal(want) {
			t.Fatalf("week = %s, want %s", week, want)
		}
	})

	t.Run("monday starts a new week", func(t *testing.T) {
		now := time.Date(2026, 9, 28, 0, 30, 0, 0, shanghai) // Monday 00:30 +08
		day, week, _ := opsWindowBounds(now, shanghai)
		if want := time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC); !day.Equal(want) {
			t.Fatalf("day = %s, want %s", day, want)
		}
		if !week.Equal(day) {
			t.Fatalf("week = %s, want == day %s on Monday", week, day)
		}
	})

	t.Run("utc tz keeps utc boundaries", func(t *testing.T) {
		now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
		day, _, month := opsWindowBounds(now, time.UTC)
		if want := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC); !day.Equal(want) {
			t.Fatalf("day = %s, want %s", day, want)
		}
		if want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); !month.Equal(want) {
			t.Fatalf("month = %s, want %s", month, want)
		}
	})
}

func TestAdminOpsMetricsService(t *testing.T) {
	t.Run("invalid tz is ErrAdminInvalidParam", func(t *testing.T) {
		svc := NewAdminOpsService(&fakeAdminUsersRepo{})
		_, err := svc.Metrics(context.Background(), "Mars/Olympus")
		if !errors.Is(err, ErrAdminInvalidParam) {
			t.Fatalf("err: %v", err)
		}
	})

	t.Run("tz=Local is rejected (M-7)", func(t *testing.T) {
		// LoadLocation("Local") is accepted by the stdlib but resolves to
		// the container clock (UTC in practice) — an operator who thinks
		// they pinned a tz has silently got UTC. Must be a 400, not a
		// silent UTC result.
		fake := &fakeAdminUsersRepo{}
		svc := NewAdminOpsService(fake)
		_, err := svc.Metrics(context.Background(), "Local")
		var pe *AdminParamError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want AdminParamError", err)
		}
		if !fake.gotOpsBounds[0].IsZero() {
			t.Fatalf("rejected tz must not reach the repo: %+v", fake.gotOpsBounds)
		}
	})

	t.Run("explicit IANA tz and UTC are accepted", func(t *testing.T) {
		svc := NewAdminOpsService(&fakeAdminUsersRepo{})
		for _, tz := range []string{"Asia/Shanghai", "UTC", "America/New_York"} {
			if _, err := svc.Metrics(context.Background(), tz); err != nil {
				t.Fatalf("tz %q rejected: %v", tz, err)
			}
		}
	})

	t.Run("empty tz defaults to Asia/Shanghai", func(t *testing.T) {
		fake := &fakeAdminUsersRepo{}
		svc := NewAdminOpsService(fake)
		if _, err := svc.Metrics(context.Background(), ""); err != nil {
			t.Fatalf("err: %v", err)
		}
		// Bounds must be the Asia/Shanghai wall-clock boundaries for the
		// current instant; assert only the shape (day/week/month ordered,
		// non-zero, UTC).
		day, week, month := fake.gotOpsBounds[0], fake.gotOpsBounds[1], fake.gotOpsBounds[2]
		if day.IsZero() || week.IsZero() || month.IsZero() {
			t.Fatalf("bounds: %+v", fake.gotOpsBounds)
		}
		if week.After(day) || month.After(day) {
			t.Fatalf("bounds out of order: %+v", fake.gotOpsBounds)
		}
		if day.Location() != time.UTC {
			t.Fatalf("bounds must be UTC: %+v", day)
		}
	})

	t.Run("assembly maps repo buckets", func(t *testing.T) {
		fake := &fakeAdminUsersRepo{
			usersCounts: repo.AdminOpsCounts{Total: 100, Today: 1, Week: 2, Month: 3},
			paidCum:     repo.AdminOpsCounts{Total: 10},
			paidAct:     repo.AdminOpsCounts{Total: 8, Month: 1},
			revenue:     repo.AdminOpsAmounts{Total: 1999.5, Today: 19.9},
		}
		svc := NewAdminOpsService(fake)
		m, err := svc.Metrics(context.Background(), "UTC")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if m.Users.Total != 100 || m.Users.Month != 3 {
			t.Fatalf("users: %+v", m.Users)
		}
		if m.PaidUsers.Cumulative.Total != 10 || m.PaidUsers.Active.Total != 8 {
			t.Fatalf("paidUsers: %+v", m.PaidUsers)
		}
		if m.Revenue.Total != 1999.5 || m.Revenue.Today != 19.9 {
			t.Fatalf("revenue: %+v", m.Revenue)
		}
	})

	t.Run("repo error propagates", func(t *testing.T) {
		svc := NewAdminOpsService(&fakeAdminUsersRepo{opsErr: errors.New("boom")})
		if _, err := svc.Metrics(context.Background(), "UTC"); err == nil {
			t.Fatal("want error")
		}
	})
}
