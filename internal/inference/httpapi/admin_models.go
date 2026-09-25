// Package httpapi hosts the HTTP handlers of the inference module:
// the standard protocol surface (/v1/*, later tasks) and the management
// API (admin_models.go). Management responses use the repo-wide
// {code,data,message} envelope (设计 §9.1).
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

// AdminModelsHandler exposes the model-catalog management API. Task 3
// mounts ONLY the read-only GET routes (模型/部署列表查询); the write
// handlers are implemented and tested here but stay unmounted until Task 4
// lands operator authorization — an unauthenticated write surface must not
// exist even for a moment (任务书: 完成 Task 4 的运营授权前不挂载可写管理路由).
type AdminModelsHandler struct {
	mgr *management.CatalogManager
}

// NewAdminModelsHandler builds the handler over the catalog manager.
func NewAdminModelsHandler(mgr *management.CatalogManager) *AdminModelsHandler {
	return &AdminModelsHandler{mgr: mgr}
}

// RegisterReadOnly mounts the read-only catalog queries. Called by
// router.Setup today.
func (h *AdminModelsHandler) RegisterReadOnly(g *gin.RouterGroup) {
	g.GET("/models", h.ListModels)
	g.GET("/models/:id", h.GetModel)
	g.GET("/models/:id/routes", h.ListModelRoutes)
	g.GET("/deployments", h.ListDeployments)
	g.GET("/catalog/revisions", h.ListRevisions)
	g.GET("/catalog/active", h.GetActiveRevision)
}

// RegisterWrite mounts the catalog write endpoints. Implemented in Task 3
// but deliberately NOT called by router.Setup — Task 4 calls it only after
// the operator-authorization middleware exists.
func (h *AdminModelsHandler) RegisterWrite(g *gin.RouterGroup) {
	g.POST("/models", h.CreateModel)
	g.PATCH("/models/:id", h.UpdateModel)
	g.POST("/models/:id/lifecycle", h.SetModelLifecycle)
	g.DELETE("/models/:id", h.DeleteModel)
	g.POST("/providers", h.CreateProvider)
	g.PATCH("/providers/:id", h.UpdateProvider)
	g.DELETE("/providers/:id", h.DeleteProvider)
	g.POST("/deployments", h.CreateDeployment)
	g.PATCH("/deployments/:id", h.UpdateDeployment)
	g.DELETE("/deployments/:id", h.DeleteDeployment)
	g.POST("/models/:id/routes", h.CreateRoute)
	g.PATCH("/routes/:route_id", h.UpdateRoute)
	g.DELETE("/routes/:route_id", h.DeleteRoute)
	g.POST("/catalog/publish", h.Publish)
	g.POST("/catalog/rollback", h.Rollback)
}

// --- DTOs ---

