package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/yunhou/users/internal/model"
)

// UsageTokenRepo 是 kaya token 用量统计（契约 yunhou-terminal
// docs/api/usage-stats.md v1.2，migration 047）的存储面。三条横切规则：
//
//  1. 租户隔离：每个查询都强制 WHERE user_id = $JWT-sub —— v1 无 RLS 是
//     显式决策（migration 047 头注），本接口就是唯一防线；
//  2. 防双计：所有读查询固定 bucket_size = 部署配置（唯一键里的
//     bucket_size 只防写重，不防读双计，契约 §6 第 1 条）；
//  3. 撤销设备：统计/导出查询用 JOIN usage_devices ... revoked_at IS NULL
//     排除（INNER JOIN 语义：摄入保证设备行必存在，所以不会漏桶）。
//
// 全部 SQL 参数化；interval 等无法走参数的位置用白名单 switch（与
// usage_repo.go 的 usageGroupColumn 同一模式）。
type UsageTokenRepo interface {
	// IngestBuckets 在单个事务里完成：设备行 auto-upsert（不存在则插入
	// label=NULL、last_seen_at=now()；已撤销 → ErrUsageDeviceRevoked 且
	// 整批零写入）、水位/墓碑丢弃过滤、通过桶的 ON CONFLICT 覆盖写入
	//（契约 §3.3/§3.4）。buckets 已通过的桶才写库，丢弃的进
	// result.Discarded（不是失败）。
	IngestBuckets(ctx context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error)

	// Summary 按固定三过滤 + 可选过滤聚合 [from, to)（半开，调用方已把
	// to 推到窗口次日 00:00Z）。
	Summary(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter) (*model.UsageTokenSummary, error)
	// Breakdown 按 dimension（service 已白名单校验）分组聚合，按
	// total_tokens DESC、键 ASC 排序；limit 由调用方给（service 传
	// limit+1 以探测截断）。display_name 不在此解析（service 层目录）。
	Breakdown(ctx context.Context, userID, bucketSize, dimension string, from, to time.Time, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, error)
	// Timeseries 按 interval（day|week|month，service 白名单）在 tz 下
	// 切桶聚合；from/to 是已换算好的 UTC 半开区间端点。桶标签 =
	// date_trunc 后的 tz 本地周期起点日期（week=周一、month=1 号）。
	Timeseries(ctx context.Context, userID, bucketSize, interval, groupBy, tz string, from, to time.Time, f model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error)
	// CoverageDevices 返回该用户全部设备（含已撤销）及每设备在当前
	// bucket_size 下的桶 MIN/MAX；顶层汇总由 service 在排除撤销设备后
	// 计算（契约 §4.5：coverage 是三过滤的唯一例外）。
	CoverageDevices(ctx context.Context, userID, bucketSize string) ([]model.UsageDeviceView, error)
	// ExportRows 在同一数据库快照里"先计数、由调用方判定、再取行"
	//（v1.2 §4.6 MUST）：BEGIN READ ONLY + REPEATABLE READ 事务内，先按同
	// 一 where 计 COUNT(*)，然后把 total 交给 decide 判定；decide 返回错
	// 误（如 fail-closed 的 export_too_large）或 fetch=false 时直接提交返
	// 回，**不取数据行**——否则超限路径会白取 cap 行（约 50MB）再丢弃
	//（PR #61 review M3）。为什么不能在流式阶段才发现超限：CSV 响应头在
	// 写出第一个字节时就已提交，读到第 cap+1 行才改主意已无法回退状态码/
	// 响应头；HTTP trailer 在浏览器 fetch 中不可见，也不能当信号。所以只
	// 能先计数、后写头——且计数与取数必须同快照，否则计数后新摄入的桶会
	// 让流式阶段再次超限。total 是窗口内命中行数（不受 cap 影响，decide
	// 报错时也透传，供日志/诊断），rows 至多 cap 行。
	ExportRows(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter, rowCap int, decide func(total int64) (fetch bool, err error)) (rows []model.UsageExportRow, total int64, err error)

	// DeleteAll 删该用户全部桶（不分 bucket_size）并把用户级水位推进到
	// clearedBefore（单调前进，GREATEST）。单事务。
	DeleteAll(ctx context.Context, userID string, clearedBefore time.Time) (int64, error)
	// DeleteByDevice 删该设备全部桶并推进设备级水位；设备行不存在 →
	// ErrUsageDeviceNotFound（整个删除回滚）。单事务。
	DeleteByDevice(ctx context.Context, userID, deviceID string, clearedBefore time.Time) (int64, error)
	// DeleteWindow 删 [fromBucket, toBucket] 闭区间内（可选该设备）的桶
	// 并写一条墓碑（deviceID nil = 全设备）。单事务。
	DeleteWindow(ctx context.Context, userID string, deviceID *string, fromBucket, toBucket time.Time) (int64, error)

	// UpsertDevice 是 POST /user/devices 的落库：不存在则插入；已存在只
	// 更新非空字段（COALESCE，空值不覆盖已有 label）；已撤销 → 整个写入
	// 回滚并返回 ErrUsageDeviceRevoked。
	UpsertDevice(ctx context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDevice, error)
	// ListDevices 列出全部设备（含已撤销，按注册序）。
	ListDevices(ctx context.Context, userID string) ([]model.UsageDevice, error)
	// UpdateDeviceLabel 只改 label（nil → NULL）；不存在 →
	// ErrUsageDeviceNotFound。
	UpdateDeviceLabel(ctx context.Context, userID, deviceID string, label *string) error
	// RevokeDevice 置 revoked_at=now()；purge 时同事务删除该设备全部桶；
	// 不存在 → ErrUsageDeviceNotFound。重复撤销幂等（revoked_at 不变）。
	RevokeDevice(ctx context.Context, userID, deviceID string, purge bool) error

	// SweepRetention 删除 bucket_start 早于 cutoff 的桶与 to_bucket 早于
	// cutoff 的墓碑（保留期定时任务，契约 §4.9）。返回两类删除行数。
	SweepRetention(ctx context.Context, cutoff time.Time) (buckets, tombstones int64, err error)
}

