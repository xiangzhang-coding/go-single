// Package handler 的说明见 file.go；本文件提供文件上传的 HTTP 适配：
// POST /api/files（multipart 字段 "file"），Bearer 鉴权，代理转存 MinIO 返回 URL。
package file

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

// Handler 文件上传的 HTTP 处理器。
type Handler struct {
	svc      *MinIO
	verifier auth.TokenVerifier
}

// NewHandler 构造处理器。
func NewHandler(svc *MinIO, verifier auth.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// RegisterRoutes 注册文件上传路由（Bearer）。
//
//	POST /api/files  上传图片（multipart 字段 "file"），返回 {"url": "..."}
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/files", auth.Middleware(h.verifier), h.Upload)
}

// Upload 接收 multipart 文件并代理上传：类型白名单 + ≤5MB，成功返回可引用 URL。
func (h *Handler) Upload(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": `file is required (multipart field "file")`})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	defer f.Close()

	url, err := h.svc.Upload(c.Request.Context(), f, fh.Size)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidType), errors.Is(err, ErrTooLarge):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"url": url})
}
