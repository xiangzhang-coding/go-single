// Package handler 暴露 social 模块的 HTTP 接口：好友申请/好友列表与
// 好友圈动态（分享/时间线/删除）。
package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/social/service"
)

// Handler social 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	posts    service.PostService
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, posts service.PostService, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, posts: posts, verifier: verifier}
}

// RegisterRoutes 注册好友与动态路由（Bearer）。
//
//	POST /api/friend-requests              发起申请 {to_user_id}
//	GET  /api/friend-requests              我的申请（scope=incoming|outgoing，status 筛选）
//	POST /api/friend-requests/:id/accept   通过申请（仅被申请人）
//	POST /api/friend-requests/:id/reject   拒绝申请（仅被申请人）
//	GET  /api/friends                      我的好友列表（双向）
//	POST /api/posts                        分享动态 {sku_id, content?, image_url?}
//	GET  /api/posts/feed                   好友圈时间线（仅好友，page/page_size 分页）
//	GET  /api/posts/mine                   我的动态（page/page_size 分页）
//	DELETE /api/posts/:id                  删除自己的动态
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.POST("/friend-requests", h.SendRequest)
	protected.GET("/friend-requests", h.ListRequests)
	protected.POST("/friend-requests/:id/accept", h.Accept)
	protected.POST("/friend-requests/:id/reject", h.Reject)
	protected.GET("/friends", h.ListFriends)
	protected.POST("/posts", h.SharePost)
	protected.GET("/posts/feed", h.FeedPosts)
	protected.GET("/posts/mine", h.MyPosts)
	protected.DELETE("/posts/:id", h.DeletePost)
}

type sendRequestRequest struct {
	ToUserID int64 `json:"to_user_id" binding:"required"`
}

func (h *Handler) SendRequest(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req sendRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to_user_id is required"})
		return
	}
	r, err := h.svc.SendRequest(c.Request.Context(), claims.UserID, req.ToUserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, r)
}

func (h *Handler) ListRequests(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	scope := c.DefaultQuery("scope", service.ScopeIncoming)
	if scope != service.ScopeIncoming && scope != service.ScopeOutgoing {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scope"})
		return
	}
	status := c.Query("status")
	switch status {
	case "", "pending", "accepted", "rejected":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	items, err := h.svc.ListRequests(c.Request.Context(), claims.UserID, scope, status)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) Accept(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.Accept(c.Request.Context(), claims.UserID, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) Reject(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.svc.Reject(c.Request.Context(), claims.UserID, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ListFriends(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	items, err := h.svc.ListFriends(c.Request.Context(), claims.UserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return id, true
}

// writeError 好友与动态业务错误 → HTTP 状态码（两套错误共用单一映射，新增错误单点维护）。
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, service.ErrSelfRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrTargetUserNotFound), errors.Is(err, service.ErrRequestNotFound), errors.Is(err, service.ErrPostNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrRequestForbidden), errors.Is(err, service.ErrNotPurchased), errors.Is(err, service.ErrPostForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrAlreadyFriends), errors.Is(err, service.ErrDuplicateRequest), errors.Is(err, service.ErrRequestNotPending):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