type usageTokenRepo struct{ db *sqlx.DB }

func NewUsageTokenRepo(db *sqlx.DB) *usageTokenRepo { return &usageTokenRepo{db: db} }

// ---------------------------------------------------------------------------
// 摄入
// ---------------------------------------------------------------------------

// usageTombstoneRow 是摄入过滤用到的墓碑投影。
type usageTombstoneRow struct {
	DeviceID   *string   `db:"device_id"`
	FromBucket time.Time `db:"from_bucket"`
	ToBucket   time.Time `db:"to_bucket"`
}

func (r *usageTokenRepo) IngestBuckets(ctx context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 设备行 auto-upsert（契约 §3.2 末段）：不存在则插入 label=NULL 的占位
	// 行；存在则刷新 last_seen_at。RETURNING 顺带取回撤销/水位状态。
	var revokedAt, deviceCleared *time.Time
	err = tx.QueryRowxContext(ctx, `
		INSERT INTO usage_devices (user_id, device_id, last_seen_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id, device_id) DO UPDATE SET last_seen_at = now()
		RETURNING revoked_at, cleared_before`, userID, in.DeviceID).
		Scan(&revokedAt, &deviceCleared)
	if err != nil {
		return nil, fmt.Errorf("ingest device upsert: %w", err)
	}
	// 撤销设备 → 整批拒绝（403 device_revoked），在写任何桶之前回滚。
	if revokedAt != nil {
		return nil, model.ErrUsageDeviceRevoked
	}

	var userCleared *time.Time
	err = tx.QueryRowxContext(ctx,
		`SELECT cleared_before FROM usage_token_watermarks WHERE user_id = $1`, userID).
		Scan(&userCleared)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("ingest watermark read: %w", err)
	}

	var tombstones []usageTombstoneRow
	if err := tx.SelectContext(ctx, &tombstones, `
		SELECT device_id, from_bucket, to_bucket FROM usage_token_tombstones
		WHERE user_id = $1 AND (device_id IS NULL OR device_id = $2)`,
		userID, in.DeviceID); err != nil {
		return nil, fmt.Errorf("ingest tombstone read: %w", err)
	}

	// 水位/墓碑丢弃（契约 §3.4）：命中的桶不写库、不算失败，进 discarded。
	result := &model.UsageIngestResult{Discarded: []model.UsageDiscardedBucket{}}
	var kept []model.UsageTokenBucket
	for _, b := range in.Buckets {
		if usageBucketCleared(b, userCleared, deviceCleared, tombstones) {
			result.Discarded = append(result.Discarded, model.UsageDiscardedBucket{Index: b.Index, Reason: "cleared"})
			continue
		}
		kept = append(kept, b)
	}
	result.Accepted = len(kept)

	if len(kept) > 0 {
		n, err := usageTokenUpsertTx(ctx, tx, userID, in.DeviceID, kept)
		if err != nil {
			return nil, err
		}
		result.Updated = n
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// usageBucketCleared 判定一个桶是否命中三类删除标记（半开水位 +
// 闭区间墓碑，契约 §3.4）。
func usageBucketCleared(b model.UsageTokenBucket, userCleared, deviceCleared *time.Time, tombstones []usageTombstoneRow) bool {
	if userCleared != nil && b.BucketStart.Before(*userCleared) {
		return true
	}
	if deviceCleared != nil && b.BucketStart.Before(*deviceCleared) {
		return true
	}
	for _, t := range tombstones {
		if !b.BucketStart.Before(t.FromBucket) && !b.BucketStart.After(t.ToBucket) {
			return true
		}
	}
	return false
}

// usageTokenUpsertTx 是契约 §3.3 的多行 upsert：唯一键冲突时覆盖为最新
// 累计值，不累加。返回实际写入/覆盖行数。
func usageTokenUpsertTx(ctx context.Context, tx *sqlx.Tx, userID, deviceID string, buckets []model.UsageTokenBucket) (int, error) {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO usage_token_buckets
		(user_id, device_id, bucket_start, bucket_size, source, agent_key, provider_key, model_key,
		 prompt_tokens, completion_tokens, total_tokens, requests)
		VALUES `)
	args := make([]any, 0, len(buckets)*12)
	for i, b := range buckets {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i * 12
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11, base+12)
		args = append(args, userID, deviceID, b.BucketStart, b.BucketSize, b.Source,
			b.AgentKey, b.ProviderKey, b.ModelKey,
			b.PromptTokens, b.CompletionTokens, b.TotalTokens, b.Requests)
	}
	sb.WriteString(` ON CONFLICT (user_id, device_id, bucket_start, bucket_size, source, agent_key, provider_key, model_key)
		DO UPDATE SET
		  prompt_tokens = EXCLUDED.prompt_tokens,
		  completion_tokens = EXCLUDED.completion_tokens,
		  total_tokens = EXCLUDED.total_tokens,
		  requests = EXCLUDED.requests,
		  updated_at = now()`)
	res, err := tx.ExecContext(ctx, sb.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("bucket upsert: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// 查询（固定三过滤：user_id / bucket_size / 未撤销设备）
// ---------------------------------------------------------------------------

// usageTokenBaseWhere 拼出固定三过滤 + 半开窗口；可选过滤追加其后。
// $1=userID $2=bucketSize $3=from(含) $4=to(不含)。
func usageTokenBaseWhere(f model.UsageTokenFilter) (string, []any) {
	args := make([]any, 0, 8)
	var sb strings.Builder
	sb.WriteString(` FROM usage_token_buckets b
		JOIN usage_devices d ON d.user_id = b.user_id AND d.device_id = b.device_id AND d.revoked_at IS NULL
		WHERE b.user_id = $1 AND b.bucket_size = $2 AND b.bucket_start >= $3 AND b.bucket_start < $4`)
	n := 4
	add := func(v any) string {
		n++
		args = append(args, v)
		return fmt.Sprintf("$%d", n)
	}
	if f.Source == model.UsageSourceChat || f.Source == model.UsageSourceGateway {
		sb.WriteString(` AND b.source = ` + add(f.Source))
	}
	if len(f.Agents) > 0 {
		sb.WriteString(` AND b.agent_key = ANY(` + add(pq.Array(f.Agents)) + `)`)
	}
	if len(f.ProviderKeys) > 0 {
		sb.WriteString(` AND b.provider_key = ANY(` + add(pq.Array(f.ProviderKeys)) + `)`)
	}
	if len(f.ModelKeys) > 0 {
		sb.WriteString(` AND b.model_key = ANY(` + add(pq.Array(f.ModelKeys)) + `)`)
	}
	if len(f.DeviceIDs) > 0 {
		sb.WriteString(` AND b.device_id = ANY(` + add(pq.Array(f.DeviceIDs)) + `)`)
	}
	return sb.String(), args
}

func (r *usageTokenRepo) Summary(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter) (*model.UsageTokenSummary, error) {
	where, args := usageTokenBaseWhere(f)
	// active_days 按 UTC 日期计数（§4.2：from/to 恒按 UTC）；桶存储恒 UTC，
	// AT TIME ZONE 'UTC' 只是显式钉死，不依赖会话时区。
	query := `SELECT COALESCE(SUM(b.total_tokens), 0) AS total_tokens,
		COALESCE(SUM(b.prompt_tokens), 0) AS prompt_tokens,
		COALESCE(SUM(b.completion_tokens), 0) AS completion_tokens,
		COALESCE(SUM(b.requests), 0) AS requests,
		COUNT(DISTINCT b.device_id) AS device_count,
		COUNT(DISTINCT (b.bucket_start AT TIME ZONE 'UTC')::date) AS active_days,
		MIN(b.bucket_start) AS first_bucket,
		MAX(b.bucket_start) AS last_bucket` + where
	var row struct {
		TotalTokens      int64      `db:"total_tokens"`
		PromptTokens     int64      `db:"prompt_tokens"`
		CompletionTokens int64      `db:"completion_tokens"`
		Requests         int64      `db:"requests"`
		DeviceCount      int        `db:"device_count"`
		ActiveDays       int        `db:"active_days"`
		FirstBucket      *time.Time `db:"first_bucket"`
		LastBucket       *time.Time `db:"last_bucket"`
	}
	full := append([]any{userID, bucketSize, from, to}, args...)
	if err := r.db.GetContext(ctx, &row, query, full...); err != nil {
		return nil, err
	}
	return &model.UsageTokenSummary{
		TotalTokens:      row.TotalTokens,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		Requests:         row.Requests,
		DeviceCount:      row.DeviceCount,
		ActiveDays:       row.ActiveDays,
		FirstBucket:      row.FirstBucket,
		LastBucket:       row.LastBucket,
	}, nil
}

// usageBreakdownSelect 把 dimension 白名单映射到两个键列（key2 不用时恒
// NULL，行形状保持统一）。dimension 由 service 校验，这里仍是封闭 switch
// —— 列名进 SQL 文本，白名单即参数化。
func usageBreakdownSelect(dimension string) (string, error) {
	switch dimension {
	case "agent":
		return `b.agent_key AS key1, NULL::text AS key2, NULL::text AS label`, nil
	case "provider":
		return `b.provider_key AS key1, NULL::text AS key2, NULL::text AS label`, nil
	case "model":
		return `b.model_key AS key1, NULL::text AS key2, NULL::text AS label`, nil
	case "provider_model":
		return `b.provider_key AS key1, b.model_key AS key2, NULL::text AS label`, nil
	case "device":
		return `b.device_id AS key1, NULL::text AS key2, d.label AS label`, nil
	case "source":
		return `b.source AS key1, NULL::text AS key2, NULL::text AS label`, nil
	default:
		return "", fmt.Errorf("unsupported breakdown dimension %q", dimension)
	}
}

func (r *usageTokenRepo) Breakdown(ctx context.Context, userID, bucketSize, dimension string, from, to time.Time, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, error) {
	sel, err := usageBreakdownSelect(dimension)
	if err != nil {
		return nil, err
	}
	where, args := usageTokenBaseWhere(f)
	n := len(args) + 4
	query := `SELECT ` + sel + `,
		SUM(b.total_tokens) AS total_tokens, SUM(b.prompt_tokens) AS prompt_tokens,
		SUM(b.completion_tokens) AS completion_tokens, SUM(b.requests) AS requests` +
		where + ` GROUP BY key1, key2, label
		ORDER BY total_tokens DESC, key1 ASC, key2 ASC
		LIMIT ` + fmt.Sprintf("$%d", n+1)
	full := append(append([]any{userID, bucketSize, from, to}, args...), limit)

	var rows []struct {
		Key1             string  `db:"key1"`
		Key2             *string `db:"key2"`
		Label            *string `db:"label"`
		TotalTokens      int64   `db:"total_tokens"`
		PromptTokens     int64   `db:"prompt_tokens"`
		CompletionTokens int64   `db:"completion_tokens"`
		Requests         int64   `db:"requests"`
	}
	if err := r.db.SelectContext(ctx, &rows, query, full...); err != nil {
		return nil, err
	}
	out := make([]model.UsageBreakdownItem, 0, len(rows))
	for _, row := range rows {
		item := model.UsageBreakdownItem{
			TotalTokens:      row.TotalTokens,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			Requests:         row.Requests,
		}
		k1 := row.Key1
		switch dimension {
		case "agent":
			item.AgentKey = &k1
		case "provider":
			item.ProviderKey = &k1
		case "model":
			item.ModelKey = &k1
		case "provider_model":
			item.ProviderKey = &k1
			item.ModelKey = row.Key2
		case "device":
			item.DeviceID = &k1
			item.Label = row.Label
		case "source":
			item.Source = &k1
		}
		out = append(out, item)
	}
	return out, nil
}

// usageTimeseriesGroup 把 group_by 白名单映射到分组键列（不含 bucket）。
func usageTimeseriesGroup(groupBy string) (sel, group string, err error) {
	switch groupBy {
	case "", "none":
		return "", "", nil
	case "agent":
		return `, b.agent_key AS key1, NULL::text AS key2, NULL::text AS label`, `, b.agent_key`, nil
	case "provider_model":
		return `, b.provider_key AS key1, b.model_key AS key2, NULL::text AS label`, `, b.provider_key, b.model_key`, nil
	case "device":
		return `, b.device_id AS key1, NULL::text AS key2, d.label AS label`, `, b.device_id, d.label`, nil
	default:
		return "", "", fmt.Errorf("unsupported timeseries group_by %q", groupBy)
	}
}

func (r *usageTokenRepo) Timeseries(ctx context.Context, userID, bucketSize, interval, groupBy, tz string, from, to time.Time, f model.UsageTokenFilter) ([]model.UsageTimeseriesItem, error) {
	grpSel, grpBy, err := usageTimeseriesGroup(groupBy)
	if err != nil {
		return nil, err
	}
	// interval 是 service 校验过的白名单值；date_trunc 的字段名不能走
	// 普通参数绑定（$n 在 date_trunc 里可用，但为防歧义这里白名单拼接，
	// tz 走参数）。bucket_start AT TIME ZONE $tz 得到 tz 本地墙钟时间，
	// date_trunc 在其上切日/周(周一)/月(1 号)，to_char 取周期起点日期。
	var truncField string
	switch interval {
	case "day", "week", "month":
		truncField = interval
	default:
		return nil, fmt.Errorf("unsupported timeseries interval %q", interval)
	}
	where, args := usageTokenBaseWhere(f)
	tzIdx := len(args) + 5
	query := `SELECT to_char(date_trunc('` + truncField + `', b.bucket_start AT TIME ZONE $` +
		fmt.Sprintf("%d", tzIdx) + `), 'YYYY-MM-DD') AS bucket` + grpSel + `,
		SUM(b.total_tokens) AS total_tokens, SUM(b.prompt_tokens) AS prompt_tokens,
		SUM(b.completion_tokens) AS completion_tokens, SUM(b.requests) AS requests` +
		where + ` GROUP BY bucket` + grpBy + ` ORDER BY bucket` + grpBy
	full := append(append([]any{userID, bucketSize, from, to}, args...), tz)

	var rows []struct {
		Bucket           string  `db:"bucket"`
		Key1             *string `db:"key1"`
		Key2             *string `db:"key2"`
		Label            *string `db:"label"`
		TotalTokens      int64   `db:"total_tokens"`
		PromptTokens     int64   `db:"prompt_tokens"`
		CompletionTokens int64   `db:"completion_tokens"`
		Requests         int64   `db:"requests"`
	}
	if err := r.db.SelectContext(ctx, &rows, query, full...); err != nil {
		return nil, err
	}
	out := make([]model.UsageTimeseriesItem, 0, len(rows))
	for _, row := range rows {
		item := model.UsageTimeseriesItem{
			Bucket:           row.Bucket,
			TotalTokens:      row.TotalTokens,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			Requests:         row.Requests,
		}
		switch groupBy {
		case "agent":
			item.AgentKey = row.Key1
		case "provider_model":
			item.ProviderKey = row.Key1
			item.ModelKey = row.Key2
		case "device":
			item.DeviceID = row.Key1
			item.Label = row.Label
		}
		out = append(out, item)
	}
	return out, nil
}

// coverageDeviceRow 是 CoverageDevices 的扫描行（设备 + 桶统计投影）。
type coverageDeviceRow struct {
	DeviceID       string     `db:"device_id"`
	Label          *string    `db:"label"`
	Platform       *string    `db:"platform"`
	AppVersion     *string    `db:"app_version"`
	ConsentVersion *string    `db:"consent_version"`
	ConsentedAt    *time.Time `db:"consented_at"`
	LastSeenAt     *time.Time `db:"last_seen_at"`
	RevokedAt      *time.Time `db:"revoked_at"`
	Earliest       *time.Time `db:"earliest"`
	Latest         *time.Time `db:"latest"`
}

func (r *usageTokenRepo) CoverageDevices(ctx context.Context, userID, bucketSize string) ([]model.UsageDeviceView, error) {
	// 设备列表不撤销过滤（§4.5 例外）；每设备桶统计按当前 bucket_size 过滤。
	var rows []coverageDeviceRow
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT d.device_id, d.label, d.platform, d.app_version,
		       d.consent_version, d.consented_at, d.last_seen_at, d.revoked_at,
		       s.earliest, s.latest
		FROM usage_devices d
		LEFT JOIN (
			SELECT device_id, MIN(bucket_start) AS earliest, MAX(bucket_start) AS latest
			FROM usage_token_buckets
			WHERE user_id = $1 AND bucket_size = $2
			GROUP BY device_id
		) s ON s.device_id = d.device_id
		WHERE d.user_id = $1
		ORDER BY d.created_at, d.device_id`, userID, bucketSize); err != nil {
		return nil, err
	}
	out := make([]model.UsageDeviceView, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.UsageDeviceView{
			DeviceID:       row.DeviceID,
			Label:          row.Label,
			Platform:       row.Platform,
			AppVersion:     row.AppVersion,
			ConsentVersion: row.ConsentVersion,
			ConsentedAt:    row.ConsentedAt,
			LastSeenAt:     row.LastSeenAt,
			LastBucketAt:   row.Latest, // §4.5：last_bucket_at = MAX(bucket_start)
			EarliestBucket: row.Earliest,
			LatestBucket:   row.Latest,
			Revoked:        row.RevokedAt != nil,
		})
	}
	return out, nil
}

