// admin_quota_policies.go — 配额策略(inference_policy_versions)运营端点
// (spec: docs/superpowers/specs/2026-09-26-admin-quota-policies-design.md;
// 需求: yunhou-users-quota-policies-api-requirements.md)。
//
//	GET   /admin/quota-policies?name=&status=&limit=&offset=  列表
//	      (name ASC, revision DESC)
//	GET   /admin/quota-policies/:id                           详情 +
//	      referenced_by(active 权益引用数,退役前评估)
//	POST  /admin/quota-policies                               新建/出新版
//	      (revision 服务端 max+1 派生,draft;201)
//	PATCH /admin/quota-policies/:id                           仅 draft 可改
//	      (presence 语义:缺席保留、显式 null 清空;空修改 400)
//	POST  /admin/quota-policies/:id/publish                   draft→published
//	      (同事务转同名旧 published 为 superseded;幂等重放 200)
//	POST  /admin/quota-policies/:id/retire?force=true         →retired
//	      (有 active 权益引用默认 409 + referenced_by,Q1)
//
// 全部挂 models:manage 组(Q2 过渡口径,文档标注未来收敛 quota:manage);
// 写端点 reason 必填;写 + 审计同事务;micros/tpm string 渲染(int64 JSON
// 精度安全,与 price-versions 同约定)。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// AdminQuotaPoliciesHandler exposes the quota-policy operator surface.
type AdminQuotaPoliciesHandler struct {
	svc *management.QuotaPolicyService
}

func NewAdminQuotaPoliciesHandler(svc *management.QuotaPolicyService) *AdminQuotaPoliciesHandler {
	return &AdminQuotaPoliciesHandler{svc: svc}
}

// Register mounts the endpoints; the caller wraps the group with the
// models:manage authorization middleware.
func (h *AdminQuotaPoliciesHandler) Register(g *gin.RouterGroup) {
	g.GET("/quota-policies", h.List)
	g.GET("/quota-policies/:id", h.Get)
	g.POST("/quota-policies", h.Create)
	g.PATCH("/quota-policies/:id", h.Update)
	g.POST("/quota-policies/:id/publish", h.Publish)
	g.POST("/quota-policies/:id/retire", h.Retire)
}

