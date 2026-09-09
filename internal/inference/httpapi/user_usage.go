// user_usage.go — /user/model-usage 族（Task 11；设计 §9.2：时间序列及请
// 求分页，按模型/Key 分组；显示计量状态与 as_of）。
//
//   - GET /user/model-usage/summary  — 分组聚合 + UTC 日序列；带 as_of 与
//     complete_through（统计截止时刻；本实现直读权威表，二者相等）。
//   - GET /user/model-usage/requests — 请求明细 keyset 分页（游标稳定排
//     序 created_at DESC, id DESC）；每行带计量完整性 usage_status 与
//     reserved/charge/reversed/net（十进制字符串；未结算为 null）。
//
// 用量来源只有 inference_requests/usage_records/ledger —— 绝不使用
// usage_events 心跳表。token 桶为 JSON 整数（计数），null = 未知（未报告），
// 未知绝不记 0。

package httpapi

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// UserUsageHandler serves the customer usage endpoints.
type UserUsageHandler struct {
	svc *management.UsageViewService
}

func NewUserUsageHandler(svc *management.UsageViewService) *UserUsageHandler {
	return &UserUsageHandler{svc: svc}
}

// Register mounts the endpoints on a JWT-authenticated group.
func (h *UserUsageHandler) Register(g *gin.RouterGroup) {
	g.GET("/model-usage/summary", h.Summary)
	g.GET("/model-usage/requests", h.ListRequests)
}

// usageTokensJSON renders the normalized buckets; a nil bucket is JSON null
// （未报告 ≠ 0）. The whole object is null when no usage record exists yet.
type usageTokensJSON struct {
	InputTokens      *int64 `json:"input_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"`
}

func toUsageTokensJSON(b domain.UsageBuckets) usageTokensJSON {
	return usageTokensJSON{
		InputTokens:      b.InputTokens,
		CacheReadTokens:  b.CacheReadTokens,
		CacheWriteTokens: b.CacheWriteTokens,
		OutputTokens:     b.OutputTokens,
		ReasoningTokens:  b.ReasoningTokens,
	}
}

// usageGroupJSON is one summary group. The non-grouping dimension key is
// null (group_by=model → api_key_id null, and vice versa); under
// group_by=key a null api_key_id is the JWT/facade-originated group.
type usageGroupJSON struct {
	ModelID   *string `json:"model_id"`
	APIKeyID  *string `json:"api_key_id"`
	KeyName   string  `json:"key_name"`
	KeyPrefix string  `json:"key_prefix"`

	Requests              int64 `json:"requests"`
	Settled               int64 `json:"settled"`
	Released              int64 `json:"released"`
	InFlight              int64 `json:"in_flight"`
	Failed                int64 `json:"failed"`
	ReconciliationPending int64 `json:"reconciliation_pending"`

	Reported  int64 `json:"reported"`
	Estimated int64 `json:"estimated"`
	Unknown   int64 `json:"unknown"`
	Pending   int64 `json:"pending"`

	ChargeMicros   string `json:"charge_micros"`
	ReversedMicros string `json:"reversed_micros"`
	AdjustedMicros string `json:"adjusted_micros"`
	NetMicros      string `json:"net_micros"`

	Tokens usageTokensJSON `json:"tokens"`
}

func toUsageGroupJSON(g management.UsageGroup) usageGroupJSON {
	return usageGroupJSON{
		ModelID: g.ModelID, APIKeyID: g.APIKeyID,
		KeyName: g.KeyName, KeyPrefix: g.KeyPrefix,
		Requests: g.RequestsTotal, Settled: g.SettledRequests, Released: g.ReleasedRequests,
		InFlight: g.InFlightRequests, Failed: g.FailedRequests,
		ReconciliationPending: g.ReconciliationPending,
		Reported: g.Reported, Estimated: g.Estimated, Unknown: g.Unknown, Pending: g.Pending,
		ChargeMicros:   strconv.FormatInt(g.ChargeMicros, 10),
		ReversedMicros: strconv.FormatInt(g.ReversedMicros, 10),
		AdjustedMicros: strconv.FormatInt(g.AdjustedMicros, 10),
		NetMicros:      strconv.FormatInt(g.NetMicros(), 10),
		Tokens:         toUsageTokensJSON(g.Tokens),
	}
}

// Summary handles GET /user/model-usage/summary?from=&to=&group_by=&model_id=&api_key_id=.
func (h *UserUsageHandler) Summary(c *gin.Context) {
	f, err := parseSummaryParams(c)
	if err != nil {
		fail(c, err)
		return
	}
	view, err := h.svc.Summary(c.Request.Context(), userIDOf(c), f)
	if err != nil {
		fail(c, err)
		return
	}
	groups := make([]usageGroupJSON, 0, len(view.Groups))
	for _, g := range view.Groups {
		groups = append(groups, toUsageGroupJSON(g))
	}
	series := make([]gin.H, 0, len(view.Series))
	for _, b := range view.Series {
		series = append(series, gin.H{
			"bucket_start":    b.BucketStart.UTC().Format(time.RFC3339),
			"requests":        b.RequestsTotal,
			"charge_micros":   strconv.FormatInt(b.ChargeMicros, 10),
			"reversed_micros": strconv.FormatInt(b.ReversedMicros, 10),
			"adjusted_micros": strconv.FormatInt(b.AdjustedMicros, 10),
			"net_micros":      strconv.FormatInt(b.NetMicros(), 10),
			"reported":        b.Reported,
			"estimated":       b.Estimated,
			"unknown":         b.Unknown,
		})
	}
	ok(c, gin.H{
		"server_time":      view.ServerTime.UTC().Format(time.RFC3339),
		"as_of":            view.AsOf.UTC().Format(time.RFC3339),
		"complete_through": view.CompleteThrough.UTC().Format(time.RFC3339),
		"unit":             view.Unit,
		"range":            gin.H{"from": view.From.Format(time.RFC3339), "to": view.To.Format(time.RFC3339)},
		"group_by":         string(view.GroupBy),
		"groups":           groups,
		"series":           series,
	})
}

