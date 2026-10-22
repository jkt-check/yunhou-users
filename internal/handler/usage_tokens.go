package handler

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/service"
)

// usageTokenService 是 handler 依赖的 service 面，本地声明以便测试注入
// 手写 mock（与 usage.go 的 usageService 同模式）。BucketSize/RetentionDays
// 是部署级配置，handler 的摄入校验需要与 service 读到同一份值。
type usageTokenService interface {
	BucketSize() string
	RetentionDays() int
	ExportMaxRows() int
	Ingest(ctx context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error)
	Summary(ctx context.Context, userID, from, to string, f model.UsageTokenFilter) (*model.UsageTokenSummary, error)
	Breakdown(ctx context.Context, userID, dimension, from, to string, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, bool, error)
	Timeseries(ctx context.Context, userID, interval, groupBy, tz, from, to string, f model.UsageTokenFilter) (*model.UsageTimeseriesResult, error)
	Coverage(ctx context.Context, userID string) (*model.UsageCoverage, error)
	Export(ctx context.Context, userID, from, to string, f model.UsageTokenFilter, allowTruncated bool) (rows []model.UsageExportRow, total int64, truncated bool, err error)
	Delete(ctx context.Context, userID, deviceID, from, to string) (*model.UsageDeleteResult, error)
	RegisterDevice(ctx context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDeviceView, error)
	ListDevices(ctx context.Context, userID string) ([]model.UsageDeviceView, error)
	RenameDevice(ctx context.Context, userID, deviceID string, label *string) error
	RevokeDevice(ctx context.Context, userID, deviceID string, purge bool) error
}

// UsageTokenHandler 服务 kaya token 用量统计全部端点（契约 yunhou-terminal
// docs/api/usage-stats.md v1.2）：摄入 batch/backfill、查询
// summary/breakdown/timeseries/coverage/export、删除、设备管理。
// 身份只从 JWT 取（middleware.ContextUserID），请求体里的 user_id 一律忽略。
type UsageTokenHandler struct {
	svc usageTokenService
}

func NewUsageTokenHandler(svc usageTokenService) *UsageTokenHandler {
	return &UsageTokenHandler{svc: svc}
}

// ---------------------------------------------------------------------------
// 响应 envelope（契约 §8：成功 {code:0,data,message:"ok"}；失败带
// error 符号名 + request_id）
// ---------------------------------------------------------------------------

// writeUsageTokenError 输出契约 §8 的错误形状。index 非空时 data =
// {"index":n}（invalid_bucket 等逐桶错误），否则 data = null。request_id
// 由本 handler 家族自行生成（本仓没有 request-id 中间件）。
func writeUsageTokenError(c *gin.Context, status int, code, message string, index *int) {
	var data any
	if index != nil {
		data = gin.H{"index": *index}
	}
	c.JSON(status, gin.H{
		"code":       status,
		"error":      code,
		"message":    message,
		"data":       data,
		"request_id": "req_" + uuid.NewString(),
	})
}

func writeUsageTokenErr(c *gin.Context, e *model.UsageTokenError) {
	// Data（v1.2 export_too_large 的结构化载荷）优先于 Index。
	if e.Data != nil {
		writeUsageTokenErrorData(c, e.Status, e.Code, e.Message, e.Data)
		return
	}
	writeUsageTokenError(c, e.Status, e.Code, e.Message, e.Index)
}

func writeUsageTokenErrorData(c *gin.Context, status int, code, message string, data any) {
	c.JSON(status, gin.H{
		"code":       status,
		"error":      code,
		"message":    message,
		"data":       data,
		"request_id": "req_" + uuid.NewString(),
	})
}

// mapUsageTokenError 统一映射 service 错误：契约错误（UsageTokenError）直出；
// 设备哨兵 → 403/404；其余 → 500 internal_error（细节只进日志）。
func (h *UsageTokenHandler) mapError(c *gin.Context, err error) {
	var ute *model.UsageTokenError
	if errors.As(err, &ute) {
		writeUsageTokenErr(c, ute)
		return
	}
	if errors.Is(err, model.ErrUsageDeviceRevoked) {
		writeUsageTokenError(c, http.StatusForbidden, "device_revoked", "device is revoked", nil)
		return
	}
	if errors.Is(err, model.ErrUsageDeviceNotFound) {
		writeUsageTokenError(c, http.StatusNotFound, "not_found", "device not found", nil)
		return
	}
	log.Printf("usage tokens: %s %s: %v", c.Request.Method, c.FullPath(), err)
	writeUsageTokenError(c, http.StatusInternalServerError, "internal_error", "internal error", nil)
}