func (r *usageTokenRepo) ExportRows(ctx context.Context, userID, bucketSize string, from, to time.Time, f model.UsageTokenFilter, rowCap int, decide func(total int64) (bool, error)) ([]model.UsageExportRow, int64, error) {
	where, args := usageTokenBaseWhere(f)
	base := append([]any{userID, bucketSize, from, to}, args...)

	// 先计数、由 decide 判定、后取行，同一 REPEATABLE READ 只读事务 =
	// 同一快照（契约 v1.2 §4.6 的 MUST；理由见接口注释）。
	tx, err := r.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var total int64
	if err := tx.GetContext(ctx, &total,
		`SELECT COUNT(*)`+where, base...); err != nil {
		return nil, 0, err
	}

	// fail-closed / 调用方放弃取数：直接提交，不拉取数据行（M3）。
	fetch, derr := decide(total)
	if derr != nil {
		return nil, total, derr
	}
	if !fetch {
		if err := tx.Commit(); err != nil {
			return nil, 0, err
		}
		return nil, total, nil
	}

	query := `SELECT b.device_id, b.bucket_start, b.source, b.agent_key, b.provider_key, b.model_key,
		b.prompt_tokens, b.completion_tokens, b.total_tokens, b.requests` +
		where + ` ORDER BY b.bucket_start, b.device_id, b.source, b.agent_key, b.provider_key, b.model_key
		LIMIT ` + fmt.Sprintf("$%d", len(base)+1)
	var rows []struct {
		DeviceID         string    `db:"device_id"`
		BucketStart      time.Time `db:"bucket_start"`
		Source           string    `db:"source"`
		AgentKey         string    `db:"agent_key"`
		ProviderKey      string    `db:"provider_key"`
		ModelKey         string    `db:"model_key"`
		PromptTokens     int64     `db:"prompt_tokens"`
		CompletionTokens int64     `db:"completion_tokens"`
		TotalTokens      int64     `db:"total_tokens"`
		Requests         int64     `db:"requests"`
	}
	if err := tx.SelectContext(ctx, &rows, query, append(base, rowCap)...); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	out := make([]model.UsageExportRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.UsageExportRow{
			DeviceID:         row.DeviceID,
			BucketStart:      row.BucketStart,
			Source:           row.Source,
			AgentKey:         row.AgentKey,
			ProviderKey:      row.ProviderKey,
			ModelKey:         row.ModelKey,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			TotalTokens:      row.TotalTokens,
			Requests:         row.Requests,
		})
	}
	return out, total, nil
}

