// Package handler 暴露 flashsale 模块的 HTTP 接口：admin 秒杀活动管理
// （创建/编辑/列表/上架/下架）+ 用户抢购（限流 → 幂等键 → 原子预扣 →
// 持久生命周期 → MQ 异步落单 → 202 排队中 + pre_deduction_id 供前端轮询；
// 幂等重放已回退事实时返回 200 生命周期终态）。
package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
)

// Handler flashsale 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册秒杀活动路由。
//
// admin（Bearer + admin 角色）：
//
//	POST /api/admin/flashsales             创建活动
//	PUT  /api/admin/flashsales/:id         编辑活动
//	GET  /api/admin/flashsales             活动列表（全状态 + SKU/商品摘要 + 派生状态）
//	POST /api/admin/flashsales/:id/publish   上架（预热库存）
//	POST /api/admin/flashsales/:id/unpublish 下架
//
// 用户（Bearer）：
//
//	GET  /api/flashsales             秒杀页活动列表（进行中/即将开始，携带 server_time
//	                                  供前端对齐倒计时；状态/剩余库存服务端派生）
//	POST /api/flashsales/:id/purchase      抢购（在途返回 202；回退重放返回 200）
//	GET  /api/flashsales/purchases/:id     查询本人预扣生命周期
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup, seckillTokenBucket gin.HandlerFunc) {
	admin := rg.Group("/admin", auth.Middleware(h.verifier), auth.RequireAdmin())
	admin.POST("/flashsales", h.CreateActivity)
	admin.PUT("/flashsales/:id", h.UpdateActivity)
	admin.GET("/flashsales", h.ListActivities)
	admin.POST("/flashsales/:id/publish", h.PublishActivity)
	admin.POST("/flashsales/:id/unpublish", h.UnpublishActivity)

	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.GET("/flashsales", h.ListUserActivities)
	protected.POST("/flashsales/:id/purchase", seckillTokenBucket, h.Purchase)
	protected.GET("/flashsales/purchases/:id", h.GetPurchase)
}

type activityRequest struct {
	SKUID        int64     `json:"sku_id" binding:"required"`
	Title        string    `json:"title" binding:"required"`
	Price        int64     `json:"price" binding:"required"`
	Stock        int       `json:"stock" binding:"required"`
	PerUserLimit int       `json:"per_user_limit"`
	StartAt      time.Time `json:"start_at" binding:"required"`
	EndAt        time.Time `json:"end_at" binding:"required"`
}

type purchaseRequest struct {
	ClientRequestID string `json:"client_request_id" binding:"required"`
}

type purchaseLifecycleResponse struct {
	ID           string                   `json:"id"`
	Status       model.PreDeductionStatus `json:"status"`
	OrderNo      *string                  `json:"order_no,omitempty"`
	CreatedAt    time.Time                `json:"created_at"`
	UpdatedAt    time.Time                `json:"updated_at"`
	OrderedAt    *time.Time               `json:"ordered_at,omitempty"`
	RolledBackAt *time.Time               `json:"rolled_back_at,omitempty"`
}

func (h *Handler) CreateActivity(c *gin.Context) {
	var req activityRequest
	if !bindJSON(c, &req) {
		return
	}
	a, err := h.svc.CreateActivity(c.Request.Context(), activityParams(req))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, a)
}

func (h *Handler) UpdateActivity(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req activityRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.svc.UpdateActivity(c.Request.Context(), id, activityParams(req)); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ListActivities(c *gin.Context) {
	list, err := h.svc.ListActivities(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

// ListUserActivities 秒杀页活动列表：响应携带 server_time（RFC3339），
// 前端据此计算本地与服务端时钟偏移，倒计时与服务端对齐。
func (h *Handler) ListUserActivities(c *gin.Context) {
	list, err := h.svc.ListUserActivities(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"server_time": time.Now().Format(time.RFC3339), "items": list})
}

func (h *Handler) PublishActivity(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.PublishActivity(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) UnpublishActivity(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.UnpublishActivity(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Purchase 抢购：限流（全局令牌桶中间件 + 按用户 Redis 计数）→ 幂等键 →
// 缓存原子预扣 → 持久恢复生命周期 → MQ 异步落单；预扣成功立即返回
// 在途返回 202"排队中"与稳定预扣 ID；回退事实的幂等重放返回 200 终态。
func (h *Handler) Purchase(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
		return
	}
	var req purchaseRequest
	if !bindJSON(c, &req) {
		return
	}
	result, err := h.svc.Seckill(c.Request.Context(), claims.UserID, id, req.ClientRequestID)
	if err != nil {
		writeError(c, err)
		return
	}
	response := gin.H{
		"pre_deduction_id":     strconv.FormatInt(result.PreDeductionID, 10),
		"pre_deduction_status": result.Status,
		"status":               "queued",
		"order_no":             result.OrderNo,
		"message":              "排队中",
	}
	if result.Status == model.PreDeductionStatusRolledBack {
		response["status"] = model.PreDeductionStatusRolledBack
		response["message"] = "抢购已回退"
		c.JSON(http.StatusOK, response)
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func (h *Handler) GetPurchase(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
		return
	}
	pd, err := h.svc.GetPreDeduction(c.Request.Context(), claims.UserID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, purchaseLifecycleResponse{
		ID: strconv.FormatInt(pd.ID, 10), Status: pd.Status, OrderNo: pd.OrderNo,
		CreatedAt: pd.CreatedAt, UpdatedAt: pd.UpdatedAt,
		OrderedAt: pd.OrderedAt, RolledBackAt: pd.RolledBackAt,
	})
}

func activityParams(req activityRequest) service.ActivityParams {
	perUserLimit := req.PerUserLimit
	if perUserLimit == 0 {
		perUserLimit = 1
	}
	return service.ActivityParams{
		SKUID:        req.SKUID,
		Title:        req.Title,
		Price:        req.Price,
		Stock:        req.Stock,
		PerUserLimit: perUserLimit,
		StartAt:      req.StartAt,
		EndAt:        req.EndAt,
	}
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
		httpresponse.Rule{Status: http.StatusNotFound, Errors: []error{service.ErrActivityNotFound, service.ErrPreDeductionNotFound}},
		httpresponse.Rule{Status: http.StatusConflict, Errors: []error{
			service.ErrStockIncreaseInProgress, service.ErrActivityFieldsLocked, service.ErrActivityEnded,
			service.ErrStockBelowAcceptedReservations, service.ErrReservationsUnsettled,
			service.ErrNotInWindow, service.ErrSoldOut, service.ErrLimitReached, service.ErrOffline,
		}},
		httpresponse.Rule{Status: http.StatusTooManyRequests, Errors: []error{service.ErrRateLimited}},
		httpresponse.Rule{Status: http.StatusServiceUnavailable, Errors: []error{service.ErrRecoveryIncomplete}},
	)
}
