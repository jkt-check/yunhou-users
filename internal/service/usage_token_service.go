package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
)

// UsageTokenService 实现 kaya token 用量统计（契约 yunhou-terminal
// docs/api/usage-stats.md v1.2）的业务规则。摄入的无状态逐字段校验在
// handler 完成（校验顺序即契约 §3.2 的拒绝顺序，且全或无要求零 DB 写入）；
// 本层持有部署级 bucketSize / retentionDays，负责查询窗口、维度、时区规则、
// 删除水位/墓碑的边界计算，以及 provider display_name 的目录解析。
type UsageTokenService struct {
	repo          repo.UsageTokenRepo
	bucketSize    string // hour | day（部署级固定，USAGE_TOKEN_BUCKET_SIZE）
	retentionDays int    // USAGE_TOKEN_RETENTION_DAYS
	// exportMaxRows 是导出单次行数上限（USAGE_TOKEN_EXPORT_MAX_ROWS，
	// 契约 v1.2 §4.6 的 EXPORT_MAX_ROWS）。
	exportMaxRows int
}

// NewUsageTokenService 的 bucketSize/retentionDays/exportMaxRows 已由
// config.Validate 校验过枚举与下界。
func NewUsageTokenService(r repo.UsageTokenRepo, bucketSize string, retentionDays, exportMaxRows int) *UsageTokenService {
	return &UsageTokenService{repo: r, bucketSize: bucketSize, retentionDays: retentionDays, exportMaxRows: exportMaxRows}
}

// BucketSize / RetentionDays / ExportMaxRows 暴露部署配置（handler 的校验、
// 导出头与 coverage 响应需要同一份值，避免两处漂移）。
func (s *UsageTokenService) BucketSize() string { return s.bucketSize }
func (s *UsageTokenService) RetentionDays() int { return s.retentionDays }
func (s *UsageTokenService) ExportMaxRows() int { return s.exportMaxRows }

// ---------------------------------------------------------------------------
// 摄入
// ---------------------------------------------------------------------------

