package ws

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

// Handler 返回 /ws 握手 handler：token 经 query 参数传递（浏览器 WS API 无法自定义
// 请求头）；注意 token 会出现在访问日志/代理日志中，此为演示取舍（见 DESIGN）。
// 鉴权失败在升级前返回 401；成功后接管连接直至断开（阻塞）。
func (h *Hub) Handler(verifier auth.TokenVerifier) gin.HandlerFunc {
	upgrader := &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			if len(h.cfg.AllowOrigins) == 0 {
				// 演示取舍：允许所有 Origin（前端 VITE_WS_BASE 可跨源直连）。
				return true
			}
			origin := r.Header.Get("Origin")
			for _, allowed := range h.cfg.AllowOrigins {
				if origin == allowed {
					return true
				}
			}
			return false
		},
	}
	return func(c *gin.Context) {
		token := c.Query("token")
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		claims, err := verifier.Verify(c.Request.Context(), token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			// 升级失败（如非 GET / 非 websocket 协议）：响应已由 upgrader 写出。
			return
		}
		// 阻塞至连接关闭：请求日志/指标将该请求视为长连接，属预期。
		h.Handle(claims.UserID, conn)
	}
}
