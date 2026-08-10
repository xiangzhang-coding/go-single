package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/user/service"
)

// AddressHandler 地址簿的 HTTP 处理器。
type AddressHandler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// NewAddress 构造地址簿处理器。
func NewAddress(svc service.Service, verifier auth.TokenVerifier) *AddressHandler {
	return &AddressHandler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册地址簿路由（Bearer）。
//
//	GET    /api/addresses             我的地址列表（默认地址排最前）
//	POST   /api/addresses             新增地址（首条自动为默认；is_default=true 显式设默认）
//	PUT    /api/addresses/:id         编辑地址
//	DELETE /api/addresses/:id         删除地址
//	PUT    /api/addresses/:id/default 设为默认
func (h *AddressHandler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.GET("/addresses", h.List)
	protected.POST("/addresses", h.Create)
	protected.PUT("/addresses/:id", h.Update)
	protected.DELETE("/addresses/:id", h.Delete)
	protected.PUT("/addresses/:id/default", h.SetDefault)
}

type addressRequest struct {
	Receiver  string `json:"receiver" binding:"required"`
	Phone     string `json:"phone" binding:"required"`
	Province  string `json:"province" binding:"required"`
	City      string `json:"city" binding:"required"`
	District  string `json:"district" binding:"required"`
	Detail    string `json:"detail" binding:"required"`
	IsDefault bool   `json:"is_default"`
}

// addressUpdateRequest 编辑请求：不含 is_default（默认指向只经
// PUT /api/addresses/:id/default 切换，避免静默忽略的字段）。
type addressUpdateRequest struct {
	Receiver string `json:"receiver" binding:"required"`
	Phone    string `json:"phone" binding:"required"`
	Province string `json:"province" binding:"required"`
	City     string `json:"city" binding:"required"`
	District string `json:"district" binding:"required"`
	Detail   string `json:"detail" binding:"required"`
}

func (h *AddressHandler) Create(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req addressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	a, err := h.svc.CreateAddress(c.Request.Context(), claims.UserID, addressParams(req))
	if err != nil {
		writeAddressError(c, err)
		return
	}
	c.JSON(http.StatusCreated, a)
}

func (h *AddressHandler) Update(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	var req addressUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.svc.UpdateAddress(c.Request.Context(), claims.UserID, id, updateAddressParams(req)); err != nil {
		writeAddressError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *AddressHandler) Delete(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteAddress(c.Request.Context(), claims.UserID, id); err != nil {
		writeAddressError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *AddressHandler) List(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	list, err := h.svc.ListAddresses(c.Request.Context(), claims.UserID)
	if err != nil {
		writeAddressError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": list})
}

func (h *AddressHandler) SetDefault(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.SetDefaultAddress(c.Request.Context(), claims.UserID, id); err != nil {
		writeAddressError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func addressParams(req addressRequest) service.AddressParams {
	return service.AddressParams{
		Receiver:  req.Receiver,
		Phone:     req.Phone,
		Province:  req.Province,
		City:      req.City,
		District:  req.District,
		Detail:    req.Detail,
		IsDefault: req.IsDefault,
	}
}

func updateAddressParams(req addressUpdateRequest) service.AddressParams {
	return service.AddressParams{
		Receiver: req.Receiver,
		Phone:    req.Phone,
		Province: req.Province,
		City:     req.City,
		District: req.District,
		Detail:   req.Detail,
	}
}

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

// writeAddressError 地址簿业务错误 → HTTP 状态码。
func writeAddressError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidAddress):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrAddressNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrAddressForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
