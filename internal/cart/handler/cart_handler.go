// Package handler 暴露 cart 模块的 HTTP 接口（Bearer 鉴权，全部操作限定本人购物车）。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/cart/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

// Handler cart 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册购物车路由（Bearer，所有操作作用于本人购物车）。
//
//	GET    /api/cart             我的购物车列表（含 SKU/商品快照）
//	POST   /api/cart             加购 {sku_id, quantity}（重复加购合并数量）
//	PUT    /api/cart/items/:id   修改数量 {quantity}
//	DELETE /api/cart/items/:id   删除条目
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.GET("/cart", h.List)
	protected.POST("/cart", h.AddItem)
	protected.PUT("/cart/items/:id", h.UpdateQuantity)
	protected.DELETE("/cart/items/:id", h.DeleteItem)
}

type addItemRequest struct {
	SKUID    int64 `json:"sku_id" binding:"required"`
	Quantity int   `json:"quantity"`
}

type updateQuantityRequest struct {
	Quantity int `json:"quantity" binding:"required"`
}

func (h *Handler) AddItem(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req addItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	item, err := h.svc.AddItem(c.Request.Context(), claims.UserID, req.SKUID, req.Quantity)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, item)
}

func (h *Handler) UpdateQuantity(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req updateQuantityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.svc.UpdateQuantity(c.Request.Context(), claims.UserID, id, req.Quantity); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) DeleteItem(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteItem(c.Request.Context(), claims.UserID, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	list, err := h.svc.ListItems(c.Request.Context(), claims.UserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

// writeError 购物车业务错误 → HTTP 状态码。
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "request timeout"})
	case errors.Is(err, service.ErrInvalidInput):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrSKUNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrSKUUnavailable):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrCartItemNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrCartItemForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
