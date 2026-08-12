// Package handler 暴露 coupon 模块的 HTTP 接口：admin 发布/编辑券模板，
// 用户浏览可领券、领取与查看我的券。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/coupon/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

// Handler coupon 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册优惠券路由。
//
// 用户（Bearer）：
//
//	GET  /api/coupons             可领券列表（含当前用户视角状态）
//	POST /api/coupons/:id/claim   领取
//	GET  /api/coupons/mine        我的券（status 筛选 + 分页）
//
// admin（Bearer + admin 角色）：
//
//	POST /api/admin/coupons       发布券模板
//	PUT  /api/admin/coupons/:id   编辑券模板
//	GET  /api/admin/coupons       模板列表（含已领数）
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.GET("/coupons", h.ListClaimable)
	protected.POST("/coupons/:id/claim", h.Claim)
	protected.GET("/coupons/mine", h.ListMine)

	admin := rg.Group("/admin", auth.Middleware(h.verifier), auth.RequireAdmin())
	admin.POST("/coupons", h.CreateTemplate)
	admin.PUT("/coupons/:id", h.UpdateTemplate)
	admin.GET("/coupons", h.ListTemplates)
}

type templateRequest struct {
	Name         string    `json:"name" binding:"required"`
	Type         string    `json:"type" binding:"required"`
	Value        int64     `json:"value" binding:"required"`
	MinAmount    int64     `json:"min_amount"`
	Total        int       `json:"total" binding:"required"`
	PerUserLimit int       `json:"per_user_limit"`
	ValidFrom    time.Time `json:"valid_from" binding:"required"`
	ValidUntil   time.Time `json:"valid_until" binding:"required"`
}

func (h *Handler) CreateTemplate(c *gin.Context) {
	var req templateRequest
	if !bindJSON(c, &req) {
		return
	}
	params := templateParams(req)
	if req.PerUserLimit == 0 {
		params.PerUserLimit = 1
	}
	t, err := h.svc.CreateTemplate(c.Request.Context(), params)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, t)
}

func (h *Handler) UpdateTemplate(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req templateRequest
	if !bindJSON(c, &req) {
		return
	}
	params := templateParams(req)
	if req.PerUserLimit == 0 {
		params.PerUserLimit = 1
	}
	if err := h.svc.UpdateTemplate(c.Request.Context(), id, params); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ListTemplates(c *gin.Context) {
	list, err := h.svc.ListTemplates(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

func (h *Handler) ListClaimable(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	list, err := h.svc.ListClaimable(c.Request.Context(), claims.UserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

func (h *Handler) Claim(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	uc, err := h.svc.Claim(c.Request.Context(), claims.UserID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, uc)
}

func (h *Handler) ListMine(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	status := c.Query("status")
	switch status {
	case "", "unused", "used", "expired":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	items, total, err := h.svc.ListMine(c.Request.Context(), claims.UserID, status, page, pageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

func templateParams(req templateRequest) service.TemplateParams {
	return service.TemplateParams{
		Name:         req.Name,
		Type:         req.Type,
		Value:        req.Value,
		MinAmount:    req.MinAmount,
		Total:        req.Total,
		PerUserLimit: req.PerUserLimit,
		ValidFrom:    req.ValidFrom,
		ValidUntil:   req.ValidUntil,
	}
}

func bindJSON(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return false
	}
	return true
}

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

// writeError 业务错误 → HTTP 状态码。
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrTemplateNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrSoldOut), errors.Is(err, service.ErrClaimLimitReached), errors.Is(err, service.ErrNotInWindow):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
