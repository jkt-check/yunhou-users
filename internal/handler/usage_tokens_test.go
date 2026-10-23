package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
)

// mockUsageTokenSvc 实现 handler 的本地 usageTokenService 接口（手写 mock，
// 不用外部 mock 库）。
type mockUsageTokenSvc struct {
	bucketSize    string
	retentionDays int

	ingestResult *model.UsageIngestResult
	ingestErr    error
	ingestCalls  int
	gotUserID    string
	gotIngest    model.UsageTokenIngestInput

	summary       *model.UsageTokenSummary
	summaryErr    error
	breakdown     []model.UsageBreakdownItem
	truncated     bool
	breakdownErr  error
	timeseries    *model.UsageTimeseriesResult
	timeseriesErr error
	coverage      *model.UsageCoverage
	coverageErr   error
	export        []model.UsageExportRow
	exportTotal   int64
	exportTrunc   bool
	exportErr     error
	exportMaxRows int
	del           *model.UsageDeleteResult
	delErr        error
	device        *model.UsageDeviceView
	deviceErr     error
	devices       []model.UsageDeviceView
	devicesErr    error
	renameErr     error
	revokeErr     error

	gotBreakdownArgs  []string
	gotLimit          int
	gotTimeseriesArgs []string
	gotDeleteArgs     []string
	gotPurge          bool
	gotAllowTruncated bool
	gotRenameLabel    *string
	gotRegister       model.UsageDeviceRegisterInput
}

func (m *mockUsageTokenSvc) BucketSize() string {
	if m.bucketSize == "" {
		return "hour"
	}
	return m.bucketSize
}

func (m *mockUsageTokenSvc) RetentionDays() int {
	if m.retentionDays == 0 {
		return 180
	}
	return m.retentionDays
}

func (m *mockUsageTokenSvc) ExportMaxRows() int {
	if m.exportMaxRows == 0 {
		return 500000
	}
	return m.exportMaxRows
}

func (m *mockUsageTokenSvc) Ingest(_ context.Context, userID string, in model.UsageTokenIngestInput) (*model.UsageIngestResult, error) {
	m.ingestCalls++
	m.gotUserID = userID
	m.gotIngest = in
	return m.ingestResult, m.ingestErr
}

func (m *mockUsageTokenSvc) Summary(_ context.Context, userID, from, to string, f model.UsageTokenFilter) (*model.UsageTokenSummary, error) {
	m.gotUserID = userID
	return m.summary, m.summaryErr
}

func (m *mockUsageTokenSvc) Breakdown(_ context.Context, userID, dimension, from, to string, f model.UsageTokenFilter, limit int) ([]model.UsageBreakdownItem, bool, error) {
	m.gotBreakdownArgs = []string{dimension, from, to}
	m.gotLimit = limit
	return m.breakdown, m.truncated, m.breakdownErr
}

func (m *mockUsageTokenSvc) Timeseries(_ context.Context, userID, interval, groupBy, tz, from, to string, f model.UsageTokenFilter) (*model.UsageTimeseriesResult, error) {
	m.gotTimeseriesArgs = []string{interval, groupBy, tz}
	return m.timeseries, m.timeseriesErr
}

func (m *mockUsageTokenSvc) Coverage(_ context.Context, userID string) (*model.UsageCoverage, error) {
	return m.coverage, m.coverageErr
}

func (m *mockUsageTokenSvc) Export(_ context.Context, userID, from, to string, f model.UsageTokenFilter, allowTruncated bool) ([]model.UsageExportRow, int64, bool, error) {
	m.gotAllowTruncated = allowTruncated
	return m.export, m.exportTotal, m.exportTrunc, m.exportErr
}

func (m *mockUsageTokenSvc) Delete(_ context.Context, userID, deviceID, from, to string) (*model.UsageDeleteResult, error) {
	m.gotDeleteArgs = []string{deviceID, from, to}
	return m.del, m.delErr
}

func (m *mockUsageTokenSvc) RegisterDevice(_ context.Context, userID string, in model.UsageDeviceRegisterInput) (*model.UsageDeviceView, error) {
	m.gotUserID = userID
	m.gotRegister = in
	return m.device, m.deviceErr
}

func (m *mockUsageTokenSvc) ListDevices(_ context.Context, userID string) ([]model.UsageDeviceView, error) {
	return m.devices, m.devicesErr
}

func (m *mockUsageTokenSvc) RenameDevice(_ context.Context, userID, deviceID string, label *string) error {
	m.gotRenameLabel = label
	return m.renameErr
}

func (m *mockUsageTokenSvc) RevokeDevice(_ context.Context, userID, deviceID string, purge bool) error {
	m.gotPurge = purge
	return m.revokeErr
}

