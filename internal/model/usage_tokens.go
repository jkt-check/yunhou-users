package model

import (
	"errors"
	"time"
)

// 本文件是 kaya token 用量统计（契约 yunhou-terminal docs/api/usage-stats.md
// v1.2）的数据模型。边界：客户端自报的聚合桶，仅供用户自助查询，不是计费
// 依据（计费走 inference 计量表组，见 migration 047 头部注释）。

const (
	// UsageBucketSizeHour / UsageBucketSizeDay 是桶粒度的全部合法值；部署级
	// 固定值来自 USAGE_TOKEN_BUCKET_SIZE（默认 hour），客户端经 coverage 的
	// bucket_size 字段自发现。
	UsageBucketSizeHour = "hour"
	UsageBucketSizeDay  = "day"
)

const (
	UsageSourceChat    = "chat"
	UsageSourceGateway = "gateway"
)

const (
	// UsageTokenMaxBatchBody 是摄入请求体上限（契约 §3.2：≤ 256 KiB）。
	UsageTokenMaxBatchBody = 256 << 10
	// UsageTokenMaxBuckets 是单批桶数上限（契约 §3.2：1–500）。
	UsageTokenMaxBuckets = 500
	// UsageTokenMaxWindowDays 是查询/删除窗口上限（契约 §4.1：180 天，
	// 与本地保留对齐）。
	UsageTokenMaxWindowDays = 180
	// UsageTokenDefaultWindowDays 是 from/to 缺省时的回看窗口（90 天）。
	UsageTokenDefaultWindowDays = 90
	// UsageTokenBackfillDefaultDays / MaxDays 是 backfill 的默认/上限天数
	//（契约 §3.6：默认 30，最多 180，与本地保留上限对齐）。
	UsageTokenBackfillDefaultDays = 30
	UsageTokenBackfillMaxDays     = 180
)

// ErrUsageDeviceNotFound / ErrUsageDeviceRevoked 是设备管理的哨兵错误
// （与 model.ErrSessionAlreadyRevoked 同模式：repo 产生，service 透传，
// handler 映射为 404 not_found / 403 device_revoked）。
var (
	ErrUsageDeviceNotFound = errors.New("usage device not found")
	ErrUsageDeviceRevoked  = errors.New("usage device revoked")
)

// UsageTokenError 携带契约 §8 的符号错误码。handler 统一映射：
// Status/Code 直出，Message 进 envelope.message；data 的取舍顺序是
// Data（export_too_large 等结构化载荷，v1.2）> Index（invalid_bucket
// 专用）> null。
type UsageTokenError struct {
	Status  int
	Code    string
	Message string
	Index   *int
	// Data 是结构化错误载荷（v1.2 §4.6：export_too_large 至少带
	// max_rows / from / to，from/to 为生效窗口的 YYYY-MM-DD）。
	Data map[string]any
}

func (e *UsageTokenError) Error() string { return e.Message }

// UsageTokenBucket 是一个通过全部校验、等待落库的聚合桶。上传的是该桶的
// 累计值（不是 delta），服务端 ON CONFLICT 覆盖（契约 §3.3）。
type UsageTokenBucket struct {
	// Index 是该桶在请求体 buckets[] 里的下标，仅供 discarded 回报；
	// 不入库。
	Index            int
	BucketStart      time.Time // UTC，已按部署粒度对齐
	BucketSize       string    // hour | day（必等于部署配置）
	Source           string    // chat | gateway
	AgentKey         string
	ProviderKey      string
	ModelKey         string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64 // 服务端兜底后恒 = 有效 total（§2.5）
	Requests         int64
}

// UsageTokenIngestInput 是 handler 完成全部无状态校验后交给 service 的
// 摄入载荷。user_id 不在其中——身份只来自 JWT sub（契约 §1.2：请求体里的
// user_id 一律忽略）。
type UsageTokenIngestInput struct {
	DeviceID string
	// BatchID 是客户端建议的 ULID/UUID 追踪 id，只用于日志，不作为幂等键
	//（唯一键是桶 key，契约 §3.2）。
	BatchID string
	Buckets []UsageTokenBucket
}

