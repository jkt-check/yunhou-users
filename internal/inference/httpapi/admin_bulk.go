// admin_bulk.go — 运营批量导入端点（Task 15，设计 §9.2 /admin/models 族的
// "批量导入"能力；models:manage 权限，router 装配）。
//
//   POST /admin/catalog/bulk-import
//     body: {"task_id": "...", "dry_run": bool, "providers": [...], "models": [...]}
//
// 语义（management/bulk_import.go 头部注释是权威）：
//   - dry_run=true → 逐项预演（would_insert/would_skip/error），一行不写；
//     部分错误的预览只存在于 dry-run。
//   - dry_run=false → 全部有效才落库（任一 error → 400 + 逐项错误，一行
//     不写——绝不半发布）；task_id 幂等（同文档重复提交 200 + replayed=true，
//     不重复创建；异文档同 task_id → 409，M-4）。
//   - 落库即草稿；发布走既有 POST /admin/catalog/publish。

package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yunhou/users/internal/inference/domain"
	"github.com/yunhou/users/internal/inference/management"
)

// AdminBulkHandler exposes the catalog bulk-import endpoint.
type AdminBulkHandler struct {
	svc *management.BulkImportService
}

// NewAdminBulkHandler builds the handler.
func NewAdminBulkHandler(svc *management.BulkImportService) *AdminBulkHandler {
	return &AdminBulkHandler{svc: svc}
}

// Register mounts the endpoint; the caller wraps the group with the
// models:manage authorization middleware.
func (h *AdminBulkHandler) Register(g *gin.RouterGroup) {
	g.POST("/catalog/bulk-import", h.Import)
}

type bulkImportRequest struct {
	TaskID   string                  `json:"task_id" binding:"required"`
	DryRun   bool                    `json:"dry_run"`
	Providers []management.BulkProvider `json:"providers"`
	Models    []management.BulkModel    `json:"models"`
}

// Import handles POST /catalog/bulk-import. 逐项错误在 data.items 里（
// dry-run 200 / commit 400），不藏在 message 字符串里。
func (h *AdminBulkHandler) Import(c *gin.Context) {
	var req bulkImportRequest
	if err := strictBindJSON(c, &req); err != nil {
		fail(c, domain.NewError(domain.CodeInvalidInput, "invalid request body (unknown fields rejected): "+err.Error()))
		return
	}
	doc := &management.BulkCatalog{Providers: req.Providers, Models: req.Models}
	res, err := h.svc.Import(c.Request.Context(), actorOf(c), req.TaskID, doc, req.DryRun)
	if err != nil {
		fail(c, err)
		return
	}
	switch {
	case res.HasErrors():
		// commit 与 dry-run 同一形状：逐项错误；commit 一行未写。
		status := http.StatusOK
		if !res.DryRun {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"code": status, "data": res, "message": "bulk import has item errors"})
	case res.Replayed:
		ok(c, res) // 幂等重放：已记录的结果原样返回
	case res.Committed:
		c.JSON(http.StatusCreated, gin.H{"code": 0, "data": res})
	default:
		ok(c, res) // dry-run 全有效
	}
}