// usageTokenTestEngine 装配全部端点，身份 stub 与 router 的 JWTAuth 位置一致。
func usageTokenTestEngine(svc usageTokenService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	h := NewUsageTokenHandler(svc)
	e.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserID, "user-abc")
		c.Next()
	})
	e.POST("/user/usage/tokens/batch", h.PostBatch)
	e.POST("/user/usage/tokens/backfill", h.PostBackfill)
	e.GET("/user/usage/tokens/summary", h.GetSummary)
	e.GET("/user/usage/tokens/breakdown", h.GetBreakdown)
	e.GET("/user/usage/tokens/timeseries", h.GetTimeseries)
	e.GET("/user/usage/tokens/coverage", h.GetCoverage)
	e.GET("/user/usage/tokens/export", h.GetExport)
	e.DELETE("/user/usage/tokens", h.DeleteTokens)
	e.POST("/user/devices", h.PostDevice)
	e.GET("/user/devices", h.ListDevices)
	e.PATCH("/user/devices/:id", h.PatchDevice)
	e.DELETE("/user/devices/:id", h.DeleteDevice)
	return e
}

func doUsageTokenReq(t *testing.T, e *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	return w
}

// usageTokenErrBody 解码契约 §8 的错误 envelope。
type usageTokenErrBody struct {
	Code      int             `json:"code"`
	Error     string          `json:"error"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
}

func decodeUsageTokenErr(t *testing.T, w *httptest.ResponseRecorder) usageTokenErrBody {
	t.Helper()
	var b usageTokenErrBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return b
}

// validBucketStart 返回昨天某整点（UTC），保证既不过期也不在未来。
func validBucketStart(hour int) string {
	d := time.Now().UTC().Add(-24 * time.Hour)
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

func validBucketJSON(hour int) string {
	return fmt.Sprintf(`{"bucket_start":%q,"bucket_size":"hour","source":"chat","agent_key":"chat","provider_key":"builtin","model_key":"default","prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"requests":1}`,
		validBucketStart(hour))
}

func validBatchJSON(buckets ...string) string {
	return `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","batch_id":"01J8ZK4Q7YV3M2N1P0R9S8T7","buckets":[` +
		strings.Join(buckets, ",") + `]}`
}

