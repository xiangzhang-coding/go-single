// Package handler 暴露 chat 模块的 HTTP 接口（REST 通道）：
// 发送消息（可幂等重试）、会话列表、会话消息（游标分页）、已读推进。
package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/chat/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

// Handler chat 模块的 HTTP 处理器。
type Handler struct {
	svc      service.Service
	verifier auth.TokenVerifier
}

// New 构造处理器。
func New(svc service.Service, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册消息与会话路由（Bearer）。
//
//	POST /api/messages                      发送消息 {to_user_id, type, content?, url?, client_request_id?}
//	GET  /api/conversations                 我的会话列表（before_id 游标 + limit 分页，最近消息 + 未读数）
//	GET  /api/conversations/:key/messages   会话消息（after_id/before_id 游标 + limit 分页）
//	POST /api/conversations/:key/read       推进已读游标 {last_message_id}
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	protected := rg.Group("", auth.Middleware(h.verifier))
	protected.POST("/messages", h.Send)
	protected.GET("/conversations", h.ListConversations)
	protected.GET("/conversations/:key/messages", h.ListMessages)
	protected.POST("/conversations/:key/read", h.MarkRead)
}

type sendMessageRequest struct {
	ToUserID        int64  `json:"to_user_id" binding:"required"`
	Type            string `json:"type" binding:"required"`
	Content         string `json:"content"`
	URL             string `json:"url"`
	ClientRequestID string `json:"client_request_id"`
}

func (h *Handler) Send(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req sendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to_user_id and type are required"})
		return
	}
	result, err := h.svc.Send(c.Request.Context(), claims.UserID, service.SendParams{
		ToUserID:        req.ToUserID,
		Type:            req.Type,
		Content:         req.Content,
		URL:             req.URL,
		ClientRequestID: req.ClientRequestID,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusCreated
	if result.Idempotent {
		// 幂等重放：同 client_request_id 已有消息，返回原消息（同 id）。
		status = http.StatusOK
	}
	c.JSON(status, result.Message)
}

func (h *Handler) ListConversations(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	beforeID, _ := strconv.ParseInt(c.Query("before_id"), 10, 64)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	items, hasMore, err := h.svc.ListConversations(c.Request.Context(), claims.UserID, beforeID, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "has_more": hasMore})
}

func (h *Handler) ListMessages(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	afterID, _ := strconv.ParseInt(c.Query("after_id"), 10, 64)
	beforeID, _ := strconv.ParseInt(c.Query("before_id"), 10, 64)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	items, hasMore, err := h.svc.ListMessages(c.Request.Context(), claims.UserID,
		c.Param("key"), afterID, beforeID, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "has_more": hasMore})
}

type markReadRequest struct {
	LastMessageID int64 `json:"last_message_id" binding:"required"`
}

func (h *Handler) MarkRead(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req markReadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "last_message_id is required"})
		return
	}
	if err := h.svc.MarkRead(c.Request.Context(), claims.UserID, c.Param("key"), req.LastMessageID); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeError 消息业务错误 → HTTP 状态码。
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidInput), errors.Is(err, service.ErrSelfMessage):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrRecipientNotFound), errors.Is(err, service.ErrConversationNotFound), errors.Is(err, service.ErrMessageNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, service.ErrNotFriends), errors.Is(err, service.ErrConversationForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}