// ---------------------------------------------------------------------------
// 摄入（POST /user/usage/tokens/batch 与 /backfill）
// ---------------------------------------------------------------------------

// usageDeviceIDPattern 是契约 §3.2 的 device_id 形态。
var usageDeviceIDPattern = regexp.MustCompile(`^d_[a-z0-9]{8,32}$`)

// usageAgentKeyPattern 是 agent_key 字符集（§3.2：1–64，[A-Za-z0-9._-]）。
var usageAgentKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// usageTokenBatchRaw 是摄入请求体的原始形状。数值字段用 json.Number，
// 使"非整数/负数"能被逐桶校验归为 invalid_bucket（带 index），而不是在
// JSON 解码层笼统地变成 invalid_request。未知字段（含 user_id）默认忽略
// —— 契约 §2.2 向前兼容 + §1.2 身份只信 JWT。
type usageTokenBatchRaw struct {
	SchemaVersion *int                  `json:"schema_version"`
	DeviceID      string                `json:"device_id"`
	BatchID       string                `json:"batch_id"`
	Backfill      bool                  `json:"backfill"`
	MaxDays       json.Number           `json:"max_days"`
	Buckets       []usageTokenBucketRaw `json:"buckets"`
}

type usageTokenBucketRaw struct {
	BucketStart      string      `json:"bucket_start"`
	BucketSize       string      `json:"bucket_size"`
	Source           string      `json:"source"`
	AgentKey         string      `json:"agent_key"`
	ProviderKey      string      `json:"provider_key"`
	ModelKey         string      `json:"model_key"`
	PromptTokens     json.Number `json:"prompt_tokens"`
	CompletionTokens json.Number `json:"completion_tokens"`
	TotalTokens      json.Number `json:"total_tokens"`
	Requests         json.Number `json:"requests"`
}

// PostBatch 处理 POST /user/usage/tokens/batch。
func (h *UsageTokenHandler) PostBatch(c *gin.Context) { h.ingest(c, false) }

// PostBackfill 处理 POST /user/usage/tokens/backfill（§3.6：max_days 默认
// 30，上限 180；其余校验/幂等语义与 batch 相同）。
func (h *UsageTokenHandler) PostBackfill(c *gin.Context) { h.ingest(c, true) }