func TestUsageTokenBatch_Valid(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{ingestResult: &model.UsageIngestResult{
		Accepted: 1, Updated: 1, Discarded: []model.UsageDiscardedBucket{},
	}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", validBatchJSON(validBucketJSON(1)))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Code    int                     `json:"code"`
		Data    model.UsageIngestResult `json:"data"`
		Message string                  `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.Data.Accepted != 1 || resp.Data.Updated != 1 {
		t.Errorf("unexpected success envelope: %s", w.Body.String())
	}
	if svc.gotUserID != "user-abc" {
		t.Errorf("userID = %q, want JWT context value", svc.gotUserID)
	}
	// 请求体里的 user_id 一律忽略（§1.2）：身份只来自 JWT。
	if svc.gotIngest.DeviceID != "d_7f3a9c2e5b104d8e" || len(svc.gotIngest.Buckets) != 1 {
		t.Errorf("ingest input = %+v", svc.gotIngest)
	}
}

// TestUsageTokenBatch_IgnoresBodyUserID: body 带别人的 user_id 也必须按 JWT 身份摄入。
func TestUsageTokenBatch_IgnoresBodyUserID(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{ingestResult: &model.UsageIngestResult{Accepted: 1, Updated: 1}}
	e := usageTokenTestEngine(svc)
	body := `{"schema_version":1,"user_id":"someone-else","device_id":"d_7f3a9c2e5b104d8e","buckets":[` + validBucketJSON(1) + `]}`
	w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", body)
	if w.Code != http.StatusOK || svc.gotUserID != "user-abc" {
		t.Errorf("got %d, userID %q", w.Code, svc.gotUserID)
	}
}

// TestUsageTokenBatch_ValidationOrder 覆盖摄入校验顺序（§3.2 拒绝顺序）与
// 各错误码。每个用例都是整批拒绝且 service 零调用（全或无，零写入）。
func TestUsageTokenBatch_ValidationOrder(t *testing.T) {
	t.Parallel()

	bigBody := `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","buckets":[` +
		validBucketJSON(1) + `],"pad":"` + strings.Repeat("x", 260<<10) + `"}`

	futureStart := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)
	oldStart := time.Now().UTC().Add(-200 * 24 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)

	mkBucket := func(mut func(map[string]any)) string {
		b := map[string]any{
			"bucket_start": validBucketStart(1), "bucket_size": "hour", "source": "chat",
			"agent_key": "chat", "provider_key": "builtin", "model_key": "default",
			"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "requests": 1,
		}
		mut(b)
		raw, _ := json.Marshal(b)
		return string(raw)
	}

	cases := []struct {
		name     string
		body     string
		wantCode string
		wantIdx  *int // 期望 data.index；nil = data 必须为 null
	}{
		{"payload too large", bigBody, "payload_too_large", nil},
		{"trailing over-limit still payload_too_large", `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","buckets":[]}` +
			strings.Repeat(" ", 300<<10), "payload_too_large", nil},
		{"malformed json", `{"schema_version":`, "invalid_request", nil},
		{"trailing garbage after json", `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","buckets":[]} garbage`, "invalid_request", nil},
		{"missing schema_version", `{"device_id":"d_7f3a9c2e5b104d8e","buckets":[` + validBucketJSON(1) + `]}`, "invalid_request", nil},
		{"schema_version 2 beats bad device_id", `{"schema_version":2,"device_id":"nope","buckets":[]}`, "unsupported_schema_version", nil},
		{"schema_version string", `{"schema_version":"1","device_id":"d_7f3a9c2e5b104d8e","buckets":[]}`, "invalid_request", nil},
		{"schema_version 0", `{"schema_version":0,"device_id":"d_7f3a9c2e5b104d8e","buckets":[]}`, "invalid_request", nil},
		{"device_id bad", `{"schema_version":1,"device_id":"hostname-1","buckets":[]}`, "invalid_request", nil},
		{"device_id checked before bucket count", `{"schema_version":1,"device_id":"bad","buckets":[]}`, "invalid_request", nil},
		{"empty buckets", `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","buckets":[]}`, "invalid_request", nil},
		{"bucket_size mismatch", `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","buckets":[` +
			mkBucket(func(b map[string]any) { b["bucket_size"] = "day" }) + `]}`, "bucket_size_mismatch", ptrInt(0)},
		{"bucket_start not RFC3339", validBatchJSON(mkBucket(func(b map[string]any) { b["bucket_start"] = "2026-10-20 01:00" })), "invalid_bucket", ptrInt(0)},
		{"bucket_start non-UTC offset", validBatchJSON(mkBucket(func(b map[string]any) {
			d := time.Now().UTC().Add(-24 * time.Hour)
			b["bucket_start"] = time.Date(d.Year(), d.Month(), d.Day(), 9, 0, 0, 0, time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
		})), "invalid_bucket", ptrInt(0)},
		{"bucket_start unaligned", validBatchJSON(mkBucket(func(b map[string]any) {
			d := time.Now().UTC().Add(-24 * time.Hour)
			b["bucket_start"] = time.Date(d.Year(), d.Month(), d.Day(), 1, 30, 0, 0, time.UTC).Format(time.RFC3339)
		})), "invalid_bucket", ptrInt(0)},
		{"future bucket", validBatchJSON(mkBucket(func(b map[string]any) { b["bucket_start"] = futureStart })), "future_bucket", ptrInt(0)},
		{"too old", validBatchJSON(mkBucket(func(b map[string]any) { b["bucket_start"] = oldStart })), "too_old", ptrInt(0)},
		{"bad source", validBatchJSON(mkBucket(func(b map[string]any) { b["source"] = "api" })), "invalid_bucket", ptrInt(0)},
		{"empty agent_key", validBatchJSON(mkBucket(func(b map[string]any) { b["agent_key"] = "" })), "invalid_bucket", ptrInt(0)},
		{"agent_key bad chars", validBatchJSON(mkBucket(func(b map[string]any) { b["agent_key"] = "claude code" })), "invalid_bucket", ptrInt(0)},
		{"agent_key too long", validBatchJSON(mkBucket(func(b map[string]any) { b["agent_key"] = strings.Repeat("a", 65) })), "invalid_bucket", ptrInt(0)},
		{"provider_key too long", validBatchJSON(mkBucket(func(b map[string]any) { b["provider_key"] = strings.Repeat("p", 129) })), "invalid_bucket", ptrInt(0)},
		{"provider_key control char", validBatchJSON(mkBucket(func(b map[string]any) { b["provider_key"] = "preset:deepseek\n" })), "invalid_bucket", ptrInt(0)},
		{"model_key too long", validBatchJSON(mkBucket(func(b map[string]any) { b["model_key"] = strings.Repeat("m", 201) })), "invalid_bucket", ptrInt(0)},
		{"negative tokens", validBatchJSON(mkBucket(func(b map[string]any) { b["prompt_tokens"] = -1 })), "invalid_bucket", ptrInt(0)},
		{"fractional tokens", validBatchJSON(mkBucket(func(b map[string]any) { b["total_tokens"] = 1.5 })), "invalid_bucket", ptrInt(0)},
		{"requests 0", validBatchJSON(mkBucket(func(b map[string]any) { b["requests"] = 0 })), "invalid_bucket", ptrInt(0)},
		{"requests missing", validBatchJSON(mkBucket(func(b map[string]any) { delete(b, "requests") })), "invalid_bucket", ptrInt(0)},
		{"index points at the failing bucket", validBatchJSON(validBucketJSON(1),
			mkBucket(func(b map[string]any) { b["bucket_start"] = validBucketStart(2); b["requests"] = 0 })), "invalid_bucket", ptrInt(1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &mockUsageTokenSvc{}
			e := usageTokenTestEngine(svc)
			w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d (%s), want 400", w.Code, w.Body.String())
			}
			body := decodeUsageTokenErr(t, w)
			if body.Error != tc.wantCode {
				t.Errorf("error = %q, want %q (body %s)", body.Error, tc.wantCode, w.Body.String())
			}
			if body.Code != 400 {
				t.Errorf("code = %d, want 400", body.Code)
			}
			if !strings.HasPrefix(body.RequestID, "req_") {
				t.Errorf("request_id = %q, want req_ prefix", body.RequestID)
			}
			if tc.wantIdx == nil {
				if string(body.Data) != "null" {
					t.Errorf("data = %s, want null", body.Data)
				}
			} else {
				var d struct {
					Index int `json:"index"`
				}
				if err := json.Unmarshal(body.Data, &d); err != nil || d.Index != *tc.wantIdx {
					t.Errorf("data = %s, want {\"index\":%d}", body.Data, *tc.wantIdx)
				}
			}
			if svc.ingestCalls != 0 {
				t.Errorf("service called %d times; all-or-nothing requires zero DB writes on rejection", svc.ingestCalls)
			}
		})
	}
}

