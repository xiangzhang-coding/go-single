package main

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// requestLogger 以 zap 结构化日志输出每请求访问日志。
func requestLogger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		log.Info("http_request",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
		)
	}
}

// safeRecovery 恢复 panic，但不转储请求行或请求头，避免 WS JWT 进入应用日志。
func safeRecovery(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() == nil {
				return
			}
			log.Error("panic recovered",
				zap.String("path", c.Request.URL.Path),
				zap.ByteString("stack", debug.Stack()),
			)
			if !c.Writer.Written() {
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
			c.Abort()
		}()
		c.Next()
	}
}

func configureTrustedProxies(r *gin.Engine, proxies []string, log *zap.Logger) {
	if err := r.SetTrustedProxies(proxies); err != nil {
		log.Error("设置可信反代白名单失败，已禁用代理头", zap.Error(err))
		_ = r.SetTrustedProxies(nil)
	}
}

// requestTimeout 全链路 context 超时（T20）：为请求派生带截止时间的 ctx 并
// 替换 c.Request.Context()，经 handler → service → 存储/MQ 逐层传递（各层调用
// 均以 ctx 为第一参数）；依赖超时时底层调用快速失败返回，handler 不会挂起。
// handler 返回后若已超时且尚未写响应，补写 504（快速失败的可观测出口）。
// 仅注册于 /api 业务路由组（/ws 长连接需自行管理生命周期，不适用请求超时）。
func requestTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)

		c.Next()

		// gin ResponseWriter 未写任何内容时 Size() 为 -1（已写为累计字节数）。
		if c.Writer.Size() < 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			c.AbortWithStatusJSON(http.StatusGatewayTimeout, gin.H{"error": "request timeout"})
		}
	}
}
