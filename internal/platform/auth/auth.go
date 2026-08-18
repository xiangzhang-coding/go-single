// Package auth 提供认证相关的基础设施：TokenVerifier 轻量 seam（自签 JWT 实现，
// 未来可换 OIDC 等第三方实现）与 Gin 鉴权中间件。
package auth

import (
	"context"
	"errors"
	"time"
)

// ErrInvalidToken 表示 token 缺失、被篡改或已过期。
var ErrInvalidToken = errors.New("invalid token")

// Claims 是校验通过后携带的声明：用户 ID、角色与授权截止时间。
type Claims struct {
	UserID    int64
	Role      string
	ExpiresAt time.Time
}

// TokenVerifier 校验 token 并返回声明；成功结果必须携带未来的 ExpiresAt。
// 自签 JWT 实现见 jwt.go；OIDC/第三方实现可替换（进 backlog）。
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (*Claims, error)
}