// ingest 的校验顺序即契约 §3.2 的拒绝顺序；任一失败整批 400 且零写入
// （全或无：校验全部在 DB 之前完成，service 只在全部通过后被调用一次）。
func (h *UsageTokenHandler) ingest(c *gin.Context, isBackfill bool) {
	userID := c.GetString(middleware.ContextUserID)

	// 1. body ≤ 256 KiB；超限 → payload_too_large；JSON 解析失败 →
	//    invalid_request。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, model.UsageTokenMaxBatchBody)
	var req usageTokenBatchRaw
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeUsageTokenError(c, http.StatusBadRequest, "payload_too_large", "request body exceeds 256 KiB", nil)
			return
		}
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "request body is not valid JSON", nil)
		return
	}

	// 2. schema_version：缺失/非数 → invalid_request（解码层已拒非数）；
	//    >1 → unsupported_schema_version；≠1 → invalid_request。
	if req.SchemaVersion == nil {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "schema_version is required", nil)
		return
	}
	if *req.SchemaVersion > 1 {
		writeUsageTokenError(c, http.StatusBadRequest, "unsupported_schema_version",
			fmt.Sprintf("schema_version %d is not supported (max 1)", *req.SchemaVersion), nil)
		return
	}
	if *req.SchemaVersion != 1 {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "schema_version must be 1", nil)
		return
	}

	// 3. device_id 形态。
	if !usageDeviceIDPattern.MatchString(req.DeviceID) {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request",
			"device_id must match ^d_[a-z0-9]{8,32}$", nil)
		return
	}

	// 4. buckets 条数 1–500。
	if len(req.Buckets) == 0 {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "buckets must contain at least 1 entry", nil)
		return
	}
	if len(req.Buckets) > model.UsageTokenMaxBuckets {
		writeUsageTokenError(c, http.StatusBadRequest, "too_many_buckets",
			fmt.Sprintf("buckets must not exceed %d entries", model.UsageTokenMaxBuckets), nil)
		return
	}

	// backfill 的 max_days（默认 30，合法区间 1–180；越界 → invalid_request，
	// 显式拒绝好过静默改语义）。
	maxDays := model.UsageTokenBackfillDefaultDays
	if isBackfill && req.MaxDays != "" {
		n, err := req.MaxDays.Int64()
		if err != nil || n < 1 || n > model.UsageTokenBackfillMaxDays {
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("max_days must be an integer between 1 and %d", model.UsageTokenBackfillMaxDays), nil)
			return
		}
		maxDays = int(n)
	}

	// 5. bucket_size 必须等于部署配置（防双计 MUST-1，§6）。
	bucketSize := h.svc.BucketSize()
	for i := range req.Buckets {
		if req.Buckets[i].BucketSize != bucketSize {
			writeUsageTokenError(c, http.StatusBadRequest, "bucket_size_mismatch",
				fmt.Sprintf("buckets[%d].bucket_size %q does not match server config %q; fetch /user/usage/tokens/coverage first",
					i, req.Buckets[i].BucketSize, bucketSize), &i)
			return
		}
	}

	// 6. 逐桶校验。too_old 阈值：batch = 保留期；backfill = min(max_days,
	//    保留期)（回填窗口不能越过保留期，否则传进来也立刻被 sweeper 删掉）。
	now := time.Now().UTC()
	oldDays := h.svc.RetentionDays()
	if isBackfill && maxDays < oldDays {
		oldDays = maxDays
	}
	tooOldBefore := now.Add(-time.Duration(oldDays) * 24 * time.Hour)

	buckets := make([]model.UsageTokenBucket, 0, len(req.Buckets))
	for i := range req.Buckets {
		b, verr := h.validateBucket(&req.Buckets[i], i, bucketSize, now, tooOldBefore)
		if verr != nil {
			writeUsageTokenErr(c, verr)
			return
		}
		buckets = append(buckets, *b)
	}

	// 7. 同请求内桶 key 重复 → duplicate_bucket_key。
	seen := make(map[string]int, len(buckets))
	for i := range buckets {
		key := usageTokenBucketKey(&buckets[i])
		if first, dup := seen[key]; dup {
			writeUsageTokenError(c, http.StatusBadRequest, "duplicate_bucket_key",
				fmt.Sprintf("buckets[%d] duplicates the key of buckets[%d]; merge before uploading", i, first), &i)
			return
		}
		seen[key] = i
	}

	// 8. 设备检查 / 9. 水位墓碑丢弃 / 10. 单事务 upsert 都在 service/repo
	//    的一个事务里完成。
	result, err := h.svc.Ingest(c.Request.Context(), userID, model.UsageTokenIngestInput{
		DeviceID: req.DeviceID,
		BatchID:  req.BatchID,
		Buckets:  buckets,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": result, "message": "ok"})
}

// usageTokenBucketKey 是请求内去重键（契约 §3.2：bucket_start +
// bucket_size + source + agent_key + provider_key + model_key）。
func usageTokenBucketKey(b *model.UsageTokenBucket) string {
	return b.BucketStart.UTC().Format(time.RFC3339Nano) + "\x00" + b.BucketSize + "\x00" + b.Source + "\x00" +
		b.AgentKey + "\x00" + b.ProviderKey + "\x00" + b.ModelKey
}

// validateBucket 做逐桶校验（契约 §3.2 字段约束 + §3.5 服务端兜底）。
// 通过时返回可落库的桶（含 total_tokens 兜底）；失败返回带 index 的错误。
func (h *UsageTokenHandler) validateBucket(raw *usageTokenBucketRaw, index int, bucketSize string, now, tooOldBefore time.Time) (*model.UsageTokenBucket, *model.UsageTokenError) {
	fail := func(code, msg string) *model.UsageTokenError {
		return &model.UsageTokenError{Status: http.StatusBadRequest, Code: code,
			Message: fmt.Sprintf("buckets[%d].%s", index, msg), Index: &index}
	}

	// bucket_start：RFC3339Nano 同时接受 Z 与 +00:00（§2.2 ts 形态）；必须
	// 是 UTC 边界（契约 §3.2「RFC3339 UTC 边界」），且按部署粒度对齐。
	start, err := time.Parse(time.RFC3339Nano, raw.BucketStart)
	if err != nil {
		return nil, fail("invalid_bucket", "bucket_start must be RFC3339 (e.g. 2026-10-20T01:00:00Z)")
	}
	if _, off := start.Zone(); off != 0 {
		return nil, fail("invalid_bucket", "bucket_start must be UTC (Z or +00:00 offset)")
	}
	start = start.UTC()
	if bucketSize == model.UsageBucketSizeDay {
		if start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 || start.Nanosecond() != 0 {
			return nil, fail("invalid_bucket", "bucket_start must be aligned to UTC 00:00:00 for day buckets")
		}
	} else if start.Minute() != 0 || start.Second() != 0 || start.Nanosecond() != 0 {
		return nil, fail("invalid_bucket", "bucket_start must be aligned to the hour for hour buckets")
	}
	if start.After(now.Add(time.Hour)) {
		return nil, fail("future_bucket", "bucket_start is more than 1h ahead of server time")
	}
	if start.Before(tooOldBefore) {
		return nil, fail("too_old", "bucket_start is beyond the retention/backfill window")
	}

	if raw.Source != model.UsageSourceChat && raw.Source != model.UsageSourceGateway {
		return nil, fail("invalid_bucket", "source must be chat|gateway")
	}
	if len(raw.AgentKey) < 1 || len(raw.AgentKey) > 64 || !usageAgentKeyPattern.MatchString(raw.AgentKey) {
		return nil, fail("invalid_bucket", "agent_key must be 1-64 chars of [A-Za-z0-9._-]")
	}
	// 契约 §3.2 的长度上限按字符计（中文模型名/标签不按字节误拒）。
	if utf8.RuneCountInString(raw.ProviderKey) < 1 || utf8.RuneCountInString(raw.ProviderKey) > 128 || usageHasControlChar(raw.ProviderKey) {
		return nil, fail("invalid_bucket", "provider_key must be 1-128 chars without control characters")
	}
	if utf8.RuneCountInString(raw.ModelKey) < 1 || utf8.RuneCountInString(raw.ModelKey) > 200 || usageHasControlChar(raw.ModelKey) {
		return nil, fail("invalid_bucket", "model_key must be 1-200 chars without control characters")
	}

	prompt, cerr := usageTokenCount(raw.PromptTokens, "prompt_tokens", index)
	if cerr != nil {
		return nil, cerr
	}
	completion, cerr := usageTokenCount(raw.CompletionTokens, "completion_tokens", index)
	if cerr != nil {
		return nil, cerr
	}
	total, cerr := usageTokenCount(raw.TotalTokens, "total_tokens", index)
	if cerr != nil {
		return nil, cerr
	}
	// 服务端兜底（§2.5/§3.5）：total 缺失或为 0 时取 prompt+completion。
	if total == 0 {
		total = prompt + completion
	}
	requests, cerr := usageTokenCount(raw.Requests, "requests", index)
	if cerr != nil {
		return nil, cerr
	}
	if requests < 1 {
		return nil, fail("invalid_bucket", "requests must be an integer >= 1")
	}

	return &model.UsageTokenBucket{
		Index:            index,
		BucketStart:      start,
		BucketSize:       bucketSize,
		Source:           raw.Source,
		AgentKey:         raw.AgentKey,
		ProviderKey:      raw.ProviderKey,
		ModelKey:         raw.ModelKey,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		Requests:         requests,
	}, nil
}

// usageTokenCount 解析一个 token 计数：缺失按 0，非整数/负数 →
// invalid_bucket（带 index）。
func usageTokenCount(n json.Number, field string, index int) (int64, *model.UsageTokenError) {
	if n == "" {
		return 0, nil
	}
	v, err := n.Int64()
	if err != nil || v < 0 {
		return 0, &model.UsageTokenError{Status: http.StatusBadRequest, Code: "invalid_bucket",
			Message: fmt.Sprintf("buckets[%d].%s must be an integer >= 0", index, field), Index: &index}
	}
	return v, nil
}

// usageHasControlChar 拒绝 provider_key / model_key 里的控制字符（§3.2）。
func usageHasControlChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// usageTokenFilterFromQuery 读 §4.1 的通用过滤参数（可重复的走 QueryArray）。
func usageTokenFilterFromQuery(c *gin.Context) model.UsageTokenFilter {
	return model.UsageTokenFilter{
		Source:       c.DefaultQuery("source", "all"),
		Agents:       c.QueryArray("agent"),
		ProviderKeys: c.QueryArray("provider_key"),
		ModelKeys:    c.QueryArray("model_key"),
		DeviceIDs:    c.QueryArray("device_id"),
	}
}

// GetSummary 处理 GET /user/usage/tokens/summary。
func (h *UsageTokenHandler) GetSummary(c *gin.Context) {
	sum, err := h.svc.Summary(c.Request.Context(), c.GetString(middleware.ContextUserID),
		c.Query("from"), c.Query("to"), usageTokenFilterFromQuery(c))
	if err != nil {
		h.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": sum, "message": "ok"})
}

// GetBreakdown 处理 GET /user/usage/tokens/breakdown。limit 默认 50、最大
// 100（service 侧钳制）；limit 非正整数 → invalid_request。
func (h *UsageTokenHandler) GetBreakdown(c *gin.Context) {
	limit := 0 // 0 = 交给 service 用默认值
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "limit must be a positive integer", nil)
			return
		}
		limit = n
	}
	items, truncated, err := h.svc.Breakdown(c.Request.Context(), c.GetString(middleware.ContextUserID),
		c.Query("dimension"), c.Query("from"), c.Query("to"), usageTokenFilterFromQuery(c), limit)
	if err != nil {
		h.mapError(c, err)
		return
	}
	if items == nil {
		items = []model.UsageBreakdownItem{}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"items": items, "truncated": truncated}, "message": "ok"})
}