type modelDTO struct {
	ID                string    `json:"id"`
	DisplayName       string    `json:"display_name"`
	Lifecycle         string    `json:"lifecycle"`
	ModelVersion      string    `json:"model_version"`
	Aliases           []string  `json:"aliases"`
	InputModalities   []string  `json:"input_modalities"`
	OutputModalities  []string  `json:"output_modalities"`
	ContextTokens     int       `json:"context_tokens"`
	MaxOutputTokens   int       `json:"max_output_tokens"`
	Protocols         []string  `json:"protocols"`
	SupportsTools     bool      `json:"supports_tools"`
	SupportsReasoning bool      `json:"supports_reasoning"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func toModelDTO(m *domain.Model) modelDTO {
	protocols := make([]string, 0, len(m.Protocols))
	for _, p := range m.Protocols {
		protocols = append(protocols, string(p))
	}
	return modelDTO{
		ID: m.ID, DisplayName: m.DisplayName, Lifecycle: string(m.Lifecycle),
		ModelVersion: m.ModelVersion, Aliases: m.Aliases,
		InputModalities: m.InputModalities, OutputModalities: m.OutputModalities,
		ContextTokens: m.ContextTokens, MaxOutputTokens: m.MaxOutputTokens,
		Protocols: protocols, SupportsTools: m.SupportsTools,
		SupportsReasoning: m.SupportsReasoning,
		CreatedAt:         m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// modelCreateRequest is the POST /models body. An explicit lifecycle is
// rejected unless it is "draft" (安全审查 I-1): new models always start as
// draft and promote only via POST /models/:id/lifecycle, so a create can
// never skip the draft→publish state machine. Reason 是可选的运营理由，
// 透传到审计事件（评审轮2 finding6）。
type modelCreateRequest struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Lifecycle         string   `json:"lifecycle"`
	ModelVersion      string   `json:"model_version"`
	Aliases           []string `json:"aliases"`
	InputModalities   []string `json:"input_modalities"`
	OutputModalities  []string `json:"output_modalities"`
	ContextTokens     int      `json:"context_tokens"`
	MaxOutputTokens   int      `json:"max_output_tokens"`
	Protocols         []string `json:"protocols"`
	SupportsTools     bool     `json:"supports_tools"`
	SupportsReasoning bool     `json:"supports_reasoning"`
	Reason            string   `json:"reason"`
}

func (r *modelCreateRequest) toDomain() *domain.Model {
	m := &domain.Model{
		ID: r.ID, DisplayName: r.DisplayName, ModelVersion: r.ModelVersion,
		Aliases: r.Aliases, InputModalities: r.InputModalities,
		OutputModalities: r.OutputModalities, ContextTokens: r.ContextTokens,
		MaxOutputTokens: r.MaxOutputTokens, SupportsTools: r.SupportsTools,
		SupportsReasoning: r.SupportsReasoning,
	}
	if r.Lifecycle != "" {
		m.Lifecycle = domain.Lifecycle(r.Lifecycle)
	}
	for _, p := range r.Protocols {
		m.Protocols = append(m.Protocols, domain.Protocol(p))
	}
	return m
}

// modelUpdateRequest is the PATCH /models/:id body: read-modify-write ——
// 每个字段都是指针，缺省（nil）保留存量值（安全审查 I-1：此前省略字段
// 会被重置为零值/draft，运营改个 display_name 就能把在售 active 模型打回
// 草稿）。DTO 刻意不含 lifecycle/id 字段：strictBindJSON 将其按未知字段
// 拒绝（400），lifecycle 只能经专用的 POST /models/:id/lifecycle 状态机
// 端点流转（retired→active 这类跳变在状态机层同样被拒）。
type modelUpdateRequest struct {
	DisplayName       *string    `json:"display_name"`
	ModelVersion      *string    `json:"model_version"`
	Aliases           *[]string  `json:"aliases"`
	InputModalities   *[]string  `json:"input_modalities"`
	OutputModalities  *[]string  `json:"output_modalities"`
	ContextTokens     *int       `json:"context_tokens"`
	MaxOutputTokens   *int       `json:"max_output_tokens"`
	Protocols         *[]string  `json:"protocols"`
	SupportsTools     *bool      `json:"supports_tools"`
	SupportsReasoning *bool      `json:"supports_reasoning"`
	UpdatedAt         *time.Time `json:"updated_at"`
	Reason            string     `json:"reason"`
}

// apply overlays only the provided fields onto the stored model.
func (r *modelUpdateRequest) apply(m *domain.Model) {
	if r.DisplayName != nil {
		m.DisplayName = *r.DisplayName
	}
	if r.ModelVersion != nil {
		m.ModelVersion = *r.ModelVersion
	}
	if r.Aliases != nil {
		m.Aliases = *r.Aliases
	}
	if r.InputModalities != nil {
		m.InputModalities = *r.InputModalities
	}
	if r.OutputModalities != nil {
		m.OutputModalities = *r.OutputModalities
	}
	if r.ContextTokens != nil {
		m.ContextTokens = *r.ContextTokens
	}
	if r.MaxOutputTokens != nil {
		m.MaxOutputTokens = *r.MaxOutputTokens
	}
	if r.Protocols != nil {
		m.Protocols = m.Protocols[:0]
		for _, p := range *r.Protocols {
			m.Protocols = append(m.Protocols, domain.Protocol(p))
		}
	}
	if r.SupportsTools != nil {
		m.SupportsTools = *r.SupportsTools
	}
	if r.SupportsReasoning != nil {
		m.SupportsReasoning = *r.SupportsReasoning
	}
	if r.UpdatedAt != nil {
		m.UpdatedAt = *r.UpdatedAt
	}
}

type providerDTO struct {
	ID          string    `json:"id"`
	Code        string    `json:"code"`
	DisplayName string    `json:"display_name"`
	AccessType  string    `json:"access_type"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type providerWriteRequest struct {
	Code        string            `json:"code"`
	DisplayName string            `json:"display_name"`
	AccessType  domain.AccessType `json:"access_type"`
	Status      string            `json:"status"`
	UpdatedAt   *time.Time        `json:"updated_at"`
	Reason      string            `json:"reason"` // 可选运营理由 → 审计
}

type deploymentDTO struct {
	ID               string          `json:"id"`
	ProviderID       string          `json:"provider_id"`
	UpstreamModel    string          `json:"upstream_model"`
	BaseURL          string          `json:"base_url"`
	Protocol         string          `json:"protocol"`
	Region           string          `json:"region"`
	ConnectTimeoutMs int64           `json:"connect_timeout_ms"`
	RequestTimeoutMs int64           `json:"request_timeout_ms"`
	ConfigVersion    int             `json:"config_version"`
	Config           json.RawMessage `json:"config"`
	Status           string          `json:"status"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

func toDeploymentDTO(d *domain.Deployment) deploymentDTO {
	return deploymentDTO{
		ID: d.ID, ProviderID: d.ProviderID, UpstreamModel: d.UpstreamModel,
		BaseURL: d.BaseURL, Protocol: string(d.Protocol), Region: d.Region,
		ConnectTimeoutMs: int64(d.ConnectTimeout / time.Millisecond),
		RequestTimeoutMs: int64(d.RequestTimeout / time.Millisecond),
		ConfigVersion:    d.ConfigVersion,
		Config:           d.Config.Raw,
		Status:           string(d.Status),
		CreatedAt:        d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

type deploymentWriteRequest struct {
	ProviderID       string          `json:"provider_id"`
	UpstreamModel    string          `json:"upstream_model"`
	BaseURL          string          `json:"base_url"`
	Protocol         string          `json:"protocol"`
	Region           string          `json:"region"`
	ConnectTimeoutMs int64           `json:"connect_timeout_ms"`
	RequestTimeoutMs int64           `json:"request_timeout_ms"`
	Config           json.RawMessage `json:"config"`
	Status           string          `json:"status"`
	ConfigVersion    int             `json:"config_version"`
	Reason           string          `json:"reason"` // 可选运营理由 → 审计
}

func (r *deploymentWriteRequest) toDomain(id string) *domain.Deployment {
	d := &domain.Deployment{
		ID: id, ProviderID: r.ProviderID, UpstreamModel: r.UpstreamModel,
		BaseURL: r.BaseURL, Protocol: domain.Protocol(r.Protocol), Region: r.Region,
		ConnectTimeout: time.Duration(r.ConnectTimeoutMs) * time.Millisecond,
		RequestTimeout: time.Duration(r.RequestTimeoutMs) * time.Millisecond,
		ConfigVersion:  r.ConfigVersion,
		Config:         domain.ExtensionConfig{SchemaVersion: 1, Raw: r.Config},
	}
	if r.Status != "" {
		d.Status = domain.DeploymentStatus(r.Status)
	}
	return d
}

type routeDTO struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"model_id"`
	DeploymentID string    `json:"deployment_id"`
	Priority     int       `json:"priority"`
	Weight       int       `json:"weight"`
	Capabilities []string  `json:"capabilities"`
	PoolStrategy string    `json:"pool_strategy"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func toRouteDTO(r *domain.ModelRoute) routeDTO {
	return routeDTO{
		ID: r.ID, ModelID: r.ModelID, DeploymentID: r.DeploymentID,
		Priority: r.Priority, Weight: r.Weight, Capabilities: r.Capabilities,
		PoolStrategy: string(r.PoolStrategy), Enabled: r.Enabled,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

type routeWriteRequest struct {
	DeploymentID string     `json:"deployment_id"`
	Priority     int        `json:"priority"`
	Weight       int        `json:"weight"`
	Capabilities []string   `json:"capabilities"`
	PoolStrategy string     `json:"pool_strategy"`
	Enabled      bool       `json:"enabled"`
	UpdatedAt    *time.Time `json:"updated_at"`
	Reason       string     `json:"reason"` // 可选运营理由 → 审计
}

type revisionDTO struct {
	ID          int64      `json:"id"`
	Revision    int        `json:"revision"`
	Status      string     `json:"status"`
	IsActive    bool       `json:"is_active"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toRevisionDTO(r *domain.ConfigRevision) revisionDTO {
	return revisionDTO{
		ID: r.ID, Revision: r.Revision, Status: string(r.Status),
		IsActive: r.IsActive, PublishedAt: r.PublishedAt,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}
}

// --- envelope + error mapping ---

func ok(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": data})
}

func fail(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch domain.CodeOf(err) {
	case domain.CodeInvalidInput:
		status = http.StatusBadRequest
	case domain.CodeInvalidKey:
		status = http.StatusUnauthorized
	case domain.CodeModelNotAllowed:
		status = http.StatusForbidden
	case domain.CodeNotFound:
		status = http.StatusNotFound
	case domain.CodeConflict:
		status = http.StatusConflict
	case domain.CodeRateLimited, domain.CodeQuotaExceeded, domain.CodeInsufficientBalance:
		status = http.StatusTooManyRequests
	}
	if status == http.StatusInternalServerError {
		c.JSON(status, gin.H{"code": status, "data": nil, "message": "internal error"})
		return
	}
	// 评审轮1 M4：非 500 分支只输出 domain.Error 的 Message——err.Error()
	// 会拼上完整 cause 链（厂商 256 字节响应体、DB 约束名等内部细节会进
	// 4xx 响应）；与 /v1 safeMsg 同口径。QuotaExceededError 的 Error() 只含
	// 窗口类别名（无 cause 链），原样输出保留排障信息。
	var de *domain.Error
	var qe *domain.QuotaExceededError
	switch {
	case errors.As(err, &de):
		c.JSON(status, gin.H{"code": status, "data": nil, "message": de.Message})
	case errors.As(err, &qe):
		c.JSON(status, gin.H{"code": status, "data": nil, "message": qe.Error()})
	default:
		// 纯 errors.New 的非 domain 错误落在 4xx 是不可能的（CodeOf 兜底
		// CodeInternal → 500 已脱敏）；防御性固定文案。
		c.JSON(status, gin.H{"code": status, "data": nil, "message": "request failed"})
	}
}

// actorOf extracts the operator attribution set by the Task 4 authorization
// middleware ("user:<uid>@app:<appid>"). A request can only reach a write
// handler after the middleware verified the dual identity, so a missing
// value means the route was mounted without authorization — fail loudly in
// the audit trail rather than silently attributing to a placeholder.
func actorOf(c *gin.Context) string {
	if v, ok := c.Get(OperatorSubjectKey); ok {
		if s, ok2 := v.(string); ok2 && s != "" {
			return s
		}
	}
	return "user:unauthenticated@app:unknown"
}

// reasonOf 取无请求体写端点（DELETE 系、catalog/publish）的可选运营理由
// （?reason=）。有请求体的端点从各自 DTO 的 reason 字段取。未传为空串——
// CatalogManager.firstReason 对空串与未传等价（现状兼容，评审轮2 finding6）。
func reasonOf(c *gin.Context) string { return c.Query("reason") }

// adminListMaxLimit 是所有管理列表端点 ?limit= 的硬上限（评审轮1 m3：
// 无上限的 limit=999999999 一次调用即可触发无界扫描）。超限按上限处理
// 而非报错，与既有「非法值回落默认」同族。
const adminListMaxLimit = 500

func parseLimit(c *gin.Context, def int) int {
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			if n > adminListMaxLimit {
				return adminListMaxLimit
			}
			return n
		}
	}
	return def
}