// Ingest 落库一批已校验桶。设备撤销 / 水位墓碑丢弃在 repo 的单事务里完成；
// ErrUsageDeviceRevoked 原样透传给 handler 映射 403。
func (s *UsageTokenService) Ingest(ctx context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error) {
	result, err := s.repo.IngestBuckets(ctx, userID, in)
	if err != nil {
		// batch_id 是客户端建议的追踪 id（契约 §3.2「用于日志追踪」）——
		// 失败与丢弃两条路径都必须带上它，否则该字段从未落地。
		log.Printf("usage tokens: ingest failed: user=%s device=%s batch=%s: %v", userID, in.DeviceID, in.BatchID, err)
		return nil, err
	}
	if len(result.Discarded) > 0 {
		log.Printf("usage tokens: ingest discarded %d bucket(s) by watermark/tombstone: user=%s device=%s batch=%s",
			len(result.Discarded), userID, in.DeviceID, in.BatchID)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// usageTokenDateLayout 是 from/to 的日历日期格式（闭区间，YYYY-MM-DD）。
const usageTokenDateLayout = "2006-01-02"

// ParseUsageTokenWindow 解析 from/to 为 UTC 日历日界（导出供 handler 推导
// 导出文件名里的生效窗口）。缺省 = 最近 90 天；
// 单边缺省时另一侧按默认窗口补齐；窗口 >180 天 → window_too_large（契约
// §4.1）。返回的 to 是闭区间最后一天（左闭）；调用方用 to.AddDate(0,0,1)
// 得到半开区间上界。
func ParseUsageTokenWindow(fromStr, toStr string, now time.Time) (time.Time, time.Time, error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	var from, to time.Time
	var err error
	if toStr == "" {
		to = today
	} else {
		to, err = time.Parse(usageTokenDateLayout, toStr)
		if err != nil {
			return time.Time{}, time.Time{}, usageTokenErr(400, "invalid_request",
				fmt.Sprintf("to %q must be YYYY-MM-DD", toStr))
		}
	}
	if fromStr == "" {
		from = to.AddDate(0, 0, -(model.UsageTokenDefaultWindowDays - 1))
	} else {
		from, err = time.Parse(usageTokenDateLayout, fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, usageTokenErr(400, "invalid_request",
				fmt.Sprintf("from %q must be YYYY-MM-DD", fromStr))
		}
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, usageTokenErr(400, "invalid_request", "to must not be before from")
	}
	// 契约 §4.1 的窗口上限按自然日计：from/to 均为含端点的日期，故
	// to-from 相差 180 天即 181 个自然日，已超上限。用 >= 把边界卡死在
	// 180 个自然日（PR #61 review S5）。
	if to.Sub(from) >= model.UsageTokenMaxWindowDays*24*time.Hour {
		return time.Time{}, time.Time{}, usageTokenErr(400, "window_too_large",
			fmt.Sprintf("window must not exceed %d days", model.UsageTokenMaxWindowDays))
	}
	return from, to, nil
}

// usageTokenErr 构造契约 §8 的错误。
func usageTokenErr(status int, code, msg string) *model.UsageTokenError {
	return &model.UsageTokenError{Status: status, Code: code, Message: msg}
}

// validateUsageTokenSourceFilter 校验 source 过滤值（all|chat|gateway，
// 空 = all）。
func validateUsageTokenSourceFilter(f *model.UsageTokenFilter) error {
	switch f.Source {
	case "", "all":
		f.Source = ""
		return nil
	case model.UsageSourceChat, model.UsageSourceGateway:
		return nil
	default:
		return usageTokenErr(400, "invalid_request", `source must be all|chat|gateway`)
	}
}

// Summary 实现 GET /user/usage/tokens/summary（§4.2）。
func (s *UsageTokenService) Summary(ctx context.Context, userID, fromStr, toStr string, f model.UsageTokenFilter) (*model.UsageTokenSummary, error) {
	if err := validateUsageTokenSourceFilter(&f); err != nil {
		return nil, err
	}
	from, to, err := ParseUsageTokenWindow(fromStr, toStr, time.Now())
	if err != nil {
		return nil, err
	}
	sum, err := s.repo.Summary(ctx, userID, s.bucketSize, from, to.AddDate(0, 0, 1), f)
	if err != nil {
		return nil, err
	}
	// 输出恒 UTC（Z 后缀），不随 DB 会话时区漂移。
	sum.FirstBucket = utcTimePtr(sum.FirstBucket)
	sum.LastBucket = utcTimePtr(sum.LastBucket)
	return sum, nil
}

// usageBreakdownDimensions 是 §4.3 的 dimension 白名单。
var usageBreakdownDimensions = map[string]bool{
	"agent": true, "provider": true, "model": true,
	"provider_model": true, "device": true, "source": true,
}

// Breakdown 实现 GET /user/usage/tokens/breakdown（§4.3）：limit 默认 50、
// 最大 100（超出钳到 100）；repo 拉 limit+1 行探测截断。
func (s *UsageTokenService) Breakdown(ctx context.Context, userID, dimension, fromStr, toStr string, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, bool, error) {
	if !usageBreakdownDimensions[dimension] {
		return nil, false, usageTokenErr(400, "invalid_dimension",
			"dimension must be agent|provider|model|provider_model|device|source")
	}
	if err := validateUsageTokenSourceFilter(&f); err != nil {
		return nil, false, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	from, to, err := ParseUsageTokenWindow(fromStr, toStr, time.Now())
	if err != nil {
		return nil, false, err
	}
	items, err := s.repo.Breakdown(ctx, userID, s.bucketSize, dimension, from, to.AddDate(0, 0, 1), f, limit+1)
	if err != nil {
		return nil, false, err
	}
	truncated := len(items) > limit
	if truncated {
		items = items[:limit]
	}
	// provider / provider_model 恒带 display_name（目录解析，缺失回退 key
	// 原文，绝不返回空串，§4.3 规则条）。
	if dimension == "provider" || dimension == "provider_model" {
		for i := range items {
			name := UsageProviderDisplayName(*items[i].ProviderKey)
			items[i].DisplayName = &name
		}
	}
	return items, truncated, nil
}

// usageTimeseriesGroupBys 是 §4.4 的 group_by 白名单（none = 不分组）。
var usageTimeseriesGroupBys = map[string]bool{
	"none": true, "agent": true, "provider_model": true, "device": true,
}

// Timeseries 实现 GET /user/usage/tokens/timeseries（§4.4）。时区规则：
// hour 部署下 from/to 按 tz 本地日解释并换算 UTC 半开区间；day 部署只接受
// UTC（day 桶按 UTC 00:00 落库，非 UTC 日界算不出，允许会静默错账）。
func (s *UsageTokenService) Timeseries(ctx context.Context, userID, interval, groupBy, tz, fromStr, toStr string, f model.UsageTokenFilter) (*model.UsageTimeseriesResult, error) {
	switch interval {
	case "day", "week", "month":
	default:
		return nil, usageTokenErr(400, "invalid_interval", "interval must be day|week|month")
	}
	if !usageTimeseriesGroupBys[groupBy] {
		return nil, usageTokenErr(400, "invalid_dimension", "group_by must be none|agent|provider_model|device")
	}
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, usageTokenErr(400, "invalid_request", fmt.Sprintf("tz %q is not a valid IANA timezone", tz))
	}
	if s.bucketSize == model.UsageBucketSizeDay && tz != "UTC" {
		return nil, usageTokenErr(400, "tz_not_supported_for_day",
			"tz other than UTC is not supported when the deployment bucket_size is day")
	}
	if err := validateUsageTokenSourceFilter(&f); err != nil {
		return nil, err
	}
	// 窗口长度按日历日校验（与 tz 无关），边界再按 tz 换算。
	from, to, err := ParseUsageTokenWindow(fromStr, toStr, time.Now())
	if err != nil {
		return nil, err
	}
	fromUTC := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc).UTC()
	toUTC := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1).UTC()

	items, err := s.repo.Timeseries(ctx, userID, s.bucketSize, interval, groupBy, tz, fromUTC, toUTC, f)
	if err != nil {
		return nil, err
	}
	if groupBy == "provider_model" {
		for i := range items {
			if items[i].ProviderKey != nil {
				name := UsageProviderDisplayName(*items[i].ProviderKey)
				items[i].DisplayName = &name
			}
		}
	}
	return &model.UsageTimeseriesResult{TZ: tz, Items: items}, nil
}