// quotaPolicyView is the wire shape: micros/tpm as strings; rpm/concurrency
// as numbers; published_at omitted while draft; referenced_by 仅详情出现。
type quotaPolicyView struct {
	PolicyVersionID  string     `json:"policy_version_id"`
	Name             string     `json:"name"`
	Revision         int        `json:"revision"`
	ModelIDs         []string   `json:"model_ids"`
	FiveHourLimit    *string    `json:"five_hour_limit_micros"`
	WeeklyLimit      *string    `json:"weekly_limit_micros"`
	MonthlyLimit     *string    `json:"monthly_limit_micros"`
	RPMLimit         *int       `json:"rpm_limit"`
	TPMLimit         *string    `json:"tpm_limit"`
	ConcurrencyLimit *int       `json:"concurrency_limit"`
	OveragePolicy    string     `json:"overage_policy"`
	Status           string     `json:"status"`
	CreatedAt        time.Time  `json:"created_at"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	ReferencedBy     *int       `json:"referenced_by,omitempty"`
	// ConfigRefs 是配置级引用数(plan_benefit_configs + PAYG 配置行),
	// 仅详情/退役 409 出现(评审轮3 finding 2)。
	ConfigRefs *int `json:"referenced_by_configs,omitempty"`
}

func i64str(p *int64) *string {
	if p == nil {
		return nil
	}
	s := strconv.FormatInt(*p, 10)
	return &s
}

func toQuotaPolicyView(p *management.QuotaPolicyInfo, referencedBy, configRefs *int) quotaPolicyView {
	var published *time.Time
	if p.PublishedAt != nil {
		t := p.PublishedAt.UTC()
		published = &t
	}
	modelIDs := p.ModelIDs
	if modelIDs == nil {
		modelIDs = []string{}
	}
	return quotaPolicyView{
		PolicyVersionID: p.ID, Name: p.Name, Revision: p.Revision, ModelIDs: modelIDs,
		FiveHourLimit: i64str(p.FiveHourLimit), WeeklyLimit: i64str(p.WeeklyLimit),
		MonthlyLimit: i64str(p.MonthlyLimit),
		RPMLimit:     p.RPMLimit, TPMLimit: i64str(p.TPMLimit),
		ConcurrencyLimit: p.ConcurrencyLimit,
		OveragePolicy:    p.OveragePolicy, Status: p.Status,
		CreatedAt: p.CreatedAt.UTC(), PublishedAt: published,
		ReferencedBy: referencedBy, ConfigRefs: configRefs,
	}
}

type createQuotaPolicyRequest struct {
	Name             string   `json:"name"`
	ModelIDs         []string `json:"model_ids"`
	FiveHourLimit    *int64   `json:"five_hour_limit_micros"`
	WeeklyLimit      *int64   `json:"weekly_limit_micros"`
	MonthlyLimit     *int64   `json:"monthly_limit_micros"`
	RPMLimit         *int     `json:"rpm_limit"`
	TPMLimit         *int64   `json:"tpm_limit"`
	ConcurrencyLimit *int     `json:"concurrency_limit"`
	OveragePolicy    string   `json:"overage_policy"`
	Reason           string   `json:"reason"`
}

// Create POST /quota-policies — 新建/出新版(201)。revision 服务端派生;
// 并发同 name 撞唯一键 → 409。
func (h *AdminQuotaPoliciesHandler) Create(c *gin.Context) {
	var req createQuotaPolicyRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	created, err := h.svc.Create(c.Request.Context(), actorOf(c), management.CreateQuotaPolicyInput{
		Name: req.Name, ModelIDs: req.ModelIDs,
		FiveHourLimit: req.FiveHourLimit, WeeklyLimit: req.WeeklyLimit, MonthlyLimit: req.MonthlyLimit,
		RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit, ConcurrencyLimit: req.ConcurrencyLimit,
		OveragePolicy: req.OveragePolicy, Reason: req.Reason,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"code": 0, "data": toQuotaPolicyView(created, nil, nil)})
}

// quotaPolicyMutableFields 是 PATCH 可修改字段全集(presence 检测与未知
// 字段拒绝共用一份名单;reason 单独处理)。
var quotaPolicyMutableFields = map[string]bool{
	"model_ids":              true,
	"five_hour_limit_micros": true, "weekly_limit_micros": true, "monthly_limit_micros": true,
	"rpm_limit": true, "tpm_limit": true, "concurrency_limit": true,
	"overage_policy": true,
}

// Update PATCH /quota-policies/:id — 仅 draft 可改。presence 语义:缺席
// 字段保留、显式 null 清空该限制项;空修改 400。
func (h *AdminQuotaPoliciesHandler) Update(c *gin.Context) {
	var raw map[string]json.RawMessage
	if err := strictBindJSON(c, &raw); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	in := management.UpdateQuotaPolicyInput{Set: map[string]bool{}}
	for k := range raw {
		if k == "reason" {
			continue
		}
		if !quotaPolicyMutableFields[k] {
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown field rejected): "+k))
			return
		}
		in.Set[k] = true
	}
	if v, ok := raw["reason"]; ok {
		if err := json.Unmarshal(v, &in.Reason); err != nil {
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid reason"))
			return
		}
	}
	// 各可空字段:键存在且非 null 才解出值;键存在且为 null → 指针 nil
	// (清空语义)。
	decode := func(key string, dst any) error {
		v, ok := raw[key]
		if !ok || string(v) == "null" {
			return nil
		}
		return json.Unmarshal(v, dst)
	}
	if err := decode("model_ids", &in.ModelIDs); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid model_ids"))
		return
	}
	for _, f := range []struct {
		key string
		dst any
	}{
		{"five_hour_limit_micros", &in.FiveHourLimit},
		{"weekly_limit_micros", &in.WeeklyLimit},
		{"monthly_limit_micros", &in.MonthlyLimit},
		{"rpm_limit", &in.RPMLimit},
		{"tpm_limit", &in.TPMLimit},
		{"concurrency_limit", &in.ConcurrencyLimit},
	} {
		if err := decode(f.key, f.dst); err != nil {
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid "+f.key))
			return
		}
	}
	if err := decode("overage_policy", &in.OveragePolicy); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid overage_policy"))
		return
	}
	updated, err := h.svc.Update(c.Request.Context(), actorOf(c), c.Param("id"), in)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toQuotaPolicyView(updated, nil, nil))
}

// Publish POST /quota-policies/:id/publish — draft → published;同事务把
// 同名旧 published 转 superseded;重复调用幂等 200(不产生第二条审计)。
func (h *AdminQuotaPoliciesHandler) Publish(c *gin.Context) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	published, err := h.svc.Publish(c.Request.Context(), actorOf(c), c.Param("id"), req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toQuotaPolicyView(published, nil, nil))
}

// Retire POST /quota-policies/:id/retire?force=true — Q1:有 active 权益
// 引用默认 409 + referenced_by;force 放行;draft 直接;幂等重放 200。
func (h *AdminQuotaPoliciesHandler) Retire(c *gin.Context) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	force := c.Query("force") == "true"
	retired, err := h.svc.Retire(c.Request.Context(), actorOf(c), c.Param("id"), req.Reason, force)
	if err != nil {
		var refErr *management.QuotaPolicyRetireConflictError
		if errors.As(err, &refErr) && refErr.Info != nil {
			c.JSON(http.StatusConflict, gin.H{
				"code":    http.StatusConflict,
				"message": refErr.Error(),
				"data":    toQuotaPolicyView(refErr.Info, &refErr.ReferencedBy, &refErr.ConfigRefs),
			})
			return
		}
		fail(c, err)
		return
	}
	ok(c, toQuotaPolicyView(retired, nil, nil))
}

// Get GET /quota-policies/:id — 详情 + referenced_by(active 权益引用数)
// + referenced_by_configs(配置级引用数)(退役前评估,只读统计)。
func (h *AdminQuotaPoliciesHandler) Get(c *gin.Context) {
	info, referencedBy, configRefs, err := h.svc.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toQuotaPolicyView(info, &referencedBy, &configRefs))
}

// List GET /quota-policies?name=&status=&limit=&offset= — items 空时为
// [] 而非 null;status 白名单校验;排序 (name ASC, revision DESC)。
func (h *AdminQuotaPoliciesHandler) List(c *gin.Context) {
	filter := management.QuotaPolicyFilter{
		Name:  c.Query("name"),
		Limit: parseLimit(c, 100),
	}
	if st := c.Query("status"); st != "" {
		switch st {
		case "draft", "published", "superseded", "retired":
			filter.Status = st
		default:
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid query: unknown status "+st))
			return
		}
	}
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fail(c, domain.NewError(domain.CodeInvalidInput, "invalid query: offset must be >= 0"))
			return
		}
		filter.Offset = n
	}
	items, err := h.svc.List(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]quotaPolicyView, 0, len(items))
	for i := range items {
		out = append(out, toQuotaPolicyView(&items[i], nil, nil))
	}
	ok(c, gin.H{"items": out, "limit": filter.Limit, "offset": filter.Offset})
}