func ptrInt(n int) *int { return &n }

// TestUsageTokenBatch_TooManyBuckets: 501 桶 → too_many_buckets（构造时
// 保持 body < 256 KiB，避免先撞 payload_too_large）。
func TestUsageTokenBatch_TooManyBuckets(t *testing.T) {
	t.Parallel()
	parts := make([]string, 0, 501)
	for i := 0; i < 501; i++ {
		// 短字段、不同 bucket_start 防提前撞 duplicate 检查（count 检查在前）。
		parts = append(parts, fmt.Sprintf(`{"bucket_start":%q,"bucket_size":"hour","source":"chat","agent_key":"c","provider_key":"b","model_key":"m","prompt_tokens":1,"completion_tokens":0,"total_tokens":1,"requests":1}`,
			time.Now().UTC().Add(-time.Duration(i+24)*time.Hour).Truncate(time.Hour).Format(time.RFC3339)))
	}
	svc := &mockUsageTokenSvc{}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", validBatchJSON(parts...))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String()[:200])
	}
	if body := decodeUsageTokenErr(t, w); body.Error != "too_many_buckets" {
		t.Errorf("error = %q, want too_many_buckets", body.Error)
	}
}

// TestUsageTokenBatch_DuplicateKey: 同请求内桶 key 重复 →
// duplicate_bucket_key，index 指向后一个。
func TestUsageTokenBatch_DuplicateKey(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{}
	e := usageTokenTestEngine(svc)
	// 同一时刻、同一组维度键；bucket_start 写法不同（Z vs +00:00）也算重复。
	b1 := validBucketJSON(1)
	b2 := strings.Replace(b1, `:00Z"`, `:00+00:00"`, 1)
	w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", validBatchJSON(b1, b2))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d", w.Code)
	}
	body := decodeUsageTokenErr(t, w)
	if body.Error != "duplicate_bucket_key" {
		t.Errorf("error = %q", body.Error)
	}
	if svc.ingestCalls != 0 {
		t.Error("service must not be called")
	}
}

// TestUsageTokenBatch_DiscardedIsNotFailure: 水位/墓碑丢弃不算失败（§3.4）。
func TestUsageTokenBatch_DiscardedIsNotFailure(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{ingestResult: &model.UsageIngestResult{
		Accepted: 1, Updated: 1,
		Discarded: []model.UsageDiscardedBucket{{Index: 0, Reason: "cleared"}},
	}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch",
		validBatchJSON(validBucketJSON(1), validBucketJSON(2)))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"discarded":[{"index":0,"reason":"cleared"}]`) {
		t.Errorf("discarded payload missing: %s", w.Body.String())
	}
}

// TestUsageTokenBatch_TotalFallback: total_tokens=0 → 服务端兜底为
// prompt+completion（§2.5/§3.5）。
func TestUsageTokenBatch_TotalFallback(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{ingestResult: &model.UsageIngestResult{Accepted: 1, Updated: 1}}
	e := usageTokenTestEngine(svc)
	bucket := `{"bucket_start":` + fmt.Sprintf("%q", validBucketStart(1)) +
		`,"bucket_size":"hour","source":"gateway","agent_key":"claude-code","provider_key":"preset:deepseek","model_key":"deepseek-v4-pro","prompt_tokens":20,"completion_tokens":8,"total_tokens":0,"requests":2}`
	w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", validBatchJSON(bucket))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	got := svc.gotIngest.Buckets[0]
	if got.TotalTokens != 28 {
		t.Errorf("TotalTokens = %d, want 28 (20+8 fallback)", got.TotalTokens)
	}
}

// TestUsageTokenBatch_ServiceErrorMapping: device_revoked → 403；未知错误 →
// 500 internal_error 且细节不外泄。
func TestUsageTokenBatch_ServiceErrorMapping(t *testing.T) {
	t.Parallel()
	t.Run("device revoked", func(t *testing.T) {
		svc := &mockUsageTokenSvc{ingestErr: model.ErrUsageDeviceRevoked}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", validBatchJSON(validBucketJSON(1)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("got %d", w.Code)
		}
		if body := decodeUsageTokenErr(t, w); body.Error != "device_revoked" {
			t.Errorf("error = %q", body.Error)
		}
	})
	t.Run("internal error", func(t *testing.T) {
		svc := &mockUsageTokenSvc{ingestErr: errors.New("db connection refused")}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", validBatchJSON(validBucketJSON(1)))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d", w.Code)
		}
		body := decodeUsageTokenErr(t, w)
		if body.Error != "internal_error" || strings.Contains(body.Message, "connection") {
			t.Errorf("body = %+v; internal detail must not leak", body)
		}
	})
}

// TestUsageTokenBackfill_MaxDays: backfill 的 max_days 区间与 too_old 口径。
func TestUsageTokenBackfill_MaxDays(t *testing.T) {
	t.Parallel()
	t.Run("max_days out of range", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		body := `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","backfill":true,"max_days":181,"buckets":[` + validBucketJSON(1) + `]}`
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/backfill", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
		if body := decodeUsageTokenErr(t, w); body.Error != "invalid_request" {
			t.Errorf("error = %q", body.Error)
		}
	})
	t.Run("bucket beyond max_days is too_old", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		// 默认 max_days=30：40 天前的桶在 backfill 里 too_old（同样的桶走
		// batch 端点则合法，因为 batch 阈值是保留期 180 天）。
		old := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)
		bucket := `{"bucket_start":` + fmt.Sprintf("%q", old) +
			`,"bucket_size":"hour","source":"chat","agent_key":"chat","provider_key":"builtin","model_key":"default","prompt_tokens":1,"completion_tokens":0,"total_tokens":1,"requests":1}`
		body := `{"schema_version":1,"device_id":"d_7f3a9c2e5b104d8e","backfill":true,"buckets":[` + bucket + `]}`
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/backfill", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d (%s)", w.Code, w.Body.String())
		}
		if body := decodeUsageTokenErr(t, w); body.Error != "too_old" {
			t.Errorf("error = %q, want too_old", body.Error)
		}
	})
}

