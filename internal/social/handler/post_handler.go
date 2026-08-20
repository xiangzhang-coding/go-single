// 好友圈动态 HTTP 接口：分享（购买校验）、时间线（仅好友可见）、删除自己的动态。
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
	"github.com/xiangzhang-coding/go-single/internal/platform/pagination"
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
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
		return
	}
	var req sharePostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpresponse.Write(c, http.StatusBadRequest, "sku_id is required")
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

// MyPosts 我的动态：时间倒序分页（feed 不含自己，个人页单独展示）。
func (h *Handler) MyPosts(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
		return
	}
	p := pagination.FromQuery(c)
	items, total, err := h.posts.MyPosts(c.Request.Context(), claims.UserID, p.Page, p.PageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// FeedPosts 好友圈时间线：仅好友动态，时间倒序分页。
func (h *Handler) FeedPosts(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
		return
	}
	p := pagination.FromQuery(c)
	items, total, err := h.posts.Feed(c.Request.Context(), claims.UserID, p.Page, p.PageSize)
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
		httpresponse.Write(c, http.StatusUnauthorized, "missing token")
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
