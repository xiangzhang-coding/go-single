package file

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

// AccessAuthorizer 判定非上传者能否读取已绑定到业务对象的媒体引用。
type AccessAuthorizer interface {
	CanRead(ctx context.Context, userID int64, reference string) (bool, error)
}

type Handler struct {
	svc        *MinIO
	verifier   auth.TokenVerifier
	authorizer AccessAuthorizer
}

// NewHandler 构造处理器。authorizer 为 nil 时仅上传者本人可读取。
func NewHandler(svc *MinIO, verifier auth.TokenVerifier, authorizer AccessAuthorizer) *Handler {
	return &Handler{svc: svc, verifier: verifier, authorizer: authorizer}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/files", auth.Middleware(h.verifier), h.Upload)
	rg.GET("/files/:reference", auth.Middleware(h.verifier), h.Read)
}

func (h *Handler) Upload(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	if c.Request.ContentLength > MaxMultipartBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "multipart body too large"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxMultipartBodySize)
	err := c.Request.ParseMultipartForm(MultipartMemorySize)
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "multipart body too large"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body"})
		return
	}
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

	kind := c.PostForm("kind")
	if kind == "" {
		kind = KindImage
	}
	info, err := h.svc.Upload(c.Request.Context(), claims.UserID, kind, f, fh.Size, fh.Filename)
	if err != nil {
		switch {
		case errors.Is(err, ErrTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
		case errors.Is(err, ErrQuotaExceeded):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, ErrInvalidType), errors.Is(err, ErrInvalidKind):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"url": info.Reference, "kind": info.Kind, "filename": info.Filename,
		"content_type": info.ContentType, "size": info.Size,
	})
}

// Read 经 Bearer 鉴权从私有桶代理对象。上传者始终可读，其他用户必须由
// 头像、好友圈或聊天业务授权；客户端永远不会拿到 MinIO 地址。
func (h *Handler) Read(c *gin.Context) {
	claims, ok := auth.ClaimsFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	reference := referencePrefix + c.Param("reference")
	object, err := h.svc.Open(c.Request.Context(), reference)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidReference), errors.Is(err, ErrObjectNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	defer object.Close()

	allowed := object.OwnerID == claims.UserID
	if !allowed && h.authorizer != nil {
		allowed, err = h.authorizer.CanRead(c.Request.Context(), claims.UserID, reference)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "file access forbidden"})
		return
	}

	disposition := "attachment"
	if object.Kind == KindImage {
		disposition = "inline"
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": object.Filename}))
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, object.Size, object.ContentType, object, nil)
}
