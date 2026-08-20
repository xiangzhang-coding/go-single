// Package handler 暴露 payment 模块的 HTTP 接口：模拟支付回调。
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/payment/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
)

// Handler payment 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册支付路由。
//
// 用户（Bearer）：
//
//	POST /api/payments/mock  模拟支付回调（成功驱动订单 待支付→已支付）
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.POST("/payments/mock", h.MockPay)
}

type mockPayRequest struct {
	OrderNo   string `json:"order_id" binding:"required"`
	PaymentID string `json:"payment_id" binding:"required"`
	Amount    *int64 `json:"amount"`
	Result    string `json:"result" binding:"required"`
}

func (h *Handler) MockPay(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
		return
	}
	var req mockPayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpresponse.Write(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Amount == nil {
		httpresponse.Write(c, http.StatusBadRequest, "invalid request body: amount required")
		return
	}
	payment, err := h.svc.MockPay(c.Request.Context(), claims.UserID, service.PayParams{
		OrderNo:   req.OrderNo,
		PaymentID: req.PaymentID,
		Amount:    *req.Amount,
		Result:    req.Result,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, payment)
}

// writeError 支付业务错误 → HTTP 状态码。
func writeError(c *gin.Context, err error) {
	httpresponse.WriteError(c, err,
		httpresponse.Rule{Status: http.StatusBadRequest, Errors: []error{service.ErrInvalidInput}},
		httpresponse.Rule{Status: http.StatusForbidden, Errors: []error{service.ErrOrderForbidden}},
		httpresponse.Rule{Status: http.StatusNotFound, Errors: []error{service.ErrOrderNotFound}},
		httpresponse.Rule{Status: http.StatusConflict, Errors: []error{
			service.ErrPaymentDuplicate, service.ErrAmountMismatch, service.ErrIllegalTransition, service.ErrOrderChanged,
		}},
	)
}
