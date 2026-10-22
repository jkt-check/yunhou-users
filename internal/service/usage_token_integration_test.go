package service_test

// kaya token 用量统计的 DB 集成测试（契约 yunhou-terminal
// docs/api/usage-stats.md v1.2 §9.4 验收夹具全覆盖 + 跨账号 IDOR）。
// 连真实 Postgres（DATABASE_URL，默认 postgres://postgres@localhost/
// yunhou_users），连不上则整体 skip —— 与 internal/repo、internal/service
// 既有 DB 测试同模式。
//
// 本文件是外部测试包（service_test），因此可以穿过 handler → service →
// repo 全链路驱动真实 HTTP 语义（gin engine + 身份 stub），这是全或无
// 零写入、水位/墓碑丢弃等"跨层契约"的落地测试位置。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"

	"github.com/yunhou/users/internal/handler"
	"github.com/yunhou/users/internal/middleware"
	"github.com/yunhou/users/internal/model"
	"github.com/yunhou/users/internal/repo"
	"github.com/yunhou/users/internal/service"
)

func usageTokenDBURL() string {
	if u := os.Getenv("DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://postgres@localhost/yunhou_users?sslmode=disable"
}

// usageTokenTestDB 连接测试库并清空本特性四张表 + users（FK 依赖）。
func usageTokenTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("postgres", usageTokenDBURL())
	if err != nil {
		t.Skipf("skip: no postgres available (%v)", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(context.Background(),
		`TRUNCATE usage_token_buckets, usage_devices, usage_token_watermarks, usage_token_tombstones, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("wipe usage token tables: %v", err)
	}
	return db
}

// usageTokenTestEngine 装配真实 handler+service+repo；身份 stub 从
// X-Test-User 头读 user_id，替代 JWTAuth（鉴权中间件本身不在本测试范围，
// 路由表已保证 JWTAuth 在 handler 之前）。
func usageTokenTestEngine(t *testing.T, db *sqlx.DB, bucketSize string, retentionDays, exportMaxRows int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := service.NewUsageTokenService(repo.NewUsageTokenRepo(db), bucketSize, retentionDays, exportMaxRows)
	h := handler.NewUsageTokenHandler(svc)
	e := gin.New()
	e.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserID, c.GetHeader("X-Test-User"))
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

// usageTokenUser 建一个测试用户（users 行是四张表的 FK 父）。
func usageTokenUser(t *testing.T, db *sqlx.DB) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, status) VALUES ($1, 'active')`, id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// usageTokenReq 发请求并带身份头。
func usageTokenReq(t *testing.T, e *gin.Engine, method, path, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", user)
	e.ServeHTTP(w, req)
	return w
}

// usageTokenBucketJSON 造一个合法桶（hour 粒度）。ts 为 UTC 整点。
func usageTokenBucketJSON(ts time.Time, source, agent, provider, modelKey string, prompt, completion, total, requests int) string {
	return fmt.Sprintf(`{"bucket_start":%q,"bucket_size":"hour","source":%q,"agent_key":%q,"provider_key":%q,"model_key":%q,"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"requests":%d}`,
		ts.UTC().Format(time.RFC3339), source, agent, provider, modelKey, prompt, completion, total, requests)
}

func usageTokenBatchBody(deviceID string, buckets ...string) string {
	return `{"schema_version":1,"device_id":` + fmt.Sprintf("%q", deviceID) + `,"buckets":[` + strings.Join(buckets, ",") + `]}`
}

// usageTokenFixtureDay 是夹具使用的"两天前"UTC 日期（避开 future/too_old
// 边界，且永远落在默认 90 天窗口内）。
func usageTokenFixtureDay() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day()-2, 0, 0, 0, 0, time.UTC)
}

// fixtureBuckets 复刻契约 §9.2 的两桶形状（动态日期，数字不变）：
// (chat/chat/builtin/default 10/5/15/1) 与 (gateway/claude-code/
// preset:deepseek/deepseek-v4-pro 27/8/35/2)。
func fixtureBuckets(day time.Time, deviceID string) string {
	b1 := usageTokenBucketJSON(day.Add(1*time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)
	b2 := usageTokenBucketJSON(day.Add(2*time.Hour), "gateway", "claude-code", "preset:deepseek", "deepseek-v4-pro", 27, 8, 35, 2)
	return usageTokenBatchBody(deviceID, b1, b2)
}

func decodeOK(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("code = %d (%s)", resp.Code, w.Body.String())
	}
	return resp.Data
}

func summaryTotal(t *testing.T, e *gin.Engine, user string) float64 {
	t.Helper()
	w := usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/summary", user, "")
	data := decodeOK(t, w)
	return data["total_tokens"].(float64)
}

// §9.3 期望查询结果 + §9.4.1 重复上传不翻倍。
func TestUsageTokenIntegration_FixtureSummaryAndRepeatUpload(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	body := fixtureBuckets(day, "d_7f3a9c2e5b104d8e")

	// 上传两次：第二次必须是幂等覆盖，不翻倍（§9.4.1）。
	for i := 0; i < 2; i++ {
		w := usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, body)
		data := decodeOK(t, w)
		if data["accepted"].(float64) != 2 || data["updated"].(float64) != 2 {
			t.Fatalf("upload %d: %s", i, w.Body.String())
		}
	}

	// §9.3 summary 期望值。
	w := usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/summary", user, "")
	sum := decodeOK(t, w)
	want := map[string]float64{
		"total_tokens": 50, "prompt_tokens": 37, "completion_tokens": 13,
		"requests": 3, "device_count": 1, "active_days": 1,
	}
	for k, v := range want {
		if sum[k].(float64) != v {
			t.Errorf("summary.%s = %v, want %v (%s)", k, sum[k], v, w.Body.String())
		}
	}
	if sum["first_bucket"] != day.Add(1*time.Hour).Format(time.RFC3339) {
		t.Errorf("first_bucket = %v", sum["first_bucket"])
	}
	if sum["last_bucket"] != day.Add(2*time.Hour).Format(time.RFC3339) {
		t.Errorf("last_bucket = %v", sum["last_bucket"])
	}

	// §9.3 breakdown?dimension=agent：total DESC、key ASC。
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/breakdown?dimension=agent", user, "")
	bd := decodeOK(t, w)
	items := bd["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("breakdown items = %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["agent_key"] != "claude-code" || first["total_tokens"].(float64) != 35 || first["requests"].(float64) != 2 {
		t.Errorf("agent[0] = %v", first)
	}

	// §9.3 breakdown?dimension=provider_model：display_name 目录解析
	//（preset:deepseek → DeepSeek，builtin → 内置）。
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/breakdown?dimension=provider_model", user, "")
	items = decodeOK(t, w)["items"].([]any)
	got := map[string]string{}
	for _, it := range items {
		m := it.(map[string]any)
		got[m["provider_key"].(string)] = m["display_name"].(string)
	}
	if got["preset:deepseek"] != "DeepSeek" || got["builtin"] != "内置" {
		t.Errorf("display names = %v", got)
	}
}

// §9.4.2 + §9.4.3：覆盖不累加；累计值后传生效。
func TestUsageTokenIntegration_OverwriteNotAccumulate(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	ts := usageTokenFixtureDay().Add(5 * time.Hour)

	upload := func(total, requests int) {
		body := usageTokenBatchBody("d_dev00001",
			usageTokenBucketJSON(ts, "chat", "chat", "builtin", "default", total, 0, total, requests))
		w := usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, body)
		decodeOK(t, w)
	}

	upload(10, 1)
	if got := summaryTotal(t, e, user); got != 10 {
		t.Fatalf("after first upload total = %v, want 10", got)
	}
	// 同一桶第二次传完整累计值：覆盖为 40，不是 10+40（§9.4.2）。
	upload(40, 3)
	if got := summaryTotal(t, e, user); got != 40 {
		t.Fatalf("after overwrite total = %v, want 40", got)
	}
	// §9.4.3：requests 也以最后一次累计值为准。
	w := usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/summary", user, "")
	if got := decodeOK(t, w)["requests"].(float64); got != 3 {
		t.Errorf("requests = %v, want 3", got)
	}
}

// §9.4.4：全或无 —— 3 桶中第 2 个 requests=0 → 整批 400 invalid_bucket
// （data.index=1），数据库零写入。
func TestUsageTokenIntegration_AllOrNothingZeroWrite(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()

	bad := usageTokenBucketJSON(day.Add(2*time.Hour), "chat", "chat", "builtin", "default", 1, 0, 1, 0)
	body := usageTokenBatchBody("d_dev00001",
		usageTokenBucketJSON(day.Add(1*time.Hour), "chat", "chat", "builtin", "default", 1, 0, 1, 1),
		bad,
		usageTokenBucketJSON(day.Add(3*time.Hour), "chat", "chat", "builtin", "default", 1, 0, 1, 1))
	w := usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
		Data  struct {
			Index int `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "invalid_bucket" || resp.Data.Index != 1 {
		t.Errorf("error=%q index=%d, want invalid_bucket/1", resp.Error, resp.Data.Index)
	}
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM usage_token_buckets WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0 (all-or-nothing)", n)
	}
}

// §9.4.5：全量删除 → 水位推进 → 重传同桶被 discarded，查询总量为 0。
func TestUsageTokenIntegration_DeleteAllThenReuploadDiscarded(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	body := fixtureBuckets(usageTokenFixtureDay(), "d_dev00001")

	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, body)
	if got := summaryTotal(t, e, user); got != 50 {
		t.Fatalf("precondition total = %v", got)
	}

	w := usageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens", user, "")
	data := decodeOK(t, w)
	if data["deleted_buckets"].(float64) != 2 {
		t.Errorf("deleted_buckets = %v", data["deleted_buckets"])
	}
	if data["cleared_before"] == nil || data["tombstone"] != nil {
		t.Errorf("cleared_before/tombstone = %v/%v", data["cleared_before"], data["tombstone"])
	}
	// cleared_before 必须 RFC3339 Z 且整点对齐。
	cleared := data["cleared_before"].(string)
	ct, err := time.Parse(time.RFC3339, cleared)
	if err != nil || !strings.HasSuffix(cleared, "Z") {
		t.Errorf("cleared_before = %q", cleared)
	}
	if ct.Minute() != 0 || ct.Second() != 0 {
		t.Errorf("cleared_before not hour-aligned: %v", ct)
	}

	// 重传同一批：全部被水位丢弃（discarded，不是错误），总量仍为 0。
	w = usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, body)
	data = decodeOK(t, w)
	if data["accepted"].(float64) != 0 {
		t.Errorf("accepted = %v, want 0", data["accepted"])
	}
	if len(data["discarded"].([]any)) != 2 {
		t.Errorf("discarded = %v", data["discarded"])
	}
	if got := summaryTotal(t, e, user); got != 0 {
		t.Errorf("total after re-upload = %v, want 0", got)
	}
}

// §9.4.6：窗口墓碑 —— 窗口内重传丢弃、窗口外正常。
func TestUsageTokenIntegration_WindowTombstone(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	next := day.AddDate(0, 0, 1)

	bIn := usageTokenBucketJSON(day.Add(5*time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)
	bOut := usageTokenBucketJSON(next.Add(5*time.Hour), "chat", "chat", "builtin", "default", 20, 10, 30, 2)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", bIn, bOut))

	// 删除 day 当天窗口；tombstone 端点 hour 部署 = [00:00Z, 23:00Z]。
	w := usageTokenReq(t, e, http.MethodDelete,
		fmt.Sprintf("/user/usage/tokens?from=%s&to=%s", day.Format("2006-01-02"), day.Format("2006-01-02")), user, "")
	data := decodeOK(t, w)
	if data["cleared_before"] != nil {
		t.Errorf("window delete must not set cleared_before: %s", w.Body.String())
	}
	tomb := data["tombstone"].(map[string]any)
	if tomb["from_bucket"] != day.Format(time.RFC3339) || tomb["to_bucket"] != day.Add(23*time.Hour).Format(time.RFC3339) {
		t.Errorf("tombstone = %v", tomb)
	}
	if data["deleted_buckets"].(float64) != 1 {
		t.Errorf("deleted_buckets = %v", data["deleted_buckets"])
	}

	// 重传窗口内桶 → discarded；窗口外桶 → accepted。
	w = usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", bIn, bOut))
	data = decodeOK(t, w)
	if data["accepted"].(float64) != 1 || len(data["discarded"].([]any)) != 1 {
		t.Errorf("accepted/discarded = %v/%v", data["accepted"], data["discarded"])
	}
	if got := summaryTotal(t, e, user); got != 30 {
		t.Errorf("total = %v, want 30 (窗口内被墓碑挡住)", got)
	}
}

// 设备级删除：水位只挡该设备，其他设备不受影响。
func TestUsageTokenIntegration_DeviceLevelDelete(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	b1 := usageTokenBucketJSON(day.Add(1*time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", b1))
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00002", b1))

	// 只删 dev1。
	w := usageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens?device_id=d_dev00001", user, "")
	data := decodeOK(t, w)
	if data["deleted_buckets"].(float64) != 1 {
		t.Errorf("deleted_buckets = %v", data["deleted_buckets"])
	}
	// 不存在的设备 → 404 not_found。
	w = usageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens?device_id=d_miss00001", user, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("missing device: got %d (%s)", w.Code, w.Body.String())
	}
	// dev1 旧桶重传被丢弃；dev2 的桶完好。
	w = usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", b1))
	data = decodeOK(t, w)
	if data["accepted"].(float64) != 0 {
		t.Errorf("accepted = %v, want 0 (device watermark)", data["accepted"])
	}
	if got := summaryTotal(t, e, user); got != 15 {
		t.Errorf("total = %v, want 15 (dev2 intact)", got)
	}
}

// §9.4.8：撤销设备后其桶从 summary/breakdown/timeseries/export 消失；
// coverage 仍列出 revoked:true；撤销设备上传 → 403 device_revoked。
func TestUsageTokenIntegration_RevokedDeviceExcludedFromStats(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	b1 := usageTokenBucketJSON(day.Add(1*time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)
	b2 := usageTokenBucketJSON(day.Add(2*time.Hour), "gateway", "claude-code", "preset:deepseek", "deepseek-v4-pro", 27, 8, 35, 2)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", b1))
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00002", b2))

	// 撤销 dev2。
	w := usageTokenReq(t, e, http.MethodDelete, "/user/devices/d_dev00002", user, "")
	decodeOK(t, w)

	// summary / breakdown / timeseries / export 全部排除 dev2。
	if got := summaryTotal(t, e, user); got != 15 {
		t.Errorf("summary total = %v, want 15", got)
	}
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/breakdown?dimension=device", user, "")
	items := decodeOK(t, w)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["device_id"] != "d_dev00001" {
		t.Errorf("breakdown device = %v", items)
	}
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/timeseries?interval=day&group_by=device", user, "")
	items = decodeOK(t, w)["items"].([]any)
	for _, it := range items {
		if it.(map[string]any)["device_id"] == "d_dev00002" {
			t.Errorf("timeseries includes revoked device: %v", it)
		}
	}
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=json", user, "")
	items = decodeOK(t, w)["items"].([]any)
	if len(items) != 1 {
		t.Errorf("export items = %d, want 1 (revoked excluded)", len(items))
	}

	// coverage 仍列出 dev2 且 revoked:true；顶层 latest 只算 dev1。
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/coverage", user, "")
	cov := decodeOK(t, w)
	devices := cov["devices"].([]any)
	if len(devices) != 2 {
		t.Fatalf("coverage devices = %d", len(devices))
	}
	var revokedSeen bool
	for _, d := range devices {
		m := d.(map[string]any)
		if m["device_id"] == "d_dev00002" {
			revokedSeen = m["revoked"].(bool)
		}
	}
	if !revokedSeen {
		t.Errorf("coverage must list d_dev00002 with revoked:true: %v", devices)
	}
	if cov["latest_bucket"] != day.Add(1*time.Hour).Format(time.RFC3339) {
		t.Errorf("coverage latest_bucket = %v, want dev1's bucket only", cov["latest_bucket"])
	}
	if cov["bucket_size"] != "hour" || cov["retention_days"].(float64) != 180 || cov["server_time"] == nil ||
		cov["export_max_rows"].(float64) != 500000 {
		t.Errorf("coverage config fields wrong: %v", cov)
	}

	// 撤销后摄入 → 403 device_revoked（写桶之前整批拒绝）。
	w = usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00002", b2))
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked device ingest: got %d (%s)", w.Code, w.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil || errResp.Error != "device_revoked" {
		t.Errorf("error body = %s", w.Body.String())
	}

	// purge=true 撤销 dev1 同时删其历史桶。
	w = usageTokenReq(t, e, http.MethodDelete, "/user/devices/d_dev00001?purge=true", user, "")
	decodeOK(t, w)
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM usage_token_buckets WHERE user_id = $1 AND device_id = 'd_dev00001'`, user); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("purge left %d buckets", n)
	}
}