// Coverage 实现 GET /user/usage/tokens/coverage（§4.5）：设备列表含已撤销
// （revoked:true），顶层 earliest/latest 排除已撤销设备。
func (s *UsageTokenService) Coverage(ctx context.Context, userID string) (*model.UsageCoverage, error) {
	devices, err := s.repo.CoverageDevices(ctx, userID, s.bucketSize)
	if err != nil {
		return nil, err
	}
	out := &model.UsageCoverage{
		BucketSize:    s.bucketSize,
		RetentionDays: s.retentionDays,
		ExportMaxRows: s.exportMaxRows, // v1.2 §4.5：客户端据此预判导出窗口
		ServerTime:    time.Now().UTC(),
		Devices:       devices,
	}
	for i := range out.Devices {
		normalizeDeviceViewTimes(&out.Devices[i])
		if out.Devices[i].Revoked {
			continue // 汇总口径排除撤销设备（§4.5）
		}
		if ts := out.Devices[i].EarliestBucket; ts != nil && (out.EarliestBucket == nil || ts.Before(*out.EarliestBucket)) {
			out.EarliestBucket = ts
		}
		if ts := out.Devices[i].LatestBucket; ts != nil && (out.LatestBucket == nil || ts.After(*out.LatestBucket)) {
			out.LatestBucket = ts
		}
	}
	return out, nil
}

// Export 实现 GET /user/usage/tokens/export 的数据侧（v1.2 §4.6）；CSV
// 渲染、公式注入防护与三个 X-Result-* 响应头在 handler。fail-closed 判定
// 在本层：total 超过部署上限且未显式 allowTruncated → 400 export_too_large
// （data 带 max_rows / from / to，from/to 是生效窗口的 YYYY-MM-DD），
// 绝不返回静默截断的文件。total 是窗口内命中行数（同快照计数，不受
// cap 影响）；truncated = total > 实际返回行数。
func (s *UsageTokenService) Export(ctx context.Context, userID, fromStr, toStr string, f model.UsageTokenFilter, allowTruncated bool) (rows []model.UsageExportRow, total int64, truncated bool, err error) {
	if err := validateUsageTokenSourceFilter(&f); err != nil {
		return nil, 0, false, err
	}
	from, to, err := ParseUsageTokenWindow(fromStr, toStr, time.Now())
	if err != nil {
		return nil, 0, false, err
	}
	// fail-closed 判定放进 repo 同事务的 decide 回调（M3）：超限即报错，
	// repo 不取数据行；total 由 repo 透传供诊断。
	rows, total, err = s.repo.ExportRows(ctx, userID, s.bucketSize, from, to.AddDate(0, 0, 1), f, s.exportMaxRows,
		func(total int64) (bool, error) {
			if total > int64(s.exportMaxRows) && !allowTruncated {
				return false, &model.UsageTokenError{
					Status: 400, Code: "export_too_large",
					Message: fmt.Sprintf("export has %d rows, exceeding the %d row limit; narrow the window or add device_id/source filters, or pass allow_truncated=true for a partial export",
						total, s.exportMaxRows),
					Data: map[string]any{
						"max_rows": s.exportMaxRows,
						"from":     from.Format(usageTokenDateLayout),
						"to":       to.Format(usageTokenDateLayout),
					},
				}
			}
			return true, nil
		})
	if err != nil {
		return nil, total, false, err
	}
	truncated = total > int64(len(rows))
	for i := range rows {
		rows[i].BucketStart = rows[i].BucketStart.UTC()
	}
	return rows, total, truncated, nil
}

