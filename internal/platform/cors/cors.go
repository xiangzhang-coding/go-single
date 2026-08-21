// Package cors 提供 Gin CORS 中间件（T26 部署双路径的跨源场景）：
// 云端前端（Cloudflare Pages 等）与后端不同源，浏览器预检/响应需携带
// Access-Control-* 头；白名单经配置（cors.allow_origins）注入。
package cors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Middleware 返回按 Origin 白名单放行的 CORS 中间件：
//   - 无 Origin 头（同源/非浏览器）不处理，直接放行；
//   - Origin 不在白名单：预检请求直接 403，普通请求放行但不带 CORS 头（浏览器拦截）；
//   - 空白名单 = 允许所有 Origin（演示取舍，与 ws.allow_origins 语义一致）。
//
// 认证走 Authorization 头（非 cookie），无需 Allow-Credentials。
func Middleware(allowOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowOrigins))
	for _, o := range allowOrigins {
		allowed[o] = struct{}{}
	}
	allowAll := len(allowed) == 0

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}

		if !allowAll {
			if _, ok := allowed[origin]; !ok {
				if c.Request.Method == http.MethodOptions {
					c.AbortWithStatus(http.StatusForbidden)
					return
				}
				c.Next()
				return
			}
		}

		header := c.Writer.Header()
		header.Set("Access-Control-Allow-Origin", origin)
		header.Add("Vary", "Origin")
		header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
		header.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Length")
		header.Set("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