// --- read-only handlers ---

// ListModels GET /models?lifecycle=&after=&limit=
func (h *AdminModelsHandler) ListModels(c *gin.Context) {
	filter := domain.ModelFilter{
		AfterID: c.Query("after"),
		Limit:   parseLimit(c, 100),
	}
	if lc := c.Query("lifecycle"); lc != "" {
		l := domain.Lifecycle(lc)
		filter.Lifecycle = &l
	}
	models, err := h.mgr.ListModels(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]modelDTO, 0, len(models))
	for i := range models {
		out = append(out, toModelDTO(&models[i]))
	}
	ok(c, gin.H{"models": out})
}

// GetModel GET /models/:id
func (h *AdminModelsHandler) GetModel(c *gin.Context) {
	m, err := h.mgr.GetModel(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toModelDTO(m))
}

// ListModelRoutes GET /models/:id/routes
func (h *AdminModelsHandler) ListModelRoutes(c *gin.Context) {
	routes, err := h.mgr.ListRoutes(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]routeDTO, 0, len(routes))
	for i := range routes {
		out = append(out, toRouteDTO(&routes[i]))
	}
	ok(c, gin.H{"routes": out})
}

// ListDeployments GET /deployments?provider_id=&status=&after=&limit=
func (h *AdminModelsHandler) ListDeployments(c *gin.Context) {
	f := domain.DeploymentFilter{
		ProviderID: c.Query("provider_id"),
		AfterID:    c.Query("after"),
		Limit:      parseLimit(c, 100),
	}
	if st := c.Query("status"); st != "" {
		f.Status = domain.DeploymentStatus(st)
	}
	deployments, err := h.mgr.ListDeployments(c.Request.Context(), f)
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]deploymentDTO, 0, len(deployments))
	for i := range deployments {
		out = append(out, toDeploymentDTO(&deployments[i]))
	}
	ok(c, gin.H{"deployments": out})
}

