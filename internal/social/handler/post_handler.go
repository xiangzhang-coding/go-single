// 好友圈动态 HTTP 接口：分享（购买校验）、时间线（仅好友可见）、删除自己的动态。
package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/social/service"
)

type sharePostRequest struct {
	SKUID    int64  `json:"sku_id" binding:"required"`
	Content  string `json:"content"`
	ImageURL string `json:"image_url"`
}

// SharePost 分享动态：引用已购 SKU + 可选文案 + 可选图片；未购买 403。
func (h *Handler) SharePost(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	var req sharePostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sku_id is required"})
		return
	}
	post, err := h.posts.Share(c.Request.Context(), claims.UserID, service.ShareParams{
		SKUID:    req.SKUID,
		Content:  req.Content,
		ImageURL: req.ImageURL,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, post)
}

// FeedPosts 好友圈时间线：仅好友动态，时间倒序分页。
func (h *Handler) FeedPosts(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	items, total, err := h.posts.Feed(c.Request.Context(), claims.UserID, page, pageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// DeletePost 删除自己的动态（owner 校验）。
func (h *Handler) DeletePost(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.posts.Delete(c.Request.Context(), claims.UserID, id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