// GetTimeseries 处理 GET /user/usage/tokens/timeseries。interval 默认 day、
// group_by 默认 none、tz 默认 UTC（§4.4）。
func (h *UsageTokenHandler) GetTimeseries(c *gin.Context) {
	res, err := h.svc.Timeseries(c.Request.Context(), c.GetString(middleware.ContextUserID),
		c.DefaultQuery("interval", "day"), c.DefaultQuery("group_by", "none"),
		c.DefaultQuery("tz", "UTC"), c.Query("from"), c.Query("to"), usageTokenFilterFromQuery(c))
	if err != nil {
		h.mapError(c, err)
		return
	}
	if res.Items == nil {
		res.Items = []model.UsageTimeseriesItem{}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res, "message": "ok"})
}

// GetCoverage 处理 GET /user/usage/tokens/coverage（兼 config discovery）。
func (h *UsageTokenHandler) GetCoverage(c *gin.Context) {
	cov, err := h.svc.Coverage(c.Request.Context(), c.GetString(middleware.ContextUserID))
	if err != nil {
		h.mapError(c, err)
		return
	}
	if cov.Devices == nil {
		cov.Devices = []model.UsageDeviceView{}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": cov, "message": "ok"})
}

// usageExportResultHeaders 是 v1.2 §4.6 的导出头集合（每个 200 导出响应
// 恒定携带，未截断时 X-Result-Truncated 为 false）。
const usageExportExposeHeaders = "X-Result-Truncated, X-Result-Returned, X-Result-Limit"

// GetExport 处理 GET /user/usage/tokens/export。csv = 直接文件流（无
// envelope）；json = envelope，data 带 items/truncated/returned/limit。
// 超限语义（v1.2 §4.6，fail-closed）：service 在同一数据库快照里先计数，
// total > EXPORT_MAX_ROWS 且未带 allow_truncated=true → 400
// export_too_large（JSON envelope，不设任何 CSV 头——Content-Type 自然
// 不是 text/csv，响应体零 CSV 数据行）；显式 allow_truncated=true 才返回
// 按固定列序截断的前 N 行。
//
// CORS：请求带 Origin 头时（200 与 400 都要）补
// Access-Control-Expose-Headers，否则浏览器 fetch 读不到三个 X-Result-*
// 头。本仓没有 CORS 中间件，这里不发明 Allow-Origin 策略——跨域部署还需
// 运维侧在 nginx/网关配置 Access-Control-Allow-Origin，不在本契约范围。
func (h *UsageTokenHandler) GetExport(c *gin.Context) {
	if c.GetHeader("Origin") != "" {
		c.Header("Access-Control-Expose-Headers", usageExportExposeHeaders)
	}
	format := c.DefaultQuery("format", "csv")
	if format != "csv" && format != "json" {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "format must be csv|json", nil)
		return
	}
	userID := c.GetString(middleware.ContextUserID)
	fromStr, toStr := c.Query("from"), c.Query("to")
	// allow_truncated 只认显式 "true"，缺省/其它值 = false（截断是
	// opt-in，绝不默认，契约 §4.6）。
	allowTruncated := c.Query("allow_truncated") == "true"

	rows, _, truncated, err := h.svc.Export(c.Request.Context(), userID, fromStr, toStr, usageTokenFilterFromQuery(c), allowTruncated)
	if err != nil {
		// export_too_large 也走这里：尚未设置任何 CSV 头，mapError 输出
		// JSON envelope（data 带 max_rows/from/to）。
		h.mapError(c, err)
		return
	}
	if rows == nil {
		rows = []model.UsageExportRow{}
	}

	// 三个导出头恒定携带（截断与否都设），且必须在写 body 第一个字节
	// 之前——CSV 是流式响应，头一旦提交就无法再改（v1.2 §4.6 的论证）。
	c.Header("X-Result-Truncated", strconv.FormatBool(truncated))
	c.Header("X-Result-Returned", strconv.Itoa(len(rows)))
	c.Header("X-Result-Limit", strconv.Itoa(h.svc.ExportMaxRows()))

	if format == "json" {
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
			"items": rows, "truncated": truncated,
			"returned": len(rows), "limit": h.svc.ExportMaxRows(),
		}, "message": "ok"})
		return
	}

	// 文件名带生效窗口；from/to 缺省时按 service 同一规则（最近 90 天）
	// 解析出实际日期，避免文件名出现空串。
	from, to, werr := service.ParseUsageTokenWindow(fromStr, toStr, time.Now())
	if werr != nil {
		// svc.Export 已通过同样的校验，理论上不可达；兜底走统一映射。
		h.mapError(c, werr)
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="kaya-usage-%s-%s.csv"`,
		from.Format("2006-01-02"), to.Format("2006-01-02")))
	c.Status(http.StatusOK)

	w := csv.NewWriter(c.Writer)
	// 列序钉死（§4.6），首列 device_id 供多设备拆分。
	_ = w.Write([]string{"device_id", "bucket_start", "source", "agent_key", "provider_key",
		"model_key", "prompt_tokens", "completion_tokens", "total_tokens", "requests"})
	for i := range rows {
		r := &rows[i]
		_ = w.Write([]string{
			usageCSVTextCell(r.DeviceID),
			r.BucketStart.UTC().Format(time.RFC3339),
			usageCSVTextCell(r.Source),
			usageCSVTextCell(r.AgentKey),
			usageCSVTextCell(r.ProviderKey),
			usageCSVTextCell(r.ModelKey),
			strconv.FormatInt(r.PromptTokens, 10),
			strconv.FormatInt(r.CompletionTokens, 10),
			strconv.FormatInt(r.TotalTokens, 10),
			strconv.FormatInt(r.Requests, 10),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		log.Printf("usage tokens: export csv write: %v", err)
	}
}

// usageCSVTextCell 是 CSV 公式注入防护（§4.6）：以 = + - @ 开头的文本
// 单元格前置 '。对所有文本列统一生效。
func usageCSVTextCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@':
		return "'" + s
	}
	return s
}

// DeleteTokens 处理 DELETE /user/usage/tokens（§4.7：无参 = 全量+用户级
// 水位；device_id = 设备级水位；from/to = 窗口墓碑，可叠加 device_id）。
func (h *UsageTokenHandler) DeleteTokens(c *gin.Context) {
	deviceID := c.Query("device_id")
	if deviceID != "" && !usageDeviceIDPattern.MatchString(deviceID) {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request",
			"device_id must match ^d_[a-z0-9]{8,32}$", nil)
		return
	}
	res, err := h.svc.Delete(c.Request.Context(), c.GetString(middleware.ContextUserID),
		deviceID, c.Query("from"), c.Query("to"))
	if err != nil {
		h.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": res, "message": "ok"})
}

// ---------------------------------------------------------------------------
// 设备管理（§4.8）
// ---------------------------------------------------------------------------

// usageDeviceRegisterRaw 是 POST /user/devices 的请求体。label 空串视为
// 未提供（不覆盖已有 label，契约 §4.8/§3.2 auto-upsert 语义一致）。
type usageDeviceRegisterRaw struct {
	DeviceID       string  `json:"device_id"`
	Label          *string `json:"label"`
	Platform       *string `json:"platform"`
	AppVersion     *string `json:"app_version"`
	ConsentVersion *string `json:"consent_version"`
	ConsentedAt    *string `json:"consented_at"`
}

// PostDevice 处理 POST /user/devices（注册/更新；已存在只更新非空字段；
// 撤销设备 → 403 device_revoked）。
func (h *UsageTokenHandler) PostDevice(c *gin.Context) {
	var req usageDeviceRegisterRaw
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "request body is not valid JSON", nil)
		return
	}
	if !usageDeviceIDPattern.MatchString(req.DeviceID) {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request",
			"device_id must match ^d_[a-z0-9]{8,32}$", nil)
		return
	}
	in := model.UsageDeviceRegisterInput{DeviceID: req.DeviceID}
	if req.Label != nil && *req.Label != "" {
		if len(*req.Label) > 128 {
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "label must be at most 128 chars", nil)
			return
		}
		in.Label = req.Label
	}
	if req.Platform != nil && *req.Platform != "" {
		switch *req.Platform {
		case "macos", "windows", "linux":
			in.Platform = req.Platform
		default:
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "platform must be macos|windows|linux", nil)
			return
		}
	}
	// app_version 与心跳口径同一上限（64），consent_version 同样收紧。
	if req.AppVersion != nil && *req.AppVersion != "" {
		if len(*req.AppVersion) > model.UsageMaxAppVersionLen {
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("app_version must be at most %d chars", model.UsageMaxAppVersionLen), nil)
			return
		}
		in.AppVersion = req.AppVersion
	}
	if req.ConsentVersion != nil && *req.ConsentVersion != "" {
		if len(*req.ConsentVersion) > 64 {
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "consent_version must be at most 64 chars", nil)
			return
		}
		in.ConsentVersion = req.ConsentVersion
	}
	if req.ConsentedAt != nil && *req.ConsentedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, *req.ConsentedAt)
		if err != nil {
			writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "consented_at must be RFC3339", nil)
			return
		}
		u := t.UTC()
		in.ConsentedAt = &u
	}

	view, err := h.svc.RegisterDevice(c.Request.Context(), c.GetString(middleware.ContextUserID), in)
	if err != nil {
		h.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": view, "message": "ok"})
}

// ListDevices 处理 GET /user/devices（含 revoked 标记）。
func (h *UsageTokenHandler) ListDevices(c *gin.Context) {
	devices, err := h.svc.ListDevices(c.Request.Context(), c.GetString(middleware.ContextUserID))
	if err != nil {
		h.mapError(c, err)
		return
	}
	if devices == nil {
		devices = []model.UsageDeviceView{}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"devices": devices}, "message": "ok"})
}

// PatchDevice 处理 PATCH /user/devices/:id（只允许改 label；不存在 →
// 404 not_found）。空 label = 清除（存 NULL）。
func (h *UsageTokenHandler) PatchDevice(c *gin.Context) {
	deviceID := c.Param("id")
	var req struct {
		Label *string `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "request body is not valid JSON", nil)
		return
	}
	if req.Label == nil {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "label is required", nil)
		return
	}
	if len(*req.Label) > 128 {
		writeUsageTokenError(c, http.StatusBadRequest, "invalid_request", "label must be at most 128 chars", nil)
		return
	}
	var label *string
	if *req.Label != "" {
		label = req.Label
	}
	if err := h.svc.RenameDevice(c.Request.Context(), c.GetString(middleware.ContextUserID), deviceID, label); err != nil {
		h.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"device_id": deviceID, "label": label}, "message": "ok"})
}

// DeleteDevice 处理 DELETE /user/devices/:id?purge=true（撤销；purge 同时
// 删除该设备历史桶；不存在 → 404）。
func (h *UsageTokenHandler) DeleteDevice(c *gin.Context) {
	deviceID := c.Param("id")
	purge := c.Query("purge") == "true"
	if err := h.svc.RevokeDevice(c.Request.Context(), c.GetString(middleware.ContextUserID), deviceID, purge); err != nil {
		h.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"device_id": deviceID, "revoked": true, "purged": purge,
	}, "message": "ok"})
}
