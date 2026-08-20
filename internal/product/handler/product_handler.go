// Package handler 暴露 product 模块的 HTTP 接口：admin 维护类目/商品/SKU，游客浏览。
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
	"github.com/xiangzhang-coding/go-single/internal/platform/pagination"
	"github.com/xiangzhang-coding/go-single/internal/product/service"
)

// Handler product 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册商品域路由。
//
// 游客浏览：
//
//	GET /api/categories                 类目列表
//	GET /api/products                   上架商品列表（category_id 筛选 + 分页）
//	GET /api/products/:id               商品详情（仅上架）
//
// admin 管理（Bearer + admin 角色）：
//
//	POST   /api/admin/categories         新建类目
//	PUT    /api/admin/categories/:id     编辑类目
//	DELETE /api/admin/categories/:id     删除类目（类目下无商品时）
//	POST   /api/admin/products           新建商品（默认下架）
//	GET    /api/admin/products           后台商品列表（status 筛选，含草稿/下架）
//	GET    /api/admin/products/:id       后台商品详情（含草稿/下架及全部 SKU）
//	PUT    /api/admin/products/:id       编辑商品
//	POST   /api/admin/products/:id/publish     上架
//	POST   /api/admin/products/:id/unpublish   下架
//	POST   /api/admin/products/:id/skus  新建 SKU
//	PUT    /api/admin/skus/:id           编辑 SKU（规格/价格/库存）
//	DELETE /api/admin/skus/:id           删除 SKU
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/categories", h.ListCategories)
	rg.GET("/products", h.ListProducts)
	rg.GET("/products/:id", h.GetDetail)

	admin := rg.Group("/admin", auth.Middleware(h.verifier), auth.RequireAdmin())
	admin.POST("/categories", h.CreateCategory)
	admin.PUT("/categories/:id", h.UpdateCategory)
	admin.DELETE("/categories/:id", h.DeleteCategory)

	admin.POST("/products", h.CreateProduct)
	admin.GET("/products", h.ListAdminProducts)
	admin.GET("/products/:id", h.GetAdminDetail)
	admin.PUT("/products/:id", h.UpdateProduct)
	admin.POST("/products/:id/publish", h.PublishProduct)
	admin.POST("/products/:id/unpublish", h.UnpublishProduct)
	admin.POST("/products/:id/skus", h.CreateSKU)
	admin.PUT("/skus/:id", h.UpdateSKU)
	admin.DELETE("/skus/:id", h.DeleteSKU)
}

type categoryRequest struct {
	Name string `json:"name" binding:"required"`
}

func (h *Handler) CreateCategory(c *gin.Context) {
	var req categoryRequest
	if !bindJSON(c, &req) {
		return
	}
	cat, err := h.svc.CreateCategory(c.Request.Context(), req.Name)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, cat)
}

func (h *Handler) UpdateCategory(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req categoryRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.svc.UpdateCategory(c.Request.Context(), id, req.Name); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteCategory(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteCategory(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type productRequest struct {
	CategoryID  int64  `json:"category_id" binding:"required"`
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
}

func (h *Handler) CreateProduct(c *gin.Context) {
	var req productRequest
	if !bindJSON(c, &req) {
		return
	}
	p, err := h.svc.CreateProduct(c.Request.Context(), req.CategoryID, req.Title, req.Description)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (h *Handler) UpdateProduct(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req productRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.svc.UpdateProduct(c.Request.Context(), id, req.CategoryID, req.Title, req.Description); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) PublishProduct(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.PublishProduct(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) UnpublishProduct(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.UnpublishProduct(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type skuRequest struct {
	Specs json.RawMessage `json:"specs" binding:"required"`
	Price int64           `json:"price"`
	Stock int             `json:"stock"`
}

func (h *Handler) CreateSKU(c *gin.Context) {
	productID, ok := idParam(c)
	if !ok {
		return
	}
	var req skuRequest
	if !bindJSON(c, &req) {
		return
	}
	sku, err := h.svc.CreateSKU(c.Request.Context(), productID, req.Specs, req.Price, req.Stock)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, sku)
}

func (h *Handler) UpdateSKU(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req skuRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.svc.UpdateSKU(c.Request.Context(), id, req.Specs, req.Price, req.Stock); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteSKU(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteSKU(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ListCategories(c *gin.Context) {
	list, err := h.svc.ListCategories(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

func (h *Handler) ListProducts(c *gin.Context) {
	p := pagination.FromQuery(c)

	var categoryID *int64
	if raw := c.Query("category_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			httpresponse.Write(c, http.StatusBadRequest, "invalid category_id")
			return
		}
		categoryID = &id
	}

	items, total, err := h.svc.ListProducts(c.Request.Context(), categoryID, p.Page, p.PageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

func (h *Handler) GetDetail(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	d, err := h.svc.GetDetail(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, d)
}

func (h *Handler) GetAdminDetail(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	detail, err := h.svc.GetAdminDetail(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// ListAdminProducts 后台商品列表：status 空 = 全部（含草稿/下架），
// 支持 category_id 筛选与分页；返回 {items, total} 与游客列表同构。
func (h *Handler) ListAdminProducts(c *gin.Context) {
	p := pagination.FromQuery(c)
	status := c.Query("status")

	var categoryID *int64
	if raw := c.Query("category_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			httpresponse.Write(c, http.StatusBadRequest, "invalid category_id")
			return
		}
		categoryID = &id
	}

	items, total, err := h.svc.ListAllProducts(c.Request.Context(), categoryID, status, p.Page, p.PageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

func bindJSON(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		httpresponse.Write(c, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpresponse.Write(c, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

// writeError 业务错误 → HTTP 状态码。
func writeError(c *gin.Context, err error) {
	httpresponse.WriteError(c, err,
		httpresponse.Rule{Status: http.StatusBadRequest, Errors: []error{service.ErrInvalidInput}},
		httpresponse.Rule{Status: http.StatusNotFound, Errors: []error{
			service.ErrProductNotFound, service.ErrSKUNotFound, service.ErrCategoryNotFound,
		}},
		httpresponse.Rule{Status: http.StatusConflict, Errors: []error{service.ErrCategoryInUse}},
	)
}