// 跨账号 IDOR（migration 047 头注的 RLS 补偿测试）：A 的桶/设备对 B
// 不可见、不可操作；B 用同名 device_id 也是 B 自己的行。
func TestUsageTokenIntegration_CrossUserIDOR(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	userA := usageTokenUser(t, db)
	userB := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	b1 := usageTokenBucketJSON(day.Add(1*time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", userA, usageTokenBatchBody("d_dev00001", b1))
	usageTokenReq(t, e, http.MethodPost, "/user/devices", userA,
		`{"device_id":"d_dev00001","label":"A 的工作机","platform":"macos"}`)

	// B 的所有读都为空。
	if got := summaryTotal(t, e, userB); got != 0 {
		t.Errorf("B summary total = %v, want 0", got)
	}
	w := usageTokenReq(t, e, http.MethodGet, "/user/devices", userB, "")
	if devices := decodeOK(t, w)["devices"].([]any); len(devices) != 0 {
		t.Errorf("B devices = %v, want empty", devices)
	}
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export?format=json", userB, "")
	if items := decodeOK(t, w)["items"].([]any); len(items) != 0 {
		t.Errorf("B export = %v", items)
	}

	// B 不能改/删 A 的设备（404，不泄露存在性）。
	w = usageTokenReq(t, e, http.MethodPatch, "/user/devices/d_dev00001", userB, `{"label":"hacked"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("B patch A device: got %d", w.Code)
	}
	w = usageTokenReq(t, e, http.MethodDelete, "/user/devices/d_dev00001", userB, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("B delete A device: got %d", w.Code)
	}
	// B 按设备删除 A 的桶 → 404（A 的数据不动）。
	w = usageTokenReq(t, e, http.MethodDelete, "/user/usage/tokens?device_id=d_dev00001", userB, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("B delete A buckets: got %d", w.Code)
	}
	if got := summaryTotal(t, e, userA); got != 15 {
		t.Errorf("A total after B's attempts = %v, want 15", got)
	}
	// A 的 label 未被 B 改掉。
	w = usageTokenReq(t, e, http.MethodGet, "/user/devices", userA, "")
	devices := decodeOK(t, w)["devices"].([]any)
	if devices[0].(map[string]any)["label"] != "A 的工作机" {
		t.Errorf("A device label = %v", devices[0])
	}
}

// 设备注册/改名/删除的 HTTP 语义：POST upsert 不覆盖已有 label（空值）；
// PATCH 只改 label；摄入 auto-upsert 的设备行 label 为 NULL。
func TestUsageTokenIntegration_DeviceLifecycle(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)

	// 摄入 auto-upsert：label NULL、last_seen_at 有值。
	b1 := usageTokenBucketJSON(usageTokenFixtureDay().Add(1*time.Hour), "chat", "chat", "builtin", "default", 1, 0, 1, 1)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", b1))
	w := usageTokenReq(t, e, http.MethodGet, "/user/devices", user, "")
	dev := decodeOK(t, w)["devices"].([]any)[0].(map[string]any)
	if dev["label"] != nil || dev["last_seen_at"] == nil || dev["revoked"].(bool) {
		t.Errorf("auto-upserted device = %v", dev)
	}

	// POST 补登记 label/platform。
	w = usageTokenReq(t, e, http.MethodPost, "/user/devices", user,
		`{"device_id":"d_dev00001","label":"工作机","platform":"macos","app_version":"3.1.2","consent_version":"v1","consented_at":"2026-10-20T01:00:00Z"}`)
	decodeOK(t, w)
	// 再 POST 不带 label：不得清掉已有 label。
	w = usageTokenReq(t, e, http.MethodPost, "/user/devices", user, `{"device_id":"d_dev00001","app_version":"3.1.3"}`)
	data := decodeOK(t, w)
	if data["label"] != "工作机" || data["app_version"] != "3.1.3" {
		t.Errorf("after re-register: %v", data)
	}
	if data["consent_version"] != "v1" {
		t.Errorf("consent_version = %v", data["consent_version"])
	}

	// PATCH 改名。
	w = usageTokenReq(t, e, http.MethodPatch, "/user/devices/d_dev00001", user, `{"label":"新名字"}`)
	decodeOK(t, w)
	w = usageTokenReq(t, e, http.MethodGet, "/user/devices", user, "")
	dev = decodeOK(t, w)["devices"].([]any)[0].(map[string]any)
	if dev["label"] != "新名字" {
		t.Errorf("label = %v", dev["label"])
	}
	// PATCH 不存在的设备 → 404。
	w = usageTokenReq(t, e, http.MethodPatch, "/user/devices/d_miss00001", user, `{"label":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("patch missing: got %d", w.Code)
	}
}

// timeseries 的 tz 切桶（hour 部署 + 非 UTC）：UTC 17:00 的桶在
// Asia/Shanghai 属次日 01:00，日标签必须是次日（§4.4）。
func TestUsageTokenIntegration_TimeseriesTZ(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	b := usageTokenBucketJSON(day.Add(17*time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", b))

	localDay := day.AddDate(0, 0, 1) // Asia/Shanghai 下该桶属次日
	w := usageTokenReq(t, e, http.MethodGet,
		fmt.Sprintf("/user/usage/tokens/timeseries?interval=day&tz=Asia/Shanghai&from=%s&to=%s",
			localDay.Format("2006-01-02"), localDay.Format("2006-01-02")), user, "")
	data := decodeOK(t, w)
	if data["tz"] != "Asia/Shanghai" {
		t.Errorf("tz = %v", data["tz"])
	}
	items := data["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["bucket"] != localDay.Format("2006-01-02") {
		t.Errorf("items = %v, want one bucket labeled %s", items, localDay.Format("2006-01-02"))
	}
}

// 保留期 sweeper：过期桶与过期墓碑被删，窗口内的保留。
func TestUsageTokenIntegration_RetentionSweep(t *testing.T) {
	db := usageTokenTestDB(t)
	user := usageTokenUser(t, db)
	now := time.Now().UTC().Truncate(time.Hour)
	old := now.Add(-200 * 24 * time.Hour)
	recent := now.Add(-24 * time.Hour)

	// 直接 SQL 种数据（摄入校验会拒 200 天前的桶，这里测的是 sweeper）。
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO usage_devices (user_id, device_id) VALUES ($1, 'd_dev00001')`, user); err != nil {
		t.Fatal(err)
	}
	for _, ts := range []time.Time{old, recent} {
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO usage_token_buckets (user_id, device_id, bucket_start, bucket_size, source, agent_key, provider_key, model_key, prompt_tokens, completion_tokens, total_tokens, requests)
			VALUES ($1, 'd_dev00001', $2, 'hour', 'chat', 'chat', 'builtin', 'default', 1, 0, 1, 1)`, user, ts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO usage_token_tombstones (user_id, device_id, from_bucket, to_bucket) VALUES
		($1, NULL, $2, $2), ($1, NULL, $3, $3)`, user, old, recent); err != nil {
		t.Fatal(err)
	}

	sweeper := service.NewUsageTokenRetentionSweeper(repo.NewUsageTokenRepo(db), 180, 0)
	buckets, tombstones, err := sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if buckets != 1 || tombstones != 1 {
		t.Errorf("swept buckets/tombstones = %d/%d, want 1/1", buckets, tombstones)
	}
	var n int
	if err := db.GetContext(context.Background(), &n,
		`SELECT COUNT(*) FROM usage_token_buckets WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("remaining buckets = %d, want 1 (recent kept)", n)
	}
}

// §9.4.9/10/11：导出超限 fail-closed、显式截断三头、跨域 Expose-Headers。
// service 用小 cap（2）实例，真实 PG。
func TestUsageTokenIntegration_ExportLimits(t *testing.T) {
	db := usageTokenTestDB(t)
	const cap = 2
	e := usageTokenTestEngine(t, db, "hour", 180, cap)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()

	// 种 cap+1 = 3 个桶。
	buckets := make([]string, 0, cap+1)
	for i := 0; i < cap+1; i++ {
		buckets = append(buckets, usageTokenBucketJSON(day.Add(time.Duration(i)*time.Hour),
			"chat", "chat", "builtin", "default", 1, 0, 1, 1))
	}
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user, usageTokenBatchBody("d_dev00001", buckets...))

	exportReq := func(query string, origin string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/user/usage/tokens/export"+query, nil)
		req.Header.Set("X-Test-User", user)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		e.ServeHTTP(w, req)
		return w
	}

	// §9.4.9：超限且未带 allow_truncated → 400 export_too_large，Content-Type
	// 非 text/csv，零 CSV 数据行，data.max_rows == 配置 cap。
	w := exportReq(fmt.Sprintf("?format=csv&from=%s&to=%s", day.Format("2006-01-02"), day.Format("2006-01-02")), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over-cap export: got %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/csv") {
		t.Errorf("Content-Type = %q, must not be text/csv", ct)
	}
	if strings.Contains(w.Body.String(), "device_id,bucket_start") {
		t.Error("error body must contain zero CSV bytes")
	}
	var errResp struct {
		Error string `json:"error"`
		Data  struct {
			MaxRows int    `json:"max_rows"`
			From    string `json:"from"`
			To      string `json:"to"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if errResp.Error != "export_too_large" || errResp.Data.MaxRows != cap {
		t.Errorf("error=%q max_rows=%d, want export_too_large/%d", errResp.Error, errResp.Data.MaxRows, cap)
	}
	if errResp.Data.From != day.Format("2006-01-02") || errResp.Data.To != day.Format("2006-01-02") {
		t.Errorf("data from/to = %q/%q, want effective window %s", errResp.Data.From, errResp.Data.To, day.Format("2006-01-02"))
	}

	// §9.4.10：allow_truncated=true → 200 + 三头 + CSV 数据行数 ==
	// X-Result-Returned == cap。
	w = exportReq("?format=csv&allow_truncated=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("truncated export: got %d (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Result-Truncated"); got != "true" {
		t.Errorf("X-Result-Truncated = %q", got)
	}
	if got := w.Header().Get("X-Result-Returned"); got != "2" {
		t.Errorf("X-Result-Returned = %q, want %d", got, cap)
	}
	if got := w.Header().Get("X-Result-Limit"); got != "2" {
		t.Errorf("X-Result-Limit = %q, want %d", got, cap)
	}
	dataLines := len(strings.Split(strings.TrimSpace(w.Body.String()), "\n")) - 1 // 减表头
	if dataLines != cap {
		t.Errorf("csv data lines = %d, want %d", dataLines, cap)
	}
	// JSON 形态同步给字段。
	w = exportReq("?format=json&allow_truncated=true", "")
	var okResp struct {
		Data struct {
			Items     []any `json:"items"`
			Truncated bool  `json:"truncated"`
			Returned  int   `json:"returned"`
			Limit     int   `json:"limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &okResp); err != nil {
		t.Fatal(err)
	}
	if !okResp.Data.Truncated || okResp.Data.Returned != cap || okResp.Data.Limit != cap || len(okResp.Data.Items) != cap {
		t.Errorf("json export data = %s", w.Body.String())
	}

	// 未超限的 200 也恒定带三头（truncated=false）。第二个用户只有 1 桶。
	user2 := usageTokenUser(t, db)
	usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user2,
		usageTokenBatchBody("d_dev00001", usageTokenBucketJSON(day.Add(time.Hour), "chat", "chat", "builtin", "default", 1, 0, 1, 1)))
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/user/usage/tokens/export", nil)
	req.Header.Set("X-Test-User", user2)
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("under-cap export: got %d", w.Code)
	}
	if got := w.Header().Get("X-Result-Truncated"); got != "false" {
		t.Errorf("X-Result-Truncated = %q, want false", got)
	}
	if w.Header().Get("X-Result-Returned") != "1" || w.Header().Get("X-Result-Limit") != "2" {
		t.Errorf("returned/limit = %q/%q", w.Header().Get("X-Result-Returned"), w.Header().Get("X-Result-Limit"))
	}

	// §9.4.11：带 Origin 的请求（200 与 400）都暴露三个头名。
	w = exportReq("?format=csv&allow_truncated=true", "https://console.example.com")
	if expose := w.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(expose, "X-Result-Truncated") ||
		!strings.Contains(expose, "X-Result-Returned") || !strings.Contains(expose, "X-Result-Limit") {
		t.Errorf("200 Expose-Headers = %q", expose)
	}
	w = exportReq("?format=csv", "https://console.example.com")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d", w.Code)
	}
	if expose := w.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(expose, "X-Result-Truncated") {
		t.Errorf("400 Expose-Headers = %q", expose)
	}
}

// TestUsageTokenIntegration_ConcurrentIngestVsDelete 是 PR #61 review M4 的
// 并发回归：摄入（读水位/墓碑 → 写桶）与删除（写水位/墓碑 → 删桶）并发
// 时，落在被删窗口内的桶绝不复活。repo 两侧对同一 user_id 取
// pg_advisory_xact_lock 后，每轮的终态只能是两种串行序之一——摄入先
// （删除随后删掉）或删除先（摄入命中水位/墓碑被 discarded）——桶行数恒
// 为 0。无锁实现下存在第三种交错（摄入读到无墓碑 → 删除提交 → 摄入写
// 入），本测试正是那条交错的回归网。
func TestUsageTokenIntegration_ConcurrentIngestVsDelete(t *testing.T) {
	db := usageTokenTestDB(t)
	r := repo.NewUsageTokenRepo(db)
	user := usageTokenUser(t, db)
	ctx := context.Background()
	day := usageTokenFixtureDay()
	bucketStart := day.Add(time.Hour)

	newBucket := func() model.UsageTokenBucket {
		return model.UsageTokenBucket{
			BucketStart: bucketStart, BucketSize: "hour", Source: "chat",
			AgentKey: "chat", ProviderKey: "builtin", ModelKey: "default",
			PromptTokens: 1, CompletionTokens: 0, TotalTokens: 1, Requests: 1,
		}
	}

	const rounds = 30
	for i := 0; i < rounds; i++ {
		// 每轮换一个新 bucket_start：上一轮的墓碑已覆盖旧桶，不换则摄入
		// 恒被墓碑丢弃，测不到竞态。
		bs := day.Add(time.Duration(i+1) * time.Hour)
		var wg sync.WaitGroup
		var delErr, ingErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, delErr = r.DeleteWindow(ctx, user, nil, bs, bs)
		}()
		go func() {
			defer wg.Done()
			b := newBucket()
			b.BucketStart = bs
			_, ingErr = r.IngestBuckets(ctx, user, model.UsageTokenIngestInput{
				DeviceID: "d_concurrencym4", Buckets: []model.UsageTokenBucket{b},
			})
		}()
		wg.Wait()
		if delErr != nil {
			t.Fatalf("round %d: DeleteWindow: %v", i, delErr)
		}
		if ingErr != nil {
			t.Fatalf("round %d: IngestBuckets: %v", i, ingErr)
		}
		var n int
		if err := db.GetContext(ctx, &n,
			`SELECT COUNT(*) FROM usage_token_buckets WHERE user_id = $1 AND bucket_start = $2`,
			user, bs); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("round %d: deleted bucket resurrected (%d rows)", i, n)
		}
	}

	// DeleteAll（用户级水位）与摄入并发：同一断言。bucket_start 必须避开
	// 循环一的墓碑覆盖范围（day+1h..day+30h），否则摄入恒被墓碑丢弃，
	// 水位路径测不到。
	bucketStart2 := day.Add(48 * time.Hour)
	clearedBefore := service.CeilToBucket(time.Now().UTC(), "hour")
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		var delErr, ingErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, delErr = r.DeleteAll(ctx, user, clearedBefore)
		}()
		go func() {
			defer wg.Done()
			b := newBucket()
			b.BucketStart = bucketStart2
			_, ingErr = r.IngestBuckets(ctx, user, model.UsageTokenIngestInput{
				DeviceID: "d_concurrencym4", Buckets: []model.UsageTokenBucket{b},
			})
		}()
		wg.Wait()
		if delErr != nil {
			t.Fatalf("round %d: DeleteAll: %v", i, delErr)
		}
		if ingErr != nil {
			t.Fatalf("round %d: IngestBuckets: %v", i, ingErr)
		}
		var n int
		if err := db.GetContext(ctx, &n,
			`SELECT COUNT(*) FROM usage_token_buckets WHERE user_id = $1 AND bucket_start = $2`,
			user, bucketStart2); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("round %d: bucket resurrected after DeleteAll (%d rows)", i, n)
		}
	}

	// RevokeDevice(purge=true) 与摄入并发：purge 是唯一"在摄入 revoked
	// 检查之后才删桶"的路径。每轮换新设备（撤销是终态）；设备行先登记，
	// 否则 revoke 先跑会 ErrUsageDeviceNotFound。终态：桶行数恒 0——
	// 摄入先（purge 随后删掉）或撤销先（摄入被 ErrUsageDeviceRevoked 拒）。
	for i := 0; i < rounds; i++ {
		dev := fmt.Sprintf("d_purgerace%02d", i)
		bs := day.Add(time.Duration(72+i) * time.Hour) // 避开上面的墓碑/水位
		if _, err := r.UpsertDevice(ctx, user, model.UsageDeviceRegisterInput{DeviceID: dev}); err != nil {
			t.Fatalf("round %d: register device: %v", i, err)
		}
		var wg sync.WaitGroup
		var revErr, ingErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			revErr = r.RevokeDevice(ctx, user, dev, true)
		}()
		go func() {
			defer wg.Done()
			b := newBucket()
			b.BucketStart = bs
			_, ingErr = r.IngestBuckets(ctx, user, model.UsageTokenIngestInput{
				DeviceID: dev, Buckets: []model.UsageTokenBucket{b},
			})
		}()
		wg.Wait()
		if revErr != nil {
			t.Fatalf("round %d: RevokeDevice: %v", i, revErr)
		}
		if ingErr != nil && !errors.Is(ingErr, model.ErrUsageDeviceRevoked) {
			t.Fatalf("round %d: IngestBuckets: %v", i, ingErr)
		}
		var n int
		if err := db.GetContext(ctx, &n,
			`SELECT COUNT(*) FROM usage_token_buckets WHERE user_id = $1 AND bucket_start = $2`,
			user, bs); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("round %d: bucket resurrected after revoke+purge (%d rows)", i, n)
		}
	}
}