func parseSummaryParams(c *gin.Context) (management.UsageSummaryFilter, error) {
	var f management.UsageSummaryFilter
	var err error
	if f.From, err = parseOptionalRFC3339(c.Query("from"), "from"); err != nil {
		return f, err
	}
	if f.To, err = parseOptionalRFC3339(c.Query("to"), "to"); err != nil {
		return f, err
	}
	f.GroupBy = management.UsageGroupBy(c.Query("group_by"))
	f.ModelID = c.Query("model_id")
	f.APIKeyID = c.Query("api_key_id")
	return f, nil
}

// usageRequestJSON is one logical request row. charge_micros 派生自不可变
// 账本 charge 分录（未结算/已释放为 null —— 预占中： reserved 持有、
// charge 未知）；reversed_micros 与 adjusted_micros（debit + / credit −
// 签名合计）承载结算后修正；net = charge − reversed + adjusted。
type usageRequestJSON struct {
	RequestID  string  `json:"request_id"`
	ModelID    string  `json:"model_id"`
	APIKeyID   *string `json:"api_key_id"`
	KeyName    string  `json:"key_name"`
	KeyPrefix  string  `json:"key_prefix"`
	Protocol   string  `json:"protocol"`
	Stream     bool    `json:"stream"`
	Status     string  `json:"status"`
	UsageStatus string `json:"usage_status"`

	ReservedMicros *string `json:"reserved_micros"`
	ChargeMicros   *string `json:"charge_micros"`
	ReversedMicros string  `json:"reversed_micros"`
	AdjustedMicros string  `json:"adjusted_micros"`
	NetMicros      *string `json:"net_micros"`

	Tokens *usageTokensJSON `json:"tokens"`

	CreatedAt   string  `json:"created_at"`
	AdmittedAt  *string `json:"admitted_at"`
	CompletedAt *string `json:"completed_at"`
}

func toUsageRequestJSON(r management.RequestRow) usageRequestJSON {
	out := usageRequestJSON{
		RequestID: r.ID, ModelID: r.ModelID, APIKeyID: r.APIKeyID,
		KeyName: r.KeyName, KeyPrefix: r.KeyPrefix,
		Protocol: r.Protocol, Stream: r.Stream,
		Status: string(r.Status), UsageStatus: string(r.UsageStatus),
		ReversedMicros: strconv.FormatInt(r.ReversedMicros, 10),
		AdjustedMicros: strconv.FormatInt(r.AdjustedMicros, 10),
		CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
		AdmittedAt:     rfc3339Ptr(r.AdmittedAt),
		CompletedAt:    rfc3339Ptr(r.CompletedAt),
	}
	if r.ReservedMicros != nil {
		s := strconv.FormatInt(int64(*r.ReservedMicros), 10)
		out.ReservedMicros = &s
	}
	if r.ChargeMicros != nil {
		s := strconv.FormatInt(*r.ChargeMicros, 10)
		out.ChargeMicros = &s
	}
	if n := r.NetMicrosPtr(); n != nil {
		s := strconv.FormatInt(*n, 10)
		out.NetMicros = &s
	}
	if r.HasUsage {
		t := toUsageTokensJSON(r.Tokens)
		out.Tokens = &t
	}
	return out
}

// ListRequests handles
// GET /user/model-usage/requests?from=&to=&model_id=&api_key_id=&cursor=&limit=.
func (h *UserUsageHandler) ListRequests(c *gin.Context) {
	f, err := parseRequestListParams(c)
	if err != nil {
		fail(c, err)
		return
	}
	page, err := h.svc.ListRequests(c.Request.Context(), userIDOf(c), f)
	if err != nil {
		fail(c, err)
		return
	}
	items := make([]usageRequestJSON, 0, len(page.Items))
	for _, r := range page.Items {
		items = append(items, toUsageRequestJSON(r))
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	ok(c, gin.H{
		"server_time": page.ServerTime.UTC().Format(time.RFC3339),
		"as_of":       page.AsOf.UTC().Format(time.RFC3339),
		"unit":        page.Unit,
		"range":       gin.H{"from": page.From.Format(time.RFC3339), "to": page.To.Format(time.RFC3339)},
		"items":       items,
		"next_cursor": next,
		"limit":       page.Limit,
	})
}

func parseRequestListParams(c *gin.Context) (management.RequestListFilter, error) {
	var f management.RequestListFilter
	var err error
	if f.From, err = parseOptionalRFC3339(c.Query("from"), "from"); err != nil {
		return f, err
	}
	if f.To, err = parseOptionalRFC3339(c.Query("to"), "to"); err != nil {
		return f, err
	}
	f.ModelID = c.Query("model_id")
	f.APIKeyID = c.Query("api_key_id")
	if s := c.Query("cursor"); s != "" {
		cur, err := management.DecodeRequestCursor(s)
		if err != nil {
			return f, err
		}
		f.Cursor = &cur
	}
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			return f, domain.NewError(domain.CodeInvalidInput, "limit must be 1..100")
		}
		f.Limit = n
	}
	return f, nil
}

// parseOptionalRFC3339 parses an optional RFC3339 query value; empty → zero
// time (the service applies the documented defaults against its own clock).
func parseOptionalRFC3339(raw, name string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, domain.NewError(domain.CodeInvalidInput, name+" must be RFC3339 (UTC)")
	}
	return t, nil
}
