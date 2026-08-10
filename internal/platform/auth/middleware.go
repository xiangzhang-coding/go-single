package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// claimsKey 为 gin 上下文中当前用户声明的键。
const claimsKey = "auth.claims"

// ClaimsFrom 从请求上下文取出 Middleware 写入的用户声明。
func ClaimsFrom(c *gin.Context) (*Claims, bool) {
	v, ok := c.Get(claimsKey)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*Claims)
	return claims, ok
}

// Middleware 解析 Authorization: Bearer <token> 并校验；失败返回 401。
// 校验通过后把 Claims 写入上下文。
func Middleware(verifier TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c)
		if !ok {
			abortUnauthorized(c, "missing token")
			return
		}
		claims, err := verifier.Verify(c.Request.Context(), token)
		if err != nil {
			abortUnauthorized(c, "invalid or expired token")
			return
		}
		c.Set(claimsKey, claims)
		c.Next()
	}
}

// RequireAdmin 需在 Middleware 之后使用；非 admin 角色返回 403。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := ClaimsFrom(c)
		if !ok {
			abortUnauthorized(c, "missing token")
			return
		}
		if claims.Role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin required"})
			return
		}
		c.Next()
	}
}

func bearerToken(c *gin.Context) (string, bool) {
	h := c.GetHeader("Authorization")
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
}