// TestUsageTokenIntegration_TombstoneMerge 是 PR #61 review M6 的回归：
// 重叠窗口删除的墓碑合并为一条（不无界增长），且合并后的墓碑仍拦截
// 重传；不相交窗口各自保留。
func TestUsageTokenIntegration_TombstoneMerge(t *testing.T) {
	db := usageTokenTestDB(t)
	r := repo.NewUsageTokenRepo(db)
	user := usageTokenUser(t, db)
	ctx := context.Background()
	day := usageTokenFixtureDay()
	h := func(n int) time.Time { return day.Add(time.Duration(n) * time.Hour) }

	// 相交窗口 [h1,h3] + [h2,h4] → 合并为一条 [h1,h4]。
	if _, err := r.DeleteWindow(ctx, user, nil, h(1), h(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DeleteWindow(ctx, user, nil, h(2), h(4)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM usage_token_tombstones WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("tombstones = %d, want 1 merged row", n)
	}
	var fromB, toB time.Time
	if err := db.QueryRowxContext(ctx,
		`SELECT from_bucket, to_bucket FROM usage_token_tombstones WHERE user_id = $1`, user).
		Scan(&fromB, &toB); err != nil {
		t.Fatal(err)
	}
	if !fromB.Equal(h(1)) || !toB.Equal(h(4)) {
		t.Errorf("merged = [%s, %s], want [%s, %s]", fromB, toB, h(1), h(4))
	}

	// 不相交窗口不合并且各留一行。
	if _, err := r.DeleteWindow(ctx, user, nil, h(10), h(11)); err != nil {
		t.Fatal(err)
	}
	if err := db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM usage_token_tombstones WHERE user_id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("tombstones = %d, want 2 (disjoint window not merged)", n)
	}

	// 合并后的墓碑仍拦截窗口内重传（摄入侧按批范围过滤不能漏判）。
	res, err := r.IngestBuckets(ctx, user, model.UsageTokenIngestInput{
		DeviceID: "d_tombstonemerge",
		Buckets: []model.UsageTokenBucket{{
			BucketStart: h(2), BucketSize: "hour", Source: "chat",
			AgentKey: "chat", ProviderKey: "builtin", ModelKey: "default",
			PromptTokens: 1, TotalTokens: 1, Requests: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 0 || len(res.Discarded) != 1 {
		t.Errorf("accepted=%d discarded=%d, want 0/1", res.Accepted, len(res.Discarded))
	}

	// 设备维度隔离：设备级墓碑只拦该设备；与全设备（NULL）墓碑不互并。
	devA, devB := "d_deviceisoa01", "d_deviceisob01"
	if _, err := r.DeleteWindow(ctx, user, &devA, h(20), h(21)); err != nil {
		t.Fatal(err)
	}
	mkAt := func(start time.Time) model.UsageTokenBucket {
		return model.UsageTokenBucket{
			BucketStart: start, BucketSize: "hour", Source: "chat",
			AgentKey: "chat", ProviderKey: "builtin", ModelKey: "default",
			PromptTokens: 1, TotalTokens: 1, Requests: 1,
		}
	}
	resA, err := r.IngestBuckets(ctx, user, model.UsageTokenIngestInput{
		DeviceID: devA, Buckets: []model.UsageTokenBucket{mkAt(h(20))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resA.Accepted != 0 || len(resA.Discarded) != 1 {
		t.Errorf("device A: accepted=%d discarded=%d, want 0/1", resA.Accepted, len(resA.Discarded))
	}
	resB, err := r.IngestBuckets(ctx, user, model.UsageTokenIngestInput{
		DeviceID: devB, Buckets: []model.UsageTokenBucket{mkAt(h(20))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resB.Accepted != 1 {
		t.Errorf("device B: accepted=%d, want 1 (设备级墓碑不得误伤其它设备)", resB.Accepted)
	}
	// 同范围的全设备墓碑与 devA 的设备级墓碑不互并（不同设备域）。
	if _, err := r.DeleteWindow(ctx, user, nil, h(20), h(21)); err != nil {
		t.Fatal(err)
	}
	var nullCount, devACount int
	if err := db.GetContext(ctx, &nullCount,
		`SELECT COUNT(*) FROM usage_token_tombstones WHERE user_id = $1 AND device_id IS NULL`, user); err != nil {
		t.Fatal(err)
	}
	if err := db.GetContext(ctx, &devACount,
		`SELECT COUNT(*) FROM usage_token_tombstones WHERE user_id = $1 AND device_id = $2`, user, devA); err != nil {
		t.Fatal(err)
	}
	if nullCount != 3 || devACount != 1 {
		t.Errorf("null/devA tombstones = %d/%d, want 3/1（设备域不互并）", nullCount, devACount)
	}
}

// TestUsageTokenIntegration_MixedBucketSizeNotDoubleCounted（契约 §6 MUST-2 /
// §10.7）：hour 部署下库内混存 day 桶（如配置变更前的遗留）时，所有读查询
// 只统计当前 bucket_size 的桶，不双计。
func TestUsageTokenIntegration_MixedBucketSizeNotDoubleCounted(t *testing.T) {
	db := usageTokenTestDB(t)
	e := usageTokenTestEngine(t, db, "hour", 180, 500000)
	user := usageTokenUser(t, db)
	day := usageTokenFixtureDay()
	dev := "d_mixbucketsize"

	// 正常上传一个 hour 桶（total 15）。
	w := usageTokenReq(t, e, http.MethodPost, "/user/usage/tokens/batch", user,
		usageTokenBatchBody(dev, usageTokenBucketJSON(day.Add(time.Hour), "chat", "chat", "builtin", "default", 10, 5, 15, 1)))
	decodeOK(t, w)

	// 直接 SQL 种一条同窗口的 day 桶（摄入侧 MUST-1 会拒绝这种上传，只能
	// 经库内遗留进入）。
	if _, err := db.Exec(`INSERT INTO usage_token_buckets
		(user_id, device_id, bucket_start, bucket_size, source, agent_key, provider_key, model_key, prompt_tokens, completion_tokens, total_tokens, requests)
		VALUES ($1, $2, $3, 'day', 'chat', 'chat', 'builtin', 'default', 100, 100, 200, 9)`,
		user, dev, day); err != nil {
		t.Fatalf("seed day bucket: %v", err)
	}

	// summary 只计 hour 桶。
	if got := summaryTotal(t, e, user); got != 15 {
		t.Errorf("summary total = %v, want 15（day 桶不得计入）", got)
	}
	// export 同理：只返回 hour 桶一行。
	w = usageTokenReq(t, e, http.MethodGet, "/user/usage/tokens/export", user, "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Result-Returned"); got != "1" {
		t.Errorf("X-Result-Returned = %q, want 1（day 桶不得出现在导出）", got)
	}
}