// ListRevisions GET /catalog/revisions
func (h *AdminModelsHandler) ListRevisions(c *gin.Context) {
	revs, err := h.mgr.ListRevisions(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]revisionDTO, 0, len(revs))
	for i := range revs {
		out = append(out, toRevisionDTO(&revs[i]))
	}
	ok(c, gin.H{"revisions": out})
}

// GetActiveRevision GET /catalog/active
func (h *AdminModelsHandler) GetActiveRevision(c *gin.Context) {
	revs, err := h.mgr.ListRevisions(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	for i := range revs {
		if revs[i].IsActive {
			ok(c, toRevisionDTO(&revs[i]))
			return
		}
	}
	fail(c, domain.NewError(domain.CodeNotFound, "no active catalog revision"))
}

// --- write handlers (implemented; Task 4 mounts them after authz) ---

// CreateModel POST /models — new models always start as draft; an explicit
// non-draft lifecycle is rejected here (400) and again at the service layer.
func (h *AdminModelsHandler) CreateModel(c *gin.Context) {
	var req modelCreateRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	if req.Lifecycle != "" && domain.Lifecycle(req.Lifecycle) != domain.LifecycleDraft {
		fail(c, domain.NewError(domain.CodeInvalidInput,
			"lifecycle must not be set on create: new models start as draft; promote via POST /models/:id/lifecycle"))
		return
	}
	m := req.toDomain()
	if err := h.mgr.CreateModel(c.Request.Context(), actorOf(c), m, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, toModelDTO(m))
}

// UpdateModel PATCH /models/:id — read-modify-write: omitted fields preserve
// their stored values (安全审查 I-1), the updated_at version token is
// required, and lifecycle is not accepted here at all — the DTO has no such
// field, so strictBindJSON answers any lifecycle smuggle with a 400.
// Lifecycle moves only via POST /models/:id/lifecycle.
func (h *AdminModelsHandler) UpdateModel(c *gin.Context) {
	var req modelUpdateRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	m, err := h.mgr.GetModel(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	if req.UpdatedAt == nil {
		fail(c, domain.NewError(domain.CodeInvalidInput,
			"updated_at version token is required (read the model first)"))
		return
	}
	req.apply(m)
	if err := h.mgr.UpdateModel(c.Request.Context(), actorOf(c), m, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, toModelDTO(m))
}

// SetModelLifecycle POST /models/:id/lifecycle {"lifecycle":"active","reason":"…"}
func (h *AdminModelsHandler) SetModelLifecycle(c *gin.Context) {
	var req struct {
		Lifecycle domain.Lifecycle `json:"lifecycle" binding:"required"`
		Reason    string           `json:"reason"` // 可选运营理由 → 审计
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	if err := h.mgr.SetModelLifecycle(c.Request.Context(), actorOf(c), c.Param("id"), req.Lifecycle, req.Reason); err != nil {
		fail(c, err)
		return
	}
	m, err := h.mgr.GetModel(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, toModelDTO(m))
}

// DeleteModel DELETE /models/:id（可选 ?reason= 运营理由 → 审计）
func (h *AdminModelsHandler) DeleteModel(c *gin.Context) {
	if err := h.mgr.DeleteModel(c.Request.Context(), actorOf(c), c.Param("id"), reasonOf(c)); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"deleted": c.Param("id")})
}

// CreateProvider POST /providers
func (h *AdminModelsHandler) CreateProvider(c *gin.Context) {
	var req providerWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	p := &domain.Provider{
		Code: req.Code, DisplayName: req.DisplayName,
		AccessType: req.AccessType, Status: req.Status,
	}
	if err := h.mgr.CreateProvider(c.Request.Context(), actorOf(c), p, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, providerDTO{
		ID: p.ID, Code: p.Code, DisplayName: p.DisplayName,
		AccessType: string(p.AccessType), Status: p.Status,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
}

// UpdateProvider PATCH /providers/:id
func (h *AdminModelsHandler) UpdateProvider(c *gin.Context) {
	var req providerWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	p := &domain.Provider{
		ID: c.Param("id"), Code: req.Code, DisplayName: req.DisplayName,
		AccessType: req.AccessType, Status: req.Status,
	}
	if req.UpdatedAt != nil {
		p.UpdatedAt = *req.UpdatedAt
	}
	if err := h.mgr.UpdateProvider(c.Request.Context(), actorOf(c), p, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, providerDTO{
		ID: p.ID, Code: p.Code, DisplayName: p.DisplayName,
		AccessType: string(p.AccessType), Status: p.Status,
		UpdatedAt: p.UpdatedAt,
	})
}

// DeleteProvider DELETE /providers/:id（可选 ?reason= 运营理由 → 审计）
func (h *AdminModelsHandler) DeleteProvider(c *gin.Context) {
	if err := h.mgr.DeleteProvider(c.Request.Context(), actorOf(c), c.Param("id"), reasonOf(c)); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"deleted": c.Param("id")})
}

// CreateDeployment POST /deployments
func (h *AdminModelsHandler) CreateDeployment(c *gin.Context) {
	var req deploymentWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	d := req.toDomain("")
	if err := h.mgr.CreateDeployment(c.Request.Context(), actorOf(c), d, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, toDeploymentDTO(d))
}

// UpdateDeployment PATCH /deployments/:id — requires config_version token.
func (h *AdminModelsHandler) UpdateDeployment(c *gin.Context) {
	var req deploymentWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	d := req.toDomain(c.Param("id"))
	if err := h.mgr.UpdateDeployment(c.Request.Context(), actorOf(c), d, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, toDeploymentDTO(d))
}

// DeleteDeployment DELETE /deployments/:id（可选 ?reason= 运营理由 → 审计）
func (h *AdminModelsHandler) DeleteDeployment(c *gin.Context) {
	if err := h.mgr.DeleteDeployment(c.Request.Context(), actorOf(c), c.Param("id"), reasonOf(c)); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"deleted": c.Param("id")})
}

