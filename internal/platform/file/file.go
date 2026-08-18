// Package file 提供文件上传基础设施：MinIO 私有桶适配（platform 共享层，
// 直接依赖 minio-go，不做接口抽象——日志/配置等基础设施同 ADR-0003 原则）
// 与上传校验（类型白名单 + 大小上限）。
// 前端不直连 MinIO（presigned 直传明确不做），统一走后端代理 POST /api/files。
package file

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

const (
	// KindImage 为头像、动态配图和图片消息使用的图片对象。
	KindImage = "image"
	// KindFile 为聊天普通文件消息使用的对象。
	KindFile = "file"

	// MaxImageSize 图片上传大小上限：5 MiB。
	MaxImageSize = 5 << 20
	// MaxMessageFileSize 文件消息上传大小上限：20 MiB。
	MaxMessageFileSize = 20 << 20
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	// ErrInvalidType 类型不在白名单（png/jpeg/webp/gif）内。
	ErrInvalidType = errors.New("unsupported file type")
	// ErrInvalidKind 上传 kind 不是 image/file。
	ErrInvalidKind = errors.New("file kind must be image or file")
	// ErrTooLarge 超过对应媒体类型的大小上限。
	ErrTooLarge = errors.New("file too large")
)

// allowedTypes 类型白名单：检测到的 MIME → 对象扩展名。
var allowedImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

var allowedTextExtensions = map[string]string{
	".txt": "text/plain; charset=utf-8",
	".csv": "text/csv; charset=utf-8",
	".md":  "text/markdown; charset=utf-8",
}

// validateUpload 只信任内容魔数，并对普通文件额外校验扩展名。
// 图片允许 png/jpeg/webp/gif 且不超过 5 MiB；普通文件允许 PDF、ZIP 和
// txt/csv/md 文本且不超过 20 MiB。图片不能伪装成普通文件绕过图片策略。
func validateUpload(kind string, header []byte, size int64, filename string) (string, string, error) {
	if kind != KindImage && kind != KindFile {
		return "", "", ErrInvalidKind
	}
	limit := int64(MaxImageSize)
	if kind == KindFile {
		limit = MaxMessageFileSize
	}
	if size > limit {
		return "", "", ErrTooLarge
	}
	if size <= 0 || len(header) == 0 {
		return "", "", ErrInvalidType
	}

	detected := mimetype.Detect(header)
	mime := detected.String()
	if kind == KindImage {
		ext, ok := allowedImageTypes[mime]
		if !ok {
			return "", "", ErrInvalidType
		}
		return ext, mime, nil
	}

	ext := strings.ToLower(filepath.Ext(filename))
	switch {
	case ext == ".pdf" && detected.Is("application/pdf"):
		return "pdf", "application/pdf", nil
	case ext == ".zip" && detected.Is("application/zip"):
		return "zip", "application/zip", nil
	case allowedTextExtensions[ext] != "" && (detected.Is("text/plain") || detected.Is(strings.Split(allowedTextExtensions[ext], ";")[0])):
		return strings.TrimPrefix(ext, "."), allowedTextExtensions[ext], nil
	default:
		return "", "", ErrInvalidType
	}
}
