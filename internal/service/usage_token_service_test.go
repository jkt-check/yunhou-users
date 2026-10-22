package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yunhou/users/internal/model"
)

// mockUsageTokenRepo 用手写函数字段实现 repo.UsageTokenRepo（项目惯例：
// 不用外部 mock 库）。
type mockUsageTokenRepo struct {
	ingestFn       func(ctx context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error)
	summaryFn      func(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter) (*model.UsageTokenSummary, error)
	breakdownFn    func(ctx context.Context, userID, bucketSize, dimension string, from, to time.Time, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, error)
	timeseriesFn   func(ctx context.Context, userID, bucketSize, interval, groupBy, tz string, from, to time.Time, f model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error)
	coverageFn     func(ctx context.Context, userID, bucketSize string) ([]model.UsageDeviceView, error)
	exportFn       func(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter, rowCap int) ([]model.UsageExportRow, int64, error)
	deleteAllFn    func(ctx context.Context, userID string, clearedBefore time.Time) (int64, error)
	deleteDeviceFn func(ctx context.Context, userID, deviceID string, clearedBefore time.Time) (int64, error)
	deleteWindowFn func(ctx context.Context, userID string, deviceID *string, fromBucket, toBucket time.Time) (int64, error)
	upsertDeviceFn func(ctx context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDevice, error)
	listDevicesFn  func(ctx context.Context, userID string) ([]model.UsageDevice, error)
	updateLabelFn  func(ctx context.Context, userID, deviceID string, label *string) error
	revokeFn       func(ctx context.Context, userID, deviceID string, purge bool) error
	sweepFn        func(ctx context.Context, cutoff time.Time) (int64, int64, error)
}

func (m *mockUsageTokenRepo) IngestBuckets(ctx context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error) {
	return m.ingestFn(ctx, userID, in)
}
func (m *mockUsageTokenRepo) Summary(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter) (*model.UsageTokenSummary, error) {
	return m.summaryFn(ctx, userID, bucketSize, from, to, f)
}
func (m *mockUsageTokenRepo) Breakdown(ctx context.Context, userID, bucketSize, dimension string, from, to time.Time, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, error) {
	return m.breakdownFn(ctx, userID, bucketSize, dimension, from, to, f, limit)
}
func (m *mockUsageTokenRepo) Timeseries(ctx context.Context, userID, bucketSize, interval, groupBy, tz string, from, to time.Time, f model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error) {
	return m.timeseriesFn(ctx, userID, bucketSize, interval, groupBy, tz, from, to, f)
}
func (m *mockUsageTokenRepo) CoverageDevices(ctx context.Context, userID, bucketSize string) ([]model.UsageDeviceView, error) {
	return m.coverageFn(ctx, userID, bucketSize)
}
func (m *mockUsageTokenRepo) ExportRows(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter, rowCap int, decide func(total int64) (bool, error)) ([]model.UsageExportRow, int64, error) {
	rows, total, err := m.exportFn(ctx, userID, bucketSize, from, to, f, rowCap)
	if err != nil {
		return nil, 0, err
	}
	// 与真实 repo 同语义：decide 在取行之前判定，报错/放弃则不返回行。
	fetch, derr := decide(total)
	if derr != nil {
		return nil, total, derr
	}
	if !fetch {
		return nil, total, nil
	}
	return rows, total, nil
}
func (m *mockUsageTokenRepo) DeleteAll(ctx context.Context, userID string, clearedBefore time.Time) (int64, error) {
	return m.deleteAllFn(ctx, userID, clearedBefore)
}
func (m *mockUsageTokenRepo) DeleteByDevice(ctx context.Context, userID, deviceID string, clearedBefore time.Time) (int64, error) {
	return m.deleteDeviceFn(ctx, userID, deviceID, clearedBefore)
}
func (m *mockUsageTokenRepo) DeleteWindow(ctx context.Context, userID string, deviceID *string, fromBucket, toBucket time.Time) (int64, error) {
	return m.deleteWindowFn(ctx, userID, deviceID, fromBucket, toBucket)
}
func (m *mockUsageTokenRepo) UpsertDevice(ctx context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDevice, error) {
	return m.upsertDeviceFn(ctx, userID, in)
}
func (m *mockUsageTokenRepo) ListDevices(ctx context.Context, userID string) ([]model.UsageDevice, error) {
	return m.listDevicesFn(ctx, userID)
}
func (m *mockUsageTokenRepo) UpdateDeviceLabel(ctx context.Context, userID, deviceID string, label *string) error {
	return m.updateLabelFn(ctx, userID, deviceID, label)
}
func (m *mockUsageTokenRepo) RevokeDevice(ctx context.Context, userID, deviceID string, purge bool) error {
	return m.revokeFn(ctx, userID, deviceID, purge)
}
func (m *mockUsageTokenRepo) SweepRetention(ctx context.Context, cutoff time.Time) (int64, int64, error) {
	return m.sweepFn(ctx, cutoff)
}

