// Package handler 暴露 user 模块的 HTTP 接口：注册 / 登录 / 当前用户 / 用户查询。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

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
//	POST   /api/auth/register    注册
//	POST   /api/auth/login       登录
//	GET    /api/users/me         当前用户（受保护）
//	PATCH  /api/users/me         修改个人资料：昵称/头像（受保护，仅本人）
//	GET    /api/users            按用户名前缀搜索（受保护，"加好友"发现入口）
//	GET    /api/users/:id        指定用户（受保护，本人或 admin）
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/register", h.Register)
	rg.POST("/auth/login", h.Login)

	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.GET("/users/me", h.Me)
	protected.PATCH("/users/me", h.UpdateMe)
	protected.GET("/users", h.SearchUsers)
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

// updateProfileRequest 个人资料请求：指针区分"未提交"（不动）与"空串"（清空）。
type updateProfileRequest struct {
	Nickname  *string `json:"nickname"`
	AvatarURL *string `json:"avatar_url"`
}

// UpdateMe 修改当前用户个人资料（昵称/头像）；归属校验内建于 userID 取自令牌
// 而非请求体（防 IDOR）。头像先经 POST /api/files 上传取回 URL 再提交。
func (h *Handler) UpdateMe(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req updateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	u, err := h.svc.UpdateProfile(c.Request.Context(), claims.UserID, service.ProfileParams{
		Nickname:  req.Nickname,
		AvatarURL: req.AvatarURL,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidProfile):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, service.ErrUserNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusOK, u)
}

// SearchUsers 按用户名前缀搜索用户（"加好友"发现入口）；
// 排除自己（好友申请接口拒绝自加）；limit 非法值由服务层收敛为默认。
func (h *Handler) SearchUsers(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	users, err := h.svc.Search(c.Request.Context(), strings.TrimSpace(c.Query("username")), limit)
	if err != nil {
		if errors.Is(err, service.ErrInvalidUsername) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	items := make([]model.User, 0, len(users))
	for _, u := range users {
		if u.ID == claims.UserID {
			continue
		}
		items = append(items, *u)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
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
