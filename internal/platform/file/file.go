// Package file 提供文件上传基础设施：MinIO 私有桶适配（platform 共享层，
// 直接依赖 minio-go，不做接口抽象——日志/配置等基础设施同 ADR-0003 原则）
// 与上传校验（类型白名单 + 大小上限）。
// 前端不直连 MinIO（presigned 直传明确不做），统一走后端代理 POST /api/files。
package file

import (
	"errors"

	"github.com/gabriel-vasile/mimetype"
)

// MaxFileSize 上传大小上限：5MB。
const MaxFileSize = 5 << 20

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	// ErrInvalidType 类型不在白名单（png/jpeg/webp/gif）内。
	ErrInvalidType = errors.New("unsupported file type, allowed: png/jpeg/webp/gif")
	// ErrTooLarge 超过 5MB 大小上限。
	ErrTooLarge = errors.New("file too large, max 5MB")
)

// allowedTypes 类型白名单：检测到的 MIME → 对象扩展名。
var allowedTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
	"image/gif":  "gif",
}

// validate 校验类型白名单与大小上限，返回对象扩展名与规范化 MIME。
// header 为内容头部字节（供魔数嗅探），不信任客户端声明。
func validate(header []byte, size int64) (string, string, error) {
	if size > MaxFileSize {
		return "", "", ErrTooLarge
	}
	mime := mimetype.Detect(header).String()
	ext, ok := allowedTypes[mime]
	if !ok {
		return "", "", ErrInvalidType
	}
	return ext, mime, nil
}