// ---------------------------------------------------------------------------
// 删除（§4.7）
// ---------------------------------------------------------------------------

// CeilToBucket 是契约 §3.4 的 ceil_to_bucket：t 恰在桶边界取 t，否则取下
// 一个边界。全程 UTC，不依赖 SQL 会话时区。导出供删除路径与测试使用。
func CeilToBucket(t time.Time, bucketSize string) time.Time {
	t = t.UTC()
	var floor time.Time
	var step time.Duration
	if bucketSize == model.UsageBucketSizeDay {
		floor = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		step = 24 * time.Hour
	} else {
		floor = t.Truncate(time.Hour)
		step = time.Hour
	}
	if floor.Equal(t) {
		return t
	}
	return floor.Add(step)
}

// Delete 实现 DELETE /user/usage/tokens：无参数 = 全量 + 用户级水位；只带
// device_id = 设备级水位（设备不存在 → ErrUsageDeviceNotFound → 404）；
// 带 from/to（可叠加 device_id）= 窗口删除 + 墓碑。
func (s *UsageTokenService) Delete(ctx context.Context, userID, deviceID, fromStr, toStr string) (*model.UsageDeleteResult, error) {
	now := time.Now().UTC()
	if fromStr == "" && toStr == "" {
		cleared := CeilToBucket(now, s.bucketSize)
		var deleted int64
		var err error
		if deviceID == "" {
			deleted, err = s.repo.DeleteAll(ctx, userID, cleared)
		} else {
			deleted, err = s.repo.DeleteByDevice(ctx, userID, deviceID, cleared)
		}
		if err != nil {
			return nil, err
		}
		return &model.UsageDeleteResult{DeletedBuckets: deleted, ClearedBefore: &cleared}, nil
	}
	if fromStr == "" || toStr == "" {
		return nil, usageTokenErr(400, "invalid_request", "from and to must be given together")
	}
	from, err := time.Parse(usageTokenDateLayout, fromStr)
	if err != nil {
		return nil, usageTokenErr(400, "invalid_request", fmt.Sprintf("from %q must be YYYY-MM-DD", fromStr))
	}
	to, err := time.Parse(usageTokenDateLayout, toStr)
	if err != nil {
		return nil, usageTokenErr(400, "invalid_request", fmt.Sprintf("to %q must be YYYY-MM-DD", toStr))
	}
	if to.Before(from) {
		return nil, usageTokenErr(400, "invalid_request", "to must not be before from")
	}
	// 与 ParseUsageTokenWindow 同口径：自然日计，>= 卡死 180 天上限。
	if to.Sub(from) >= model.UsageTokenMaxWindowDays*24*time.Hour {
		return nil, usageTokenErr(400, "window_too_large",
			fmt.Sprintf("window must not exceed %d days", model.UsageTokenMaxWindowDays))
	}
	// 窗口墓碑按部署粒度取对齐桶端点（§4.7）：from_bucket = from 00:00Z；
	// to_bucket = 窗口内最后一个对齐桶起点（hour = to 23:00Z，day = to 00:00Z）。
	fromBucket := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toBucket := fromBucket
	if s.bucketSize == model.UsageBucketSizeDay {
		toBucket = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		toBucket = time.Date(to.Year(), to.Month(), to.Day(), 23, 0, 0, 0, time.UTC)
	}
	// 未来窗口防护（PR #61 复审）：整体在未来的窗口没有现存桶可删，却会
	// 留下一条拦截未来摄入的墓碑（要等保留期才被 sweeper 清掉）——直接
	// 拒绝；部分覆盖未来的窗口把 to_bucket 钳到当前桶起点，未来部分本无
	// 数据，钳制不改删除语义。
	if fromBucket.After(now) {
		return nil, usageTokenErr(400, "invalid_request", "from must not be in the future")
	}
	var curBucket time.Time
	if s.bucketSize == model.UsageBucketSizeDay {
		curBucket = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		curBucket = now.Truncate(time.Hour)
	}
	if toBucket.After(curBucket) {
		toBucket = curBucket
	}
	var devPtr *string
	if deviceID != "" {
		devPtr = &deviceID
	}
	deleted, err := s.repo.DeleteWindow(ctx, userID, devPtr, fromBucket, toBucket)
	if err != nil {
		return nil, err
	}
	return &model.UsageDeleteResult{
		DeletedBuckets: deleted,
		Tombstone: &model.UsageTombstoneView{
			DeviceID:   devPtr,
			FromBucket: fromBucket,
			ToBucket:   toBucket,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// 设备管理（§4.8）
// ---------------------------------------------------------------------------

// RegisterDevice 实现 POST /user/devices；字段校验在 handler，撤销设备由
// repo 拒绝（ErrUsageDeviceRevoked → 403）。
func (s *UsageTokenService) RegisterDevice(ctx context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDeviceView, error) {
	dev, err := s.repo.UpsertDevice(ctx, userID, in)
	if err != nil {
		return nil, err
	}
	return usageDeviceToView(dev), nil
}

// ListDevices 实现 GET /user/devices（含已撤销标记）。
func (s *UsageTokenService) ListDevices(ctx context.Context, userID string) ([]model.UsageDeviceView, error) {
	devices, err := s.repo.ListDevices(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]model.UsageDeviceView, 0, len(devices))
	for i := range devices {
		out = append(out, *usageDeviceToView(&devices[i]))
	}
	return out, nil
}

// RenameDevice 实现 PATCH /user/devices/:id（只允许改 label）。
func (s *UsageTokenService) RenameDevice(ctx context.Context, userID, deviceID string, label *string) error {
	return s.repo.UpdateDeviceLabel(ctx, userID, deviceID, label)
}

// RevokeDevice 实现 DELETE /user/devices/:id；purge=true 同事务删除该设备
// 历史桶。
func (s *UsageTokenService) RevokeDevice(ctx context.Context, userID, deviceID string, purge bool) error {
	return s.repo.RevokeDevice(ctx, userID, deviceID, purge)
}

// usageDeviceToView 转 API 视图；时间统一归一到 UTC（Z 后缀输出）。
func usageDeviceToView(d *model.UsageDevice) *model.UsageDeviceView {
	v := &model.UsageDeviceView{
		DeviceID:       d.DeviceID,
		Label:          d.Label,
		Platform:       d.Platform,
		AppVersion:     d.AppVersion,
		ConsentVersion: d.ConsentVersion,
		Revoked:        d.RevokedAt != nil,
	}
	v.ConsentedAt = utcTimePtr(d.ConsentedAt)
	v.LastSeenAt = utcTimePtr(d.LastSeenAt)
	return v
}

// normalizeDeviceViewTimes 把 coverage 设备视图的时间字段归一到 UTC。
func normalizeDeviceViewTimes(v *model.UsageDeviceView) {
	v.ConsentedAt = utcTimePtr(v.ConsentedAt)
	v.LastSeenAt = utcTimePtr(v.LastSeenAt)
	v.LastBucketAt = utcTimePtr(v.LastBucketAt)
	v.EarliestBucket = utcTimePtr(v.EarliestBucket)
	v.LatestBucket = utcTimePtr(v.LatestBucket)
}

// utcTimePtr 归一时间指针到 UTC；nil 透传。pq 扫 timestamptz 得到的时间
// 带会话时区偏移，直接 JSON 序列化会出现 +00:00 而非契约示例的 Z 后缀。
func utcTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// IsUsageDeviceNotFound 是 handler 的错误映射辅助。
func IsUsageDeviceNotFound(err error) bool { return errors.Is(err, model.ErrUsageDeviceNotFound) }