// UsageDiscardedBucket 报告一个被删除水位/墓碑丢弃的桶（契约 §3.4）——
// 不是校验失败，不导致整批 400。
type UsageDiscardedBucket struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"` // 目前恒为 "cleared"
}

// UsageIngestResult 是 POST /user/usage/tokens/{batch,backfill} 的 data。
// Accepted = 通过校验且未被丢弃的桶数；Updated = 实际写入/覆盖的行数
// （正常等于 Accepted）。
type UsageIngestResult struct {
	Accepted  int                    `json:"accepted"`
	Updated   int                    `json:"updated"`
	Discarded []UsageDiscardedBucket `json:"discarded"`
}

// UsageDevice 是 usage_devices 行。Label 只来自用户手动输入（契约 §4.8：
// 不自动采集 hostname）。
type UsageDevice struct {
	UserID         string
	DeviceID       string
	Label          *string
	Platform       *string
	AppVersion     *string
	ConsentVersion *string
	ConsentedAt    *time.Time
	ClearedBefore  *time.Time
	CreatedAt      time.Time
	LastSeenAt     *time.Time
	RevokedAt      *time.Time
}

// UsageDeviceRegisterInput 是 POST /user/devices 的已校验载荷；nil 字段
// 表示"本次不提供"，upsert 时不覆盖已有值。
type UsageDeviceRegisterInput struct {
	DeviceID       string
	Label          *string
	Platform       *string
	AppVersion     *string
	ConsentVersion *string
	ConsentedAt    *time.Time
}

// UsageDeviceView 是 GET /user/devices 与 coverage.devices[] 的设备视图。
// 时间字段 nil → null；Revoked 由 revoked_at 派生。
type UsageDeviceView struct {
	DeviceID       string     `json:"device_id"`
	Label          *string    `json:"label"`
	Platform       *string    `json:"platform"`
	AppVersion     *string    `json:"app_version"`
	ConsentVersion *string    `json:"consent_version"`
	ConsentedAt    *time.Time `json:"consented_at"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	// LastBucketAt / EarliestBucket / LatestBucket 仅 coverage 填充
	//（该设备自身桶在当前 bucket_size 下的 MIN/MAX）。契约 §4.5：无桶时
	// 必须是 null 而非字段消失，故不带 omitempty。
	LastBucketAt   *time.Time `json:"last_bucket_at"`
	EarliestBucket *time.Time `json:"earliest_bucket"`
	LatestBucket   *time.Time `json:"latest_bucket"`
	Revoked        bool       `json:"revoked"`
}

// UsageTokenFilter 是 §4.1 的通用过滤参数（除固定三过滤外的可选部分）。
type UsageTokenFilter struct {
	Source       string   // "" / "all" / "chat" / "gateway"
	Agents       []string // agent=<key>，可重复
	ProviderKeys []string // provider_key=<key>，可重复
	ModelKeys    []string // model_key=<key>，可重复
	DeviceIDs    []string // device_id=<id>，可重复
}

// UsageTokenSummary 是 GET /user/usage/tokens/summary 的 data。无数据时
// 数值字段为 0、FirstBucket/LastBucket 为 null（契约 §4.2）。
type UsageTokenSummary struct {
	TotalTokens      int64      `json:"total_tokens"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	Requests         int64      `json:"requests"`
	DeviceCount      int        `json:"device_count"`
	ActiveDays       int        `json:"active_days"`
	FirstBucket      *time.Time `json:"first_bucket"`
	LastBucket       *time.Time `json:"last_bucket"`
}

