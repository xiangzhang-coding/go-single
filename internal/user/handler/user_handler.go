// Package handler 暴露 user 模块的 HTTP 接口：注册 / 登录 / 当前用户 / 用户查询。
package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/user/model"
	"github.com/xiangzhang-coding/go-single/internal/user/service"
)

// Handler user 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册用户与认证路由。
//
//	POST /api/auth/register    注册
//	POST /api/auth/login       登录
//	GET  /api/users/me         当前用户（受保护）
//	GET  /api/users/:id        指定用户（受保护，本人或 admin）
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/register", h.Register)
	rg.POST("/auth/login", h.Login)

	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.GET("/users/me", h.Me)
	protected.GET("/users/:id", h.GetUser)
}

type credentialsRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (h *Handler) Register(c *gin.Context) {
	var req credentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
		return
	}

	u, err := h.svc.Register(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUsernameTaken):
			c.JSON(http.StatusConflict, gin.H{"error": "username already taken"})
		case errors.Is(err, service.ErrInvalidUsername), errors.Is(err, service.ErrInvalidPassword):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusCreated, u)
}

func (h *Handler) Login(c *gin.Context) {
	var req credentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username and password are required"})
		return
	}

	u, token, err := h.svc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user": u})
}

// Me 返回当前登录用户（对象级授权：仅本人）。
func (h *Handler) Me(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	u, err := h.svc.GetByID(c.Request.Context(), claims.UserID)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, u)
}

// GetUser 查询指定用户；仅本人或 admin 可见（防 IDOR）。
func (h *Handler) GetUser(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	if claims.UserID != id && claims.Role != model.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	u, err := h.svc.GetByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, u)
}