// TestUsageTokenQuery_ErrorMapping 覆盖 §8 查询类错误码的映射。
func TestUsageTokenQuery_ErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		path      string
		svcErr    error
		wantCode  int
		wantError string
	}{
		{"window too large", "/user/usage/tokens/summary?from=2026-01-01&to=2026-12-31",
			&model.UsageTokenError{Status: 400, Code: "window_too_large", Message: "window must not exceed 180 days"}, 400, "window_too_large"},
		{"invalid dimension", "/user/usage/tokens/breakdown?dimension=country",
			&model.UsageTokenError{Status: 400, Code: "invalid_dimension", Message: "..."}, 400, "invalid_dimension"},
		{"invalid interval", "/user/usage/tokens/timeseries?interval=year",
			&model.UsageTokenError{Status: 400, Code: "invalid_interval", Message: "..."}, 400, "invalid_interval"},
		{"tz not supported for day", "/user/usage/tokens/timeseries?tz=Asia/Shanghai",
			&model.UsageTokenError{Status: 400, Code: "tz_not_supported_for_day", Message: "..."}, 400, "tz_not_supported_for_day"},
		{"bad tz", "/user/usage/tokens/timeseries?tz=Mars/Olympus",
			&model.UsageTokenError{Status: 400, Code: "invalid_request", Message: "..."}, 400, "invalid_request"},
		{"delete device not found", "/user/usage/tokens?device_id=d_7f3a9c2e5b104d8e",
			model.ErrUsageDeviceNotFound, 404, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &mockUsageTokenSvc{
				summaryErr: tc.svcErr, breakdownErr: tc.svcErr, timeseriesErr: tc.svcErr,
				delErr: tc.svcErr,
			}
			e := usageTokenTestEngine(svc)
			method := http.MethodGet
			if strings.HasPrefix(tc.path, "/user/usage/tokens?") {
				method = http.MethodDelete
			}
			w := doUsageTokenReq(t, e, method, tc.path, "")
			if w.Code != tc.wantCode {
				t.Fatalf("got %d (%s), want %d", w.Code, w.Body.String(), tc.wantCode)
			}
			if body := decodeUsageTokenErr(t, w); body.Error != tc.wantError {
				t.Errorf("error = %q, want %q", body.Error, tc.wantError)
			}
		})
	}
}

// TestUsageTokenBreakdown_Params: dimension/limit 透传；limit 非法 →
// invalid_request（不调 service）。
func TestUsageTokenBreakdown_Params(t *testing.T) {
	t.Parallel()
	t.Run("limit forwarded", func(t *testing.T) {
		svc := &mockUsageTokenSvc{breakdown: []model.UsageBreakdownItem{}}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/breakdown?dimension=agent&from=2026-10-01&to=2026-10-22&limit=10", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		if svc.gotLimit != 10 || svc.gotBreakdownArgs[0] != "agent" {
			t.Errorf("args = %v limit %d", svc.gotBreakdownArgs, svc.gotLimit)
		}
		if !strings.Contains(w.Body.String(), `"truncated":false`) {
			t.Errorf("body missing truncated: %s", w.Body.String())
		}
	})
	t.Run("limit not a number", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/breakdown?dimension=agent&limit=abc", "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
		if body := decodeUsageTokenErr(t, w); body.Error != "invalid_request" {
			t.Errorf("error = %q", body.Error)
		}
	})
}

