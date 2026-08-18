package ws

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
)

const authSubprotocol = "bearer"

// Handler 返回 /ws 握手 handler：浏览器通过 Sec-WebSocket-Protocol 携带 JWT，
// 避免凭据进入请求 URL、访问日志和代理错误日志。
// 鉴权失败在升级前返回 401；成功后接管连接直至断开（阻塞）。
func (h *Hub) Handler(verifier auth.TokenVerifier) gin.HandlerFunc {
	upgrader := &websocket.Upgrader{
		Subprotocols: []string{authSubprotocol},
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
		token := tokenFromSubprotocol(c.Request)
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		claims, err := verifier.Verify(c.Request.Context(), token)
		if err != nil || claims == nil || claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(time.Now()) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		sourceIP := c.ClientIP()
		release, scope, ok := h.reserve(claims.UserID, sourceIP)
		if !ok {
			if scope == rejectionScopeShutdown {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "websocket service unavailable"})
				return
			}
			h.log.Warn("WS 连接拒绝",
				zap.String("scope", string(scope)),
				zap.Int64("user_id", claims.UserID),
				zap.String("client_ip", sourceIP),
			)
			c.Header("Retry-After", "1")
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "websocket connection limit exceeded",
				"scope": scope,
			})
			return
		}
		defer release()

		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			// 升级失败（如非 GET / 非 websocket 协议）：响应已由 upgrader 写出。
			return
		}
		// 阻塞至连接关闭：请求日志/指标将该请求视为长连接，属预期。
		h.Handle(claims.UserID, sourceIP, claims.ExpiresAt, conn)
	}
}

func tokenFromSubprotocol(r *http.Request) string {
	protocols := websocket.Subprotocols(r)
	if len(protocols) != 2 || protocols[0] != authSubprotocol {
		return ""
	}
	return protocols[1]
}