// UsageBreakdownItem 是 breakdown 的一行；哪个维度的键非空取决于
// dimension（agent/provider/model/provider_model/device/source）。
// provider/provider_model 恒带 DisplayName（目录解析，缺失回退 key 原文，
// 绝不返回空串）；device 恒带 Label（可空）。
type UsageBreakdownItem struct {
	AgentKey         *string `json:"agent_key,omitempty"`
	ProviderKey      *string `json:"provider_key,omitempty"`
	DisplayName      *string `json:"display_name,omitempty"`
	ModelKey         *string `json:"model_key,omitempty"`
	DeviceID         *string `json:"device_id,omitempty"`
	Label            *string `json:"label,omitempty"`
	Source           *string `json:"source,omitempty"`
	TotalTokens      int64   `json:"total_tokens"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int64   `json:"requests"`
}

// UsageTimeseriesItem 是 timeseries 的一行。Bucket 是 tz 下周期起点日期：
// day→YYYY-MM-DD、week→周一日期、month→当月 1 号（契约 §4.4）；不补零。
type UsageTimeseriesItem struct {
	Bucket           string  `json:"bucket"`
	AgentKey         *string `json:"agent_key,omitempty"`
	ProviderKey      *string `json:"provider_key,omitempty"`
	DisplayName      *string `json:"display_name,omitempty"`
	ModelKey         *string `json:"model_key,omitempty"`
	DeviceID         *string `json:"device_id,omitempty"`
	Label            *string `json:"label,omitempty"`
	TotalTokens      int64   `json:"total_tokens"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int64   `json:"requests"`
}

// UsageTimeseriesResult 是 GET /user/usage/tokens/timeseries 的 data。
type UsageTimeseriesResult struct {
	TZ    string                `json:"tz"`
	Items []UsageTimeseriesItem `json:"items"`
}

// UsageCoverage 是 GET /user/usage/tokens/coverage 的 data，同时是客户端
// config discovery（A1）：bucket_size / retention_days / export_max_rows /
// server_time。顶层 Earliest/LatestBucket 排除已撤销设备；Devices 仍含
// revoked:true 的设备供展示（契约 §4.5，coverage 是固定三过滤的唯一例外）。
type UsageCoverage struct {
	BucketSize    string `json:"bucket_size"`
	RetentionDays int    `json:"retention_days"`
	// ExportMaxRows 是导出接口的单次行数上限（v1.2 §4.5/§4.6），客户端
	// 据此在发起全量导出前提示窗口风险，不靠撞 400 才发现。
	ExportMaxRows  int               `json:"export_max_rows"`
	ServerTime     time.Time         `json:"server_time"`
	EarliestBucket *time.Time        `json:"earliest_bucket"`
	LatestBucket   *time.Time        `json:"latest_bucket"`
	Devices        []UsageDeviceView `json:"devices"`
}

// UsageExportRow 是导出的一行；JSON 字段名与 CSV 列同名（契约 §4.6）。
type UsageExportRow struct {
	DeviceID         string    `json:"device_id"`
	BucketStart      time.Time `json:"bucket_start"` // 输出恒 RFC3339 Z
	Source           string    `json:"source"`
	AgentKey         string    `json:"agent_key"`
	ProviderKey      string    `json:"provider_key"`
	ModelKey         string    `json:"model_key"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	Requests         int64     `json:"requests"`
}

// UsageTombstoneView 是窗口删除响应里的墓碑（RFC3339 Z 时间）。
type UsageTombstoneView struct {
	DeviceID   *string   `json:"device_id"` // null = 全设备
	FromBucket time.Time `json:"from_bucket"`
	ToBucket   time.Time `json:"to_bucket"`
}

// UsageDeleteResult 是 DELETE /user/usage/tokens 的 data：全量/设备删除
// 带 ClearedBefore（Tombstone 为 null）；窗口删除带 Tombstone
// （ClearedBefore 为 null）（契约 §4.7）。
type UsageDeleteResult struct {
	DeletedBuckets int64               `json:"deleted_buckets"`
	ClearedBefore  *time.Time          `json:"cleared_before"`
	Tombstone      *UsageTombstoneView `json:"tombstone"`
}