// TestUsageTokenTimeseries_Defaults: interval/group_by/tz 默认值按 §4.4 透传。
func TestUsageTokenTimeseries_Defaults(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{timeseries: &model.UsageTimeseriesResult{TZ: "UTC", Items: []model.UsageTimeseriesItem{}}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/timeseries", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	want := []string{"day", "none", "UTC"}
	for i, v := range want {
		if svc.gotTimeseriesArgs[i] != v {
			t.Errorf("arg %d = %q, want %q", i, svc.gotTimeseriesArgs[i], v)
		}
	}
	// 响应带 tz 字段（§4.4）。
	if !strings.Contains(w.Body.String(), `"tz":"UTC"`) {
		t.Errorf("body missing tz: %s", w.Body.String())
	}
}

// TestUsageTokenCoverage_Fields: coverage 必带 bucket_size/retention_days/
// server_time 三配置字段（config discovery，A1）。
func TestUsageTokenCoverage_Fields(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	svc := &mockUsageTokenSvc{coverage: &model.UsageCoverage{
		BucketSize: "hour", RetentionDays: 180, ExportMaxRows: 500000, ServerTime: now,
		Devices: []model.UsageDeviceView{{DeviceID: "d_7f3a9c2e5b104d8e", Revoked: false}},
	}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/coverage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"bucket_size":"hour"`, `"retention_days":180`, `"export_max_rows":500000`, `"server_time":"`, `"devices":[`} {
		if !strings.Contains(body, want) {
			t.Errorf("coverage body missing %s: %s", want, body)
		}
	}
}

// TestUsageTokenExportCSV: csv 直出文件流（无 envelope）、列序钉死、公式
// 注入字符前置 '。
func TestUsageTokenExportCSV(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	svc := &mockUsageTokenSvc{export: []model.UsageExportRow{{
		DeviceID: "d_7f3a9c2e5b104d8e", BucketStart: ts, Source: "gateway",
		AgentKey: "=cmd|'/c calc'!A1", ProviderKey: "+evil", ModelKey: "@x",
		PromptTokens: 27, CompletionTokens: 8, TotalTokens: 35, Requests: 2,
	}}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=csv&from=2026-10-20&to=2026-10-20", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `attachment; filename="kaya-usage-2026-10-20-2026-10-20.csv"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Errorf("Content-Type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("csv lines = %d, want header + 1 row: %q", len(lines), w.Body.String())
	}
	if lines[0] != "device_id,bucket_start,source,agent_key,provider_key,model_key,prompt_tokens,completion_tokens,total_tokens,requests" {
		t.Errorf("header = %q", lines[0])
	}
	// 公式注入防护：= + @ 开头的文本单元格前置 '。
	if !strings.Contains(lines[1], `"'=cmd|'/c calc'!A1"`) && !strings.Contains(lines[1], "'=cmd") {
		t.Errorf("agent_key cell not guarded: %q", lines[1])
	}
	if !strings.Contains(lines[1], "'+evil") || !strings.Contains(lines[1], "'@x") {
		t.Errorf("provider/model cells not guarded: %q", lines[1])
	}
	if !strings.Contains(lines[1], "2026-10-20T01:00:00Z") {
		t.Errorf("bucket_start not RFC3339 Z: %q", lines[1])
	}
	if strings.Contains(lines[0], `"code"`) {
		t.Error("csv must not be wrapped in envelope")
	}
}

// TestUsageTokenExportJSON: format=json 走 envelope，items 字段与 CSV 列同名。
func TestUsageTokenExportJSON(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	svc := &mockUsageTokenSvc{export: []model.UsageExportRow{{
		DeviceID: "d_x", BucketStart: ts, Source: "chat", AgentKey: "chat",
		ProviderKey: "builtin", ModelKey: "default",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Requests: 1,
	}}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=json&from=2026-10-20&to=2026-10-20", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != 0 || len(resp.Data.Items) != 1 {
		t.Fatalf("resp = %s", w.Body.String())
	}
	item := resp.Data.Items[0]
	for _, k := range []string{"device_id", "bucket_start", "source", "agent_key", "provider_key", "model_key", "prompt_tokens", "completion_tokens", "total_tokens", "requests"} {
		if _, ok := item[k]; !ok {
			t.Errorf("item missing key %q: %v", k, item)
		}
	}
}

// TestUsageTokenExport_BadFormat: format 非 csv|json → invalid_request。
func TestUsageTokenExport_BadFormat(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=xlsx", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d", w.Code)
	}
	if svc.export != nil || svc.exportErr != nil {
		t.Error("service must not be called")
	}
}