// ---------------------------------------------------------------------------
// 删除（水位 / 墓碑，契约 §4.7）
// ---------------------------------------------------------------------------

func (r *usageTokenRepo) DeleteAll(ctx context.Context, userID string, clearedBefore time.Time) (int64, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM usage_token_buckets WHERE user_id = $1`, userID)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()
	// 水位单调前进：重复全量删除不会把 cleared_before 往回拨。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_token_watermarks (user_id, cleared_before)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET
			cleared_before = GREATEST(usage_token_watermarks.cleared_before, EXCLUDED.cleared_before),
			updated_at = now()`, userID, clearedBefore); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (r *usageTokenRepo) DeleteByDevice(ctx context.Context, userID, deviceID string, clearedBefore time.Time) (int64, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先推进水位：设备行不存在时 0 行命中 → 回滚 + 404，桶删除不落库。
	res, err := tx.ExecContext(ctx, `
		UPDATE usage_devices
		SET cleared_before = GREATEST(COALESCE(cleared_before, '-infinity'::timestamptz), $3)
		WHERE user_id = $1 AND device_id = $2`, userID, deviceID, clearedBefore)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, model.ErrUsageDeviceNotFound
	}
	res, err = tx.ExecContext(ctx,
		`DELETE FROM usage_token_buckets WHERE user_id = $1 AND device_id = $2`, userID, deviceID)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (r *usageTokenRepo) DeleteWindow(ctx context.Context, userID string, deviceID *string, fromBucket, toBucket time.Time) (int64, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 墓碑区间是闭区间（契约 §3.4：from_bucket <= bucket_start <= to_bucket
	// 即丢弃），删除用同一闭区间，保证"删掉的"与"以后拒收的"完全一致。
	delQuery := `DELETE FROM usage_token_buckets
		WHERE user_id = $1 AND bucket_start >= $2 AND bucket_start <= $3`
	delArgs := []any{userID, fromBucket, toBucket}
	if deviceID != nil {
		delQuery += ` AND device_id = $4`
		delArgs = append(delArgs, *deviceID)
	}
	res, err := tx.ExecContext(ctx, delQuery, delArgs...)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_token_tombstones (user_id, device_id, from_bucket, to_bucket)
		VALUES ($1, $2, $3, $4)`, userID, deviceID, fromBucket, toBucket); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// 设备管理
// ---------------------------------------------------------------------------

const usageDeviceColumns = `user_id, device_id, label, platform, app_version,
	consent_version, consented_at, cleared_before, created_at, last_seen_at, revoked_at`

// usageDeviceRow 镜像 model.UsageDevice，供 StructScan。
type usageDeviceRow struct {
	UserID         string     `db:"user_id"`
	DeviceID       string     `db:"device_id"`
	Label          *string    `db:"label"`
	Platform       *string    `db:"platform"`
	AppVersion     *string    `db:"app_version"`
	ConsentVersion *string    `db:"consent_version"`
	ConsentedAt    *time.Time `db:"consented_at"`
	ClearedBefore  *time.Time `db:"cleared_before"`
	CreatedAt      time.Time  `db:"created_at"`
	LastSeenAt     *time.Time `db:"last_seen_at"`
	RevokedAt      *time.Time `db:"revoked_at"`
}

func (r *usageDeviceRow) toModel() *model.UsageDevice {
	return &model.UsageDevice{
		UserID:         r.UserID,
		DeviceID:       r.DeviceID,
		Label:          r.Label,
		Platform:       r.Platform,
		AppVersion:     r.AppVersion,
		ConsentVersion: r.ConsentVersion,
		ConsentedAt:    r.ConsentedAt,
		ClearedBefore:  r.ClearedBefore,
		CreatedAt:      r.CreatedAt,
		LastSeenAt:     r.LastSeenAt,
		RevokedAt:      r.RevokedAt,
	}
}

func (r *usageTokenRepo) UpsertDevice(ctx context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDevice, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 已存在时只更新非空字段（COALESCE）：客户端补登记不该清掉用户已填的
	// label。撤销设备命中此路径时整个写入回滚 → 403 device_revoked。
	var row usageDeviceRow
	err = tx.GetContext(ctx, &row, `
		INSERT INTO usage_devices (user_id, device_id, label, platform, app_version, consent_version, consented_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, device_id) DO UPDATE SET
			label           = COALESCE(EXCLUDED.label, usage_devices.label),
			platform        = COALESCE(EXCLUDED.platform, usage_devices.platform),
			app_version     = COALESCE(EXCLUDED.app_version, usage_devices.app_version),
			consent_version = COALESCE(EXCLUDED.consent_version, usage_devices.consent_version),
			consented_at    = COALESCE(EXCLUDED.consented_at, usage_devices.consented_at)
		RETURNING `+usageDeviceColumns,
		userID, in.DeviceID, in.Label, in.Platform, in.AppVersion, in.ConsentVersion, in.ConsentedAt)
	if err != nil {
		return nil, err
	}
	if row.RevokedAt != nil {
		return nil, model.ErrUsageDeviceRevoked
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return row.toModel(), nil
}

func (r *usageTokenRepo) ListDevices(ctx context.Context, userID string) ([]model.UsageDevice, error) {
	var rows []usageDeviceRow
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT `+usageDeviceColumns+` FROM usage_devices WHERE user_id = $1 ORDER BY created_at, device_id`,
		userID); err != nil {
		return nil, err
	}
	out := make([]model.UsageDevice, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row.toModel())
	}
	return out, nil
}