// usageTokenErrCode 取出契约错误码；非契约错误返回 ""。
func usageTokenErrCode(t *testing.T, err error) string {
	t.Helper()
	var ute *model.UsageTokenError
	if !errors.As(err, &ute) {
		t.Fatalf("err = %v (%T), want *model.UsageTokenError", err, err)
	}
	return ute.Code
}

func newUsageTokenSvc(r *mockUsageTokenRepo) *UsageTokenService {
	return NewUsageTokenService(r, model.UsageBucketSizeHour, 180, 500000)
}

func TestParseUsageTokenWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 8, 5, 0, 0, time.UTC)

	t.Run("defaults to last 90 days", func(t *testing.T) {
		from, to, err := ParseUsageTokenWindow("", "", now)
		if err != nil {
			t.Fatal(err)
		}
		if to.Format("2006-01-02") != "2026-10-22" {
			t.Errorf("to = %s", to)
		}
		if from.Format("2006-01-02") != "2026-07-25" { // 今天-89 天 = 共 90 天
			t.Errorf("from = %s, want 2026-07-25", from.Format("2006-01-02"))
		}
	})
	t.Run("explicit window", func(t *testing.T) {
		from, to, err := ParseUsageTokenWindow("2026-10-01", "2026-10-22", now)
		if err != nil {
			t.Fatal(err)
		}
		if from.Format("2006-01-02") != "2026-10-01" || to.Format("2006-01-02") != "2026-10-22" {
			t.Errorf("from=%s to=%s", from, to)
		}
	})
	t.Run("window over 180 days", func(t *testing.T) {
		_, _, err := ParseUsageTokenWindow("2026-01-01", "2026-07-01", now)
		if usageTokenErrCode(t, err) != "window_too_large" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
	t.Run("exactly 180 natural days is allowed", func(t *testing.T) {
		if _, _, err := ParseUsageTokenWindow("2026-04-26", "2026-10-22", now); err != nil {
			t.Errorf("179-day diff (180 natural days) must pass: %v", err)
		}
	})
	t.Run("181 natural days rejected", func(t *testing.T) {
		_, _, err := ParseUsageTokenWindow("2026-04-25", "2026-10-22", now)
		if usageTokenErrCode(t, err) != "window_too_large" {
			t.Errorf("180-day diff (181 natural days) must be rejected, code = %q", usageTokenErrCode(t, err))
		}
	})
	t.Run("to before from", func(t *testing.T) {
		_, _, err := ParseUsageTokenWindow("2026-10-22", "2026-10-01", now)
		if usageTokenErrCode(t, err) != "invalid_request" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
	t.Run("bad format", func(t *testing.T) {
		_, _, err := ParseUsageTokenWindow("10/01/2026", "", now)
		if usageTokenErrCode(t, err) != "invalid_request" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
}

// TestUsageTokenSummary_PassesFixedFilters 验证固定三过滤中的两条在
// service 落点：bucket_size=部署配置、半开区间上界 = to+1 天（§4.1）。
func TestUsageTokenSummary_PassesFixedFilters(t *testing.T) {
	t.Parallel()
	var gotBucketSize string
	var gotFrom, gotTo time.Time
	r := &mockUsageTokenRepo{
		summaryFn: func(_ context.Context, _, bucketSize string, from, to time.Time, _ model.UsageTokenFilter) (*model.UsageTokenSummary, error) {
			gotBucketSize, gotFrom, gotTo = bucketSize, from, to
			return &model.UsageTokenSummary{}, nil
		},
	}
	svc := newUsageTokenSvc(r)
	if _, err := svc.Summary(context.Background(), "u1", "2026-10-20", "2026-10-22", model.UsageTokenFilter{Source: "all"}); err != nil {
		t.Fatal(err)
	}
	if gotBucketSize != "hour" {
		t.Errorf("bucketSize = %q, want hour", gotBucketSize)
	}
	if gotFrom.Format(time.RFC3339) != "2026-10-20T00:00:00Z" || gotTo.Format(time.RFC3339) != "2026-10-23T00:00:00Z" {
		t.Errorf("half-open window = [%s, %s)", gotFrom, gotTo)
	}
}

func TestUsageTokenSummary_BadSource(t *testing.T) {
	t.Parallel()
	svc := newUsageTokenSvc(&mockUsageTokenRepo{})
	_, err := svc.Summary(context.Background(), "u1", "", "", model.UsageTokenFilter{Source: "api"})
	if usageTokenErrCode(t, err) != "invalid_request" {
		t.Errorf("code = %q", usageTokenErrCode(t, err))
	}
}

// TestUsageTokenBreakdown_ValidationAndDisplayName 覆盖 dimension 白名单、
// limit 钳制/截断、provider display_name 目录解析（§4.3）。
func TestUsageTokenBreakdown_ValidationAndDisplayName(t *testing.T) {
	t.Parallel()

	t.Run("invalid dimension", func(t *testing.T) {
		svc := newUsageTokenSvc(&mockUsageTokenRepo{})
		_, _, err := svc.Breakdown(context.Background(), "u1", "country", "", "", model.UsageTokenFilter{}, 0)
		if usageTokenErrCode(t, err) != "invalid_dimension" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})

	t.Run("limit defaults to 50 and clamps at 100", func(t *testing.T) {
		var gotLimit int
		r := &mockUsageTokenRepo{
			breakdownFn: func(_ context.Context, _, _, _ string, _, _ time.Time, _ model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, error) {
				gotLimit = limit
				return nil, nil
			},
		}
		svc := newUsageTokenSvc(r)
		if _, _, err := svc.Breakdown(context.Background(), "u1", "agent", "", "", model.UsageTokenFilter{}, 0); err != nil {
			t.Fatal(err)
		}
		if gotLimit != 51 { // service 拉 limit+1 探测截断
			t.Errorf("limit = %d, want 51 (default 50 + 1)", gotLimit)
		}
		if _, _, err := svc.Breakdown(context.Background(), "u1", "agent", "", "", model.UsageTokenFilter{}, 500); err != nil {
			t.Fatal(err)
		}
		if gotLimit != 101 {
			t.Errorf("limit = %d, want 101 (clamped 100 + 1)", gotLimit)
		}
	})

	t.Run("truncated flag and display names", func(t *testing.T) {
		str := func(s string) *string { return &s }
		// limit=1，repo 返回 2 行 → truncated，且只回 1 行。
		r := &mockUsageTokenRepo{
			breakdownFn: func(_ context.Context, _, _, dimension string, _, _ time.Time, _ model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, error) {
				return []model.UsageBreakdownItem{
					{ProviderKey: str("preset:deepseek"), ModelKey: str("deepseek-v4-pro")},
					{ProviderKey: str("builtin"), ModelKey: str("default")},
				}, nil
			},
		}
		svc := newUsageTokenSvc(r)
		items, truncated, err := svc.Breakdown(context.Background(), "u1", "provider_model", "", "", model.UsageTokenFilter{}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !truncated || len(items) != 1 {
			t.Errorf("truncated=%v len=%d, want true/1", truncated, len(items))
		}
		if items[0].DisplayName == nil || *items[0].DisplayName != "DeepSeek" {
			t.Errorf("display_name = %v, want DeepSeek (preset catalog)", items[0].DisplayName)
		}

		// 目录缺失回退 preset id；内置键固定中文标签（§2.4）。
		for providerKey, want := range map[string]string{
			"builtin":               "内置",
			"cli":                   "CLI",
			"custom":                "自定义",
			"preset:deepseek":       "DeepSeek",
			"preset:no-such-vendor": "no-such-vendor",
			"weird-key":             "weird-key",
		} {
			if got := UsageProviderDisplayName(providerKey); got != want {
				t.Errorf("UsageProviderDisplayName(%q) = %q, want %q", providerKey, got, want)
			}
		}
	})
}

// TestUsageTokenTimeseries_Validation 覆盖 §4.4：interval/group_by 白名单、
// tz 解析、day 部署的 tz 限制（§9.4 夹具 7）、hour 部署的 tz 窗口换算。
func TestUsageTokenTimeseries_Validation(t *testing.T) {
	t.Parallel()
	okRepo := &mockUsageTokenRepo{
		timeseriesFn: func(_ context.Context, _, _, _, _, _ string, _, _ time.Time, _ model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error) {
			return nil, nil
		},
	}

	t.Run("invalid interval", func(t *testing.T) {
		_, err := newUsageTokenSvc(okRepo).Timeseries(context.Background(), "u1", "year", "none", "UTC", "", "", model.UsageTokenFilter{})
		if usageTokenErrCode(t, err) != "invalid_interval" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
	t.Run("invalid group_by", func(t *testing.T) {
		_, err := newUsageTokenSvc(okRepo).Timeseries(context.Background(), "u1", "day", "model", "UTC", "", "", model.UsageTokenFilter{})
		if usageTokenErrCode(t, err) != "invalid_dimension" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
	t.Run("unparseable tz", func(t *testing.T) {
		_, err := newUsageTokenSvc(okRepo).Timeseries(context.Background(), "u1", "day", "none", "Mars/Olympus", "", "", model.UsageTokenFilter{})
		if usageTokenErrCode(t, err) != "invalid_request" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
	// §9.4 夹具 7：day 部署下 tz=Asia/Shanghai → tz_not_supported_for_day；
	// tz=UTC 正常。
	t.Run("day deployment rejects non-UTC tz", func(t *testing.T) {
		svc := NewUsageTokenService(okRepo, model.UsageBucketSizeDay, 180, 500000)
		_, err := svc.Timeseries(context.Background(), "u1", "day", "none", "Asia/Shanghai", "", "", model.UsageTokenFilter{})
		if usageTokenErrCode(t, err) != "tz_not_supported_for_day" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
	t.Run("day deployment accepts UTC", func(t *testing.T) {
		svc := NewUsageTokenService(okRepo, model.UsageBucketSizeDay, 180, 500000)
		res, err := svc.Timeseries(context.Background(), "u1", "day", "none", "UTC", "", "", model.UsageTokenFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if res.TZ != "UTC" {
			t.Errorf("tz = %q", res.TZ)
		}
	})
	// hour 部署：from/to 按 tz 本地日解释（§4.4）。Asia/Shanghai 的
	// 2026-10-20 本地日 = UTC [10-19T16:00, 10-20T16:00)。
	t.Run("hour deployment converts local days to UTC", func(t *testing.T) {
		var gotFrom, gotTo time.Time
		var gotTZ string
		r := &mockUsageTokenRepo{
			timeseriesFn: func(_ context.Context, _, _, _, _, tz string, from, to time.Time, _ model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error) {
				gotTZ, gotFrom, gotTo = tz, from, to
				return nil, nil
			},
		}
		svc := newUsageTokenSvc(r)
		_, err := svc.Timeseries(context.Background(), "u1", "day", "none", "Asia/Shanghai", "2026-10-20", "2026-10-20", model.UsageTokenFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if gotTZ != "Asia/Shanghai" {
			t.Errorf("tz = %q", gotTZ)
		}
		if gotFrom.Format(time.RFC3339) != "2026-10-19T16:00:00Z" || gotTo.Format(time.RFC3339) != "2026-10-20T16:00:00Z" {
			t.Errorf("window = [%s, %s), want [2026-10-19T16:00:00Z, 2026-10-20T16:00:00Z)", gotFrom, gotTo)
		}
	})
	t.Run("provider_model group gets display_name", func(t *testing.T) {
		r := &mockUsageTokenRepo{
			timeseriesFn: func(_ context.Context, _, _, _, _, _ string, _, _ time.Time, _ model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error) {
				pk := "preset:deepseek"
				mk := "deepseek-v4-pro"
				return []model.UsageTimeseriesItem{{Bucket: "2026-10-20", ProviderKey: &pk, ModelKey: &mk}}, nil
			},
		}
		res, err := newUsageTokenSvc(r).Timeseries(context.Background(), "u1", "day", "provider_model", "UTC", "", "", model.UsageTokenFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Items[0].DisplayName == nil || *res.Items[0].DisplayName != "DeepSeek" {
			t.Errorf("display_name = %v", res.Items[0].DisplayName)
		}
	})
}

// TestUsageTokenCoverage_Assembly 覆盖 §4.5：三配置字段、设备列表含
// revoked、顶层 earliest/latest 排除撤销设备。
func TestUsageTokenCoverage_Assembly(t *testing.T) {
	t.Parallel()
	early := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	revokedLate := late.Add(24 * time.Hour) // 撤销设备的更晚桶不得进汇总
	r := &mockUsageTokenRepo{
		coverageFn: func(_ context.Context, _, bucketSize string) ([]model.UsageDeviceView, error) {
			if bucketSize != "hour" {
				t.Errorf("bucketSize = %q", bucketSize)
			}
			return []model.UsageDeviceView{
				{DeviceID: "d_a", EarliestBucket: &early, LatestBucket: &late, Revoked: false},
				{DeviceID: "d_b", EarliestBucket: &early, LatestBucket: &revokedLate, Revoked: true},
			}, nil
		},
	}
	cov, err := newUsageTokenSvc(r).Coverage(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if cov.BucketSize != "hour" || cov.RetentionDays != 180 {
		t.Errorf("config fields = %q/%d", cov.BucketSize, cov.RetentionDays)
	}
	if cov.ExportMaxRows != 500000 {
		t.Errorf("export_max_rows = %d, want 500000 (v1.2 §4.5)", cov.ExportMaxRows)
	}
	if cov.ServerTime.IsZero() {
		t.Error("server_time must be set")
	}
	if cov.EarliestBucket == nil || !cov.EarliestBucket.Equal(early) {
		t.Errorf("earliest = %v", cov.EarliestBucket)
	}
	if cov.LatestBucket == nil || !cov.LatestBucket.Equal(late) {
		t.Errorf("latest = %v, want %v (revoked device excluded)", cov.LatestBucket, late)
	}
	if len(cov.Devices) != 2 || !cov.Devices[1].Revoked {
		t.Errorf("devices = %+v (revoked device must stay listed)", cov.Devices)
	}
}

// TestCeilToBucket 覆盖契约 §3.4 的 ceil_to_bucket：恰在边界取自身，否则
// 下一个边界；全程 UTC。
func TestCeilToBucket(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Time
		size string
		want time.Time
	}{
		{"hour exact", time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), "hour", time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)},
		{"hour mid", time.Date(2026, 10, 22, 9, 30, 15, 0, time.UTC), "hour", time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)},
		{"day exact", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), "day", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)},
		{"day mid", time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), "day", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)},
		{"non-UTC input normalized", time.Date(2026, 10, 22, 17, 30, 0, 0, time.FixedZone("CST", 8*3600)), "hour", time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CeilToBucket(tc.in, tc.size); !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Errorf("CeilToBucket(%v, %s) = %v, want %v (UTC)", tc.in, tc.size, got, tc.want)
			}
		})
	}
}

// TestUsageTokenDelete 覆盖 §4.7：全量/设备水位、窗口墓碑端点计算、窗口
// 上限、参数成对。
func TestUsageTokenDelete(t *testing.T) {
	t.Parallel()

	t.Run("full delete pushes user watermark", func(t *testing.T) {
		var gotCleared time.Time
		r := &mockUsageTokenRepo{
			deleteAllFn: func(_ context.Context, userID string, cleared time.Time) (int64, error) {
				if userID != "u1" {
					t.Errorf("userID = %q", userID)
				}
				gotCleared = cleared
				return 42, nil
			},
		}
		res, err := newUsageTokenSvc(r).Delete(context.Background(), "u1", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if res.DeletedBuckets != 42 || res.ClearedBefore == nil || res.Tombstone != nil {
			t.Errorf("res = %+v", res)
		}
		// cleared_before = ceil_to_bucket(now)：必须是未来不超过 1h 的整点。
		now := time.Now().UTC()
		if gotCleared.Before(now.Truncate(time.Hour)) || gotCleared.After(now.Truncate(time.Hour).Add(time.Hour)) {
			t.Errorf("cleared_before = %v, want ceil hour around %v", gotCleared, now)
		}
		if gotCleared.Minute() != 0 || gotCleared.Second() != 0 || gotCleared.Nanosecond() != 0 {
			t.Errorf("cleared_before not hour-aligned: %v", gotCleared)
		}
	})

	t.Run("device delete pushes device watermark", func(t *testing.T) {
		var gotDevice string
		r := &mockUsageTokenRepo{
			deleteDeviceFn: func(_ context.Context, _, deviceID string, _ time.Time) (int64, error) {
				gotDevice = deviceID
				return 7, nil
			},
		}
		res, err := newUsageTokenSvc(r).Delete(context.Background(), "u1", "d_x", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if gotDevice != "d_x" || res.ClearedBefore == nil {
			t.Errorf("device = %q res = %+v", gotDevice, res)
		}
	})

	t.Run("device delete missing -> sentinel", func(t *testing.T) {
		r := &mockUsageTokenRepo{
			deleteDeviceFn: func(_ context.Context, _, _ string, _ time.Time) (int64, error) {
				return 0, model.ErrUsageDeviceNotFound
			},
		}
		_, err := newUsageTokenSvc(r).Delete(context.Background(), "u1", "d_missing", "", "")
		if !IsUsageDeviceNotFound(err) {
			t.Errorf("err = %v, want ErrUsageDeviceNotFound", err)
		}
	})

	// 窗口墓碑端点（§4.7）：hour 部署 to_bucket = to 当天 23:00Z；day 部署
	// = to 00:00Z。
	t.Run("window tombstone bounds (hour)", func(t *testing.T) {
		var gotFrom, gotTo time.Time
		var gotDevice *string
		r := &mockUsageTokenRepo{
			deleteWindowFn: func(_ context.Context, _ string, deviceID *string, from, to time.Time) (int64, error) {
				gotDevice, gotFrom, gotTo = deviceID, from, to
				return 24, nil
			},
		}
		res, err := newUsageTokenSvc(r).Delete(context.Background(), "u1", "d_x", "2026-10-01", "2026-10-02")
		if err != nil {
			t.Fatal(err)
		}
		if gotFrom.Format(time.RFC3339) != "2026-10-01T00:00:00Z" || gotTo.Format(time.RFC3339) != "2026-10-02T23:00:00Z" {
			t.Errorf("tombstone = [%s, %s]", gotFrom, gotTo)
		}
		if gotDevice == nil || *gotDevice != "d_x" {
			t.Errorf("device = %v", gotDevice)
		}
		if res.ClearedBefore != nil || res.Tombstone == nil {
			t.Errorf("res = %+v (window delete must carry tombstone, not cleared_before)", res)
		}
	})

	t.Run("window tombstone bounds (day)", func(t *testing.T) {
		var gotTo time.Time
		r := &mockUsageTokenRepo{
			deleteWindowFn: func(_ context.Context, _ string, _ *string, _, to time.Time) (int64, error) {
				gotTo = to
				return 1, nil
			},
		}
		svc := NewUsageTokenService(r, model.UsageBucketSizeDay, 180, 500000)
		if _, err := svc.Delete(context.Background(), "u1", "", "2026-10-01", "2026-10-02"); err != nil {
			t.Fatal(err)
		}
		if gotTo.Format(time.RFC3339) != "2026-10-02T00:00:00Z" {
			t.Errorf("to_bucket = %s, want to 00:00Z for day deployment", gotTo)
		}
	})

	t.Run("window too large", func(t *testing.T) {
		_, err := newUsageTokenSvc(&mockUsageTokenRepo{}).Delete(context.Background(), "u1", "", "2026-01-01", "2026-07-01")
		if usageTokenErrCode(t, err) != "window_too_large" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})

	t.Run("from without to", func(t *testing.T) {
		_, err := newUsageTokenSvc(&mockUsageTokenRepo{}).Delete(context.Background(), "u1", "", "2026-10-01", "")
		if usageTokenErrCode(t, err) != "invalid_request" {
			t.Errorf("code = %q", usageTokenErrCode(t, err))
		}
	})
}

// TestUsageTokenRetentionSweeper 覆盖保留期 sweeper：cutoff = now -
// retentionDays，两类删除计数透传。
func TestUsageTokenRetentionSweeper(t *testing.T) {
	t.Parallel()
	var gotCutoff time.Time
	r := &mockUsageTokenRepo{
		sweepFn: func(_ context.Context, cutoff time.Time) (int64, int64, error) {
			gotCutoff = cutoff
			return 5, 2, nil
		},
	}
	s := NewUsageTokenRetentionSweeper(r, 180, 0)
	buckets, tombstones, err := s.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if buckets != 5 || tombstones != 2 {
		t.Errorf("counts = %d/%d", buckets, tombstones)
	}
	want := time.Now().Add(-180 * 24 * time.Hour)
	if gotCutoff.Before(want.Add(-time.Minute)) || gotCutoff.After(want.Add(time.Minute)) {
		t.Errorf("cutoff = %v, want ~%v", gotCutoff, want)
	}
}

// TestUsageTokenExport_FailClosed 覆盖 v1.2 §4.6 / §9.4.9-10 的 service 侧：
// total > cap 且未 allowTruncated → export_too_large（data 带 max_rows /
// from / to）；显式 allowTruncated → truncated=true、rows 为 repo 截断结果。
func TestUsageTokenExport_FailClosed(t *testing.T) {
	t.Parallel()
	repoWith := func(rows, total int) *mockUsageTokenRepo {
		return &mockUsageTokenRepo{
			exportFn: func(_ context.Context, _, _ string, _, _ time.Time, _ model.UsageTokenFilter, rowCap int) ([]model.UsageExportRow, int64, error) {
				if rowCap != 2 { // 本组测试用小 cap 实例
					t.Errorf("rowCap = %d, want 2", rowCap)
				}
				out := make([]model.UsageExportRow, rows)
				return out, int64(total), nil
			},
		}
	}
	svc := NewUsageTokenService(repoWith(2, 3), model.UsageBucketSizeHour, 180, 2)

	t.Run("over cap without allow_truncated -> export_too_large", func(t *testing.T) {
		_, total, _, err := svc.Export(context.Background(), "u1", "2026-10-20", "2026-10-22", model.UsageTokenFilter{}, false)
		if total != 3 {
			t.Errorf("total = %d, want 3 (同快照计数透传，供日志/诊断)", total)
		}
		var ute *model.UsageTokenError
		if !errors.As(err, &ute) {
			t.Fatalf("err = %v, want UsageTokenError", err)
		}
		if ute.Status != 400 || ute.Code != "export_too_large" {
			t.Errorf("status/code = %d/%q", ute.Status, ute.Code)
		}
		if ute.Data["max_rows"] != 2 || ute.Data["from"] != "2026-10-20" || ute.Data["to"] != "2026-10-22" {
			t.Errorf("data = %v, want max_rows/from/to", ute.Data)
		}
	})

	t.Run("over cap with allow_truncated -> truncated rows", func(t *testing.T) {
		rows, total, truncated, err := svc.Export(context.Background(), "u1", "2026-10-20", "2026-10-22", model.UsageTokenFilter{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !truncated || len(rows) != 2 || total != 3 {
			t.Errorf("truncated=%v rows=%d total=%d, want true/2/3", truncated, len(rows), total)
		}
	})

	t.Run("under cap -> not truncated", func(t *testing.T) {
		svc2 := NewUsageTokenService(repoWith(2, 2), model.UsageBucketSizeHour, 180, 2)
		rows, _, truncated, err := svc2.Export(context.Background(), "u1", "", "", model.UsageTokenFilter{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if truncated || len(rows) != 2 {
			t.Errorf("truncated=%v rows=%d", truncated, len(rows))
		}
	})
}