// TestUsageTokenDelete_ParamsAndShapes: 参数透传 + device_id 格式校验 +
// 成功形状（水位/墓碑）。
func TestUsageTokenDelete_ParamsAndShapes(t *testing.T) {
	t.Parallel()
	t.Run("bad device_id", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens?device_id=bad", "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("full delete shape", func(t *testing.T) {
		cleared := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
		svc := &mockUsageTokenSvc{del: &model.UsageDeleteResult{DeletedBuckets: 1234, ClearedBefore: &cleared}}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"deleted_buckets":1234`) || !strings.Contains(body, `"cleared_before":"2026-10-22T09:00:00Z"`) || !strings.Contains(body, `"tombstone":null`) {
			t.Errorf("body = %s", body)
		}
	})
	t.Run("window delete forwards args", func(t *testing.T) {
		svc := &mockUsageTokenSvc{del: &model.UsageDeleteResult{DeletedBuckets: 3,
			Tombstone: &model.UsageTombstoneView{FromBucket: time.Now(), ToBucket: time.Now()}}}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens?device_id=d_7f3a9c2e5b104d8e&from=2026-10-01&to=2026-10-02", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		want := []string{"d_7f3a9c2e5b104d8e", "2026-10-01", "2026-10-02"}
		for i, v := range want {
			if svc.gotDeleteArgs[i] != v {
				t.Errorf("arg %d = %q, want %q", i, svc.gotDeleteArgs[i], v)
			}
		}
	})
}

// TestUsageTokenDevices 覆盖设备管理端点的校验与错误映射。
func TestUsageTokenDevices(t *testing.T) {
	t.Parallel()
	t.Run("register ok", func(t *testing.T) {
		label := "工作机"
		platform := "macos"
		svc := &mockUsageTokenSvc{device: &model.UsageDeviceView{DeviceID: "d_7f3a9c2e5b104d8e", Label: &label, Platform: &platform}}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/devices",
			`{"device_id":"d_7f3a9c2e5b104d8e","label":"工作机","platform":"macos","app_version":"3.1.2","consent_version":"v1","consented_at":"2026-10-22T08:00:00Z"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d (%s)", w.Code, w.Body.String())
		}
		if svc.gotRegister.Label == nil || *svc.gotRegister.Label != "工作机" {
			t.Errorf("label = %+v", svc.gotRegister.Label)
		}
		if svc.gotRegister.ConsentedAt == nil {
			t.Error("consented_at not parsed")
		}
	})
	t.Run("register revoked -> 403", func(t *testing.T) {
		svc := &mockUsageTokenSvc{deviceErr: model.ErrUsageDeviceRevoked}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/devices", `{"device_id":"d_7f3a9c2e5b104d8e"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("got %d", w.Code)
		}
		if body := decodeUsageTokenErr(t, w); body.Error != "device_revoked" {
			t.Errorf("error = %q", body.Error)
		}
	})
	t.Run("register bad platform", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/devices", `{"device_id":"d_7f3a9c2e5b104d8e","platform":"dos"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("register long label", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPost, "/user/devices",
			`{"device_id":"d_7f3a9c2e5b104d8e","label":"`+strings.Repeat("x", 129)+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("list shape", func(t *testing.T) {
		svc := &mockUsageTokenSvc{devices: []model.UsageDeviceView{{DeviceID: "d_a", Revoked: true}}}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodGet, "/user/devices", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"revoked":true`) {
			t.Errorf("body = %s", w.Body.String())
		}
	})
	t.Run("patch requires label", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPatch, "/user/devices/d_7f3a9c2e5b104d8e", `{}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
	})
	t.Run("patch not found -> 404", func(t *testing.T) {
		svc := &mockUsageTokenSvc{renameErr: model.ErrUsageDeviceNotFound}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodPatch, "/user/devices/d_7f3a9c2e5b104d8e", `{"label":"x"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("got %d", w.Code)
		}
		if body := decodeUsageTokenErr(t, w); body.Error != "not_found" {
			t.Errorf("error = %q", body.Error)
		}
	})
	t.Run("delete purge flag forwarded", func(t *testing.T) {
		svc := &mockUsageTokenSvc{}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodDelete, "/user/devices/d_7f3a9c2e5b104d8e?purge=true", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		if !svc.gotPurge {
			t.Error("purge flag not forwarded")
		}
	})
	t.Run("delete not found -> 404", func(t *testing.T) {
		svc := &mockUsageTokenSvc{revokeErr: model.ErrUsageDeviceNotFound}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodDelete, "/user/devices/d_7f3a9c2e5b104d8e", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("got %d", w.Code)
		}
	})
}

// TestUsageTokenExport_Headers 覆盖 v1.2 §4.6：每个 200 导出响应恒定携带
// X-Result-Truncated / X-Result-Returned / X-Result-Limit 三个头；JSON 形态
// data 同步带 truncated/returned/limit。
func TestUsageTokenExport_Headers(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	row := model.UsageExportRow{
		DeviceID: "d_x", BucketStart: ts, Source: "chat", AgentKey: "chat",
		ProviderKey: "builtin", ModelKey: "default",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Requests: 1,
	}

	checkHeaders := func(t *testing.T, w *httptest.ResponseRecorder, truncated string, returned string, limit string) {
		t.Helper()
		if got := w.Header().Get("X-Result-Truncated"); got != truncated {
			t.Errorf("X-Result-Truncated = %q, want %q", got, truncated)
		}
		if got := w.Header().Get("X-Result-Returned"); got != returned {
			t.Errorf("X-Result-Returned = %q, want %q", got, returned)
		}
		if got := w.Header().Get("X-Result-Limit"); got != limit {
			t.Errorf("X-Result-Limit = %q, want %q", got, limit)
		}
	}

	t.Run("csv 200 carries three headers", func(t *testing.T) {
		svc := &mockUsageTokenSvc{export: []model.UsageExportRow{row}, exportMaxRows: 7}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=csv&from=2026-10-20&to=2026-10-20", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		checkHeaders(t, w, "false", "1", "7")
	})

	t.Run("csv truncated carries truncated=true", func(t *testing.T) {
		svc := &mockUsageTokenSvc{export: []model.UsageExportRow{row}, exportTotal: 5, exportTrunc: true, exportMaxRows: 1}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?allow_truncated=true", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		checkHeaders(t, w, "true", "1", "1")
		if !svc.gotAllowTruncated {
			t.Error("allow_truncated not forwarded to service")
		}
	})

	t.Run("json 200 carries fields and headers", func(t *testing.T) {
		svc := &mockUsageTokenSvc{export: []model.UsageExportRow{row}, exportTotal: 1, exportMaxRows: 7}
		e := usageTokenTestEngine(svc)
		w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=json", "")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		checkHeaders(t, w, "false", "1", "7")
		var resp struct {
			Data struct {
				Items     []map[string]any `json:"items"`
				Truncated bool             `json:"truncated"`
				Returned  int              `json:"returned"`
				Limit     int              `json:"limit"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Data.Truncated || resp.Data.Returned != 1 || resp.Data.Limit != 7 || len(resp.Data.Items) != 1 {
			t.Errorf("data = %s", w.Body.String())
		}
	})
}

// TestUsageTokenExport_TooLarge 覆盖 v1.2 §9.4.9 的 handler 映射：service
// 的 export_too_large → 400 JSON envelope（Content-Type 不是 text/csv，
// data 带 max_rows/from/to），响应体零 CSV 数据行。
func TestUsageTokenExport_TooLarge(t *testing.T) {
	t.Parallel()
	svc := &mockUsageTokenSvc{exportErr: &model.UsageTokenError{
		Status: 400, Code: "export_too_large",
		Message: "export exceeds the row limit",
		Data:    map[string]any{"max_rows": 2, "from": "2026-10-20", "to": "2026-10-22"},
	}}
	e := usageTokenTestEngine(svc)
	w := doUsageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?from=2026-10-20&to=2026-10-22", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/csv") {
		t.Errorf("Content-Type = %q, must not be text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("Content-Disposition must not be set on the error path, got %q", cd)
	}
	body := decodeUsageTokenErr(t, w)
	if body.Error != "export_too_large" {
		t.Errorf("error = %q", body.Error)
	}
	var data struct {
		MaxRows int    `json:"max_rows"`
		From    string `json:"from"`
		To      string `json:"to"`
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("data = %s", body.Data)
	}
	if data.MaxRows != 2 || data.From != "2026-10-20" || data.To != "2026-10-22" {
		t.Errorf("data = %+v", data)
	}
	// 零 CSV 字节：响应体不是任何 CSV 内容。
	if strings.Contains(w.Body.String(), "device_id,bucket_start") {
		t.Error("error response must not contain CSV bytes")
	}
}

// TestUsageTokenExport_ExposeHeaders 覆盖 v1.2 §9.4.11：带 Origin 的请求
// （200 与 400）都暴露三个 X-Result-* 头名；不带 Origin 不加该头。
func TestUsageTokenExport_ExposeHeaders(t *testing.T) {
	t.Parallel()
	row := model.UsageExportRow{DeviceID: "d_x", BucketStart: time.Now().UTC(), Source: "chat",
		AgentKey: "chat", ProviderKey: "builtin", ModelKey: "default", Requests: 1}

	doWithOrigin := func(t *testing.T, svc *mockUsageTokenSvc, origin string) *httptest.ResponseRecorder {
		t.Helper()
		e := usageTokenTestEngine(svc)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/user/usage/tokens/export", strings.NewReader(""))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		e.ServeHTTP(w, req)
		return w
	}

	t.Run("200 with Origin exposes headers", func(t *testing.T) {
		w := doWithOrigin(t, &mockUsageTokenSvc{export: []model.UsageExportRow{row}}, "https://console.example.com")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d", w.Code)
		}
		expose := w.Header().Get("Access-Control-Expose-Headers")
		for _, h := range []string{"X-Result-Truncated", "X-Result-Returned", "X-Result-Limit"} {
			if !strings.Contains(expose, h) {
				t.Errorf("Expose-Headers %q missing %s", expose, h)
			}
		}
	})
	t.Run("400 with Origin exposes headers", func(t *testing.T) {
		w := doWithOrigin(t, &mockUsageTokenSvc{exportErr: &model.UsageTokenError{
			Status: 400, Code: "export_too_large", Message: "...",
			Data: map[string]any{"max_rows": 2, "from": "2026-10-20", "to": "2026-10-22"},
		}}, "https://console.example.com")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d", w.Code)
		}
		if expose := w.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(expose, "X-Result-Truncated") {
			t.Errorf("Expose-Headers = %q on 400", expose)
		}
	})
	t.Run("no Origin -> no expose header", func(t *testing.T) {
		w := doWithOrigin(t, &mockUsageTokenSvc{export: []model.UsageExportRow{row}}, "")
		if got := w.Header().Get("Access-Control-Expose-Headers"); got != "" {
			t.Errorf("Expose-Headers = %q without Origin", got)
		}
	})
}