// CreateRoute POST /models/:id/routes
func (h *AdminModelsHandler) CreateRoute(c *gin.Context) {
	var req routeWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	r := &domain.ModelRoute{
		ModelID: c.Param("id"), DeploymentID: req.DeploymentID,
		Priority: req.Priority, Weight: req.Weight,
		Capabilities: req.Capabilities, Enabled: req.Enabled,
	}
	if req.PoolStrategy != "" {
		r.PoolStrategy = domain.PoolStrategy(req.PoolStrategy)
	}
	if err := h.mgr.CreateRoute(c.Request.Context(), actorOf(c), r, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, toRouteDTO(r))
}

// UpdateRoute PATCH /routes/:route_id — 先读后写：校验与乐观锁需要完整
// 对象（model_id/deployment_id 不在请求体里，由既有路由行带出；Task 16
// 实测此前直接以空 model/deployment 提交，任何 PATCH 都 400）。
func (h *AdminModelsHandler) UpdateRoute(c *gin.Context) {
	var req routeWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	r, err := h.mgr.GetRoute(c.Request.Context(), c.Param("route_id"))
	if err != nil {
		fail(c, err)
		return
	}
	r.Priority, r.Weight = req.Priority, req.Weight
	r.Capabilities, r.Enabled = req.Capabilities, req.Enabled
	if req.PoolStrategy != "" {
		r.PoolStrategy = domain.PoolStrategy(req.PoolStrategy)
	}
	if req.UpdatedAt != nil {
		r.UpdatedAt = *req.UpdatedAt
	}
	if err := h.mgr.UpdateRoute(c.Request.Context(), actorOf(c), r, req.Reason); err != nil {
		fail(c, err)
		return
	}
	ok(c, toRouteDTO(r))
}

// DeleteRoute DELETE /routes/:route_id（可选 ?reason= 运营理由 → 审计）
func (h *AdminModelsHandler) DeleteRoute(c *gin.Context) {
	if err := h.mgr.DeleteRoute(c.Request.Context(), actorOf(c), c.Param("route_id"), reasonOf(c)); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"deleted": c.Param("route_id")})
}

// Publish POST /catalog/publish（可选 ?reason= 运营理由 → 审计）
func (h *AdminModelsHandler) Publish(c *gin.Context) {
	rev, err := h.mgr.Publish(c.Request.Context(), actorOf(c), reasonOf(c))
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"revision": rev})
}

// Rollback POST /catalog/rollback {"to_revision":N,"reason":"…"}
func (h *AdminModelsHandler) Rollback(c *gin.Context) {
	var req struct {
		ToRevision int    `json:"to_revision" binding:"required"`
		Reason     string `json:"reason"` // 可选运营理由 → 审计
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body: "+err.Error()))
		return
	}
	rev, err := h.mgr.Rollback(c.Request.Context(), actorOf(c), req.ToRevision, req.Reason)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"revision": rev})
}