func (r *usageTokenRepo) UpdateDeviceLabel(ctx context.Context, userID, deviceID string, label *string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE usage_devices SET label = $3 WHERE user_id = $1 AND device_id = $2`,
		userID, deviceID, label)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrUsageDeviceNotFound
	}
	return nil
}

func (r *usageTokenRepo) RevokeDevice(ctx context.Context, userID, deviceID string, purge bool) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 撤销幂等：已撤销的行再撤销一次仍是 now() 覆盖，对外语义不变。
	res, err := tx.ExecContext(ctx,
		`UPDATE usage_devices SET revoked_at = now() WHERE user_id = $1 AND device_id = $2`,
		userID, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.ErrUsageDeviceNotFound
	}
	if purge {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM usage_token_buckets WHERE user_id = $1 AND device_id = $2`,
			userID, deviceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// 保留期
// ---------------------------------------------------------------------------

// usageSweepBatchSize 是保留期清理的单批行数（PR #61 review M2）：单语句
// DELETE 全量过期行会持锁过久，分批循环让每批成为独立短事务；批次间
// ctx 取消即中断（下轮 tick 续跑，DELETE 幂等）。
const usageSweepBatchSize = 10000

func (r *usageTokenRepo) SweepRetention(ctx context.Context, cutoff time.Time) (int64, int64, error) {
	// buckets 表单行无代理主键，按 ctid 分批；tombstones 有 BIGSERIAL id。
	buckets, err := r.sweepBatched(ctx,
		`DELETE FROM usage_token_buckets WHERE ctid IN (
			SELECT ctid FROM usage_token_buckets WHERE bucket_start < $1 LIMIT $2)`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	tombstones, err := r.sweepBatched(ctx,
		`DELETE FROM usage_token_tombstones WHERE id IN (
			SELECT id FROM usage_token_tombstones WHERE to_bucket < $1 LIMIT $2)`, cutoff)
	if err != nil {
		return buckets, 0, err
	}
	return buckets, tombstones, nil
}

// sweepBatched 循环执行带 LIMIT 子查询的 DELETE，直到删净；where 子句走
// 048 的 sweep 索引（bucket_start / to_bucket）。
func (r *usageTokenRepo) sweepBatched(ctx context.Context, query string, cutoff time.Time) (int64, error) {
	var total int64
	for {
		res, err := r.db.ExecContext(ctx, query, cutoff, usageSweepBatchSize)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < usageSweepBatchSize {
			return total, nil
		}
	}
}
