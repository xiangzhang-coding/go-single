package auth

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWTConfig 自签 JWT 的配置：签名密钥与有效期。
type JWTConfig struct {
	Secret string
	TTL    time.Duration
}

// JWT 是 TokenVerifier 的自签实现（HS256）。
type JWT struct {
	secret []byte
	ttl    time.Duration
}

type jwtClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// NewJWT 构造自签 JWT 实现。
func NewJWT(cfg JWTConfig) *JWT {
	return &JWT{secret: []byte(cfg.Secret), ttl: cfg.TTL}
}

// Issue 为用户签发 HS256 令牌（登录成功时调用）。
func (j *JWT) Issue(userID int64, role string) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(j.ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// Verify 校验令牌并提取用户 ID 与角色；非法或过期令牌返回 ErrInvalidToken。
func (j *JWT) Verify(_ context.Context, token string) (*Claims, error) {
	var claims jwtClaims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method %s", t.Method.Alg())
		}
		return j.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid || claims.ExpiresAt == nil {
		return nil, ErrInvalidToken
	}

	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &Claims{
		UserID:    userID,
		Role:      claims.Role,
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}

var _ TokenVerifier = (*JWT)(nil)
