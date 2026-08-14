// Package handler 暴露 order 模块的 HTTP 接口：用户下单（购物车结算/直购）、
// 订单列表与详情、取消、确认收货；admin 发货（role 鉴权）。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/pagination"
)

// Handler order 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册订单路由。
//
// 用户（Bearer）：
//
//	POST   /api/orders                    下单（from_cart 结算 / items 直购）
//	GET    /api/orders                    我的订单（status 筛选 + 分页）
//	GET    /api/orders/:order_no          订单详情（owner 校验）
//	POST   /api/orders/:order_no/cancel   取消待支付订单
//	POST   /api/orders/:order_no/confirm  确认收货
//
// admin（Bearer + admin 角色）：
//
//	GET    /api/admin/orders                   后台订单列表（全量，status 筛选 + 分页）
//	POST   /api/admin/orders/:order_no/ship    后台发货（已支付 → 已发货）
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.POST("/orders", h.Create)
	protected.GET("/orders", h.List)
	protected.GET("/orders/:order_no", h.GetDetail)
	protected.POST("/orders/:order_no/cancel", h.Cancel)
	protected.POST("/orders/:order_no/confirm", h.ConfirmReceipt)

	admin := rg.Group("/admin", auth.Middleware(h.verifier), auth.RequireAdmin())
	admin.GET("/orders", h.ListAll)
	admin.POST("/orders/:order_no/ship", h.Ship)
}

type createOrderRequest struct {
	ClientRequestID string    `json:"client_request_id" binding:"required"`
	AddressID       int64     `json:"address_id" binding:"required"`
	CouponID        int64     `json:"coupon_id"`
	FromCart        bool      `json:"from_cart"`
	Items           []itemReq `json:"items"`
}

type itemReq struct {
	SKUID    int64 `json:"sku_id"`
	Quantity int   `json:"quantity"`
}

func (h *Handler) Create(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	params := service.CreateParams{
		ClientRequestID: req.ClientRequestID,
		AddressID:       req.AddressID,
		CouponID:        req.CouponID,
		FromCart:        req.FromCart,
	}
	for _, it := range req.Items {
		params.Items = append(params.Items, service.ItemParams{SKUID: it.SKUID, Quantity: it.Quantity})
	}

	result, err := h.svc.Create(c.Request.Context(), claims.UserID, params)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusCreated
	if result.Processing {
		// 幂等键已占用但订单尚未提交：不是创建成功，客户端应轮询详情。
		status = http.StatusAccepted
	} else if result.Idempotent {
		status = http.StatusOK
	}
	c.JSON(status, result.Order)
}

func (h *Handler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	p := pagination.FromQuery(c)
	list, total, err := h.svc.List(c.Request.Context(), claims.UserID, c.Query("status"), p.Page, p.PageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": list, "total": total})
}

func (h *Handler) GetDetail(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	orderNo, ok2 := orderNoParam(c)
	if !ok2 {
		return
	}
	view, err := h.svc.GetDetail(c.Request.Context(), claims.UserID, orderNo)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) Cancel(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	orderNo, ok2 := orderNoParam(c)
	if !ok2 {
		return
	}
	if err := h.svc.Cancel(c.Request.Context(), claims.UserID, orderNo); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ConfirmReceipt(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	orderNo, ok2 := orderNoParam(c)
	if !ok2 {
		return
	}
	if err := h.svc.ConfirmReceipt(c.Request.Context(), claims.UserID, orderNo); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// ListAll 后台订单列表（T25）：全量订单（跨用户），status 筛选 + 分页，
// 响应 {orders, total} 与用户订单列表同构；仅供 admin（路由组鉴权）。
func (h *Handler) ListAll(c *gin.Context) {
	p := pagination.FromQuery(c)
	list, total, err := h.svc.ListAll(c.Request.Context(), c.Query("status"), p.Page, p.PageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": list, "total": total})
}

func (h *Handler) Ship(c *gin.Context) {
	orderNo, ok := orderNoParam(c)
	if !ok {
		return
	}
	if err := h.svc.Ship(c.Request.Context(), orderNo); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// orderNoParam 校验路径参数为纯数字订单号（雪花 ID 十进制）。
func orderNoParam(c *gin.Context) (string, bool) {
	raw := c.Param("order_no")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || len(raw) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order_no"})
		return "", false
	}
	return raw, true
}

// writeError 订单业务错误 → HTTP 状态码。
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "request timeout"})
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, service.ErrCartEmpty):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrOrderNotFound), errors.Is(err, service.ErrSKUNotFound),
		errors.Is(err, service.ErrCouponNotFound), errors.Is(err, service.ErrAddressNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrOrderForbidden), errors.Is(err, service.ErrAddressForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrInsufficientStock), errors.Is(err, service.ErrSKUUnavailable),
		errors.Is(err, service.ErrCouponUsed), errors.Is(err, service.ErrCouponExpired),
		errors.Is(err, service.ErrCouponThresholdNotMet), errors.Is(err, service.ErrIllegalTransition),
		errors.Is(err, service.ErrOrderChanged):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
