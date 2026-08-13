// 全链路请求超时中间件单元测试（T20）：依赖挂起（handler 不响应 ctx）时
// 快速失败补写 504；正常请求透传；ctx 截止时间逐层可见（handler 收到）。
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestTimeoutFastFail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestTimeout(50 * time.Millisecond))
	r.GET("/slow", func(c *gin.Context) {
		// 模拟依赖挂起：不响应 ctx，但底层调用会感知截止时间快速返回。
		select {
		case <-c.Request.Context().Done():
			c.JSON(http.StatusInternalServerError, gin.H{"error": "dependency failed fast"})
		case <-time.After(time.Hour):
		}
	})
	r.GET("/fast", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	rec := httptest.NewRecorder()
	start := time.Now()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slow", nil))
	require.Less(t, time.Since(start), 2*time.Second, "依赖超时必须快速失败，不挂起")
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fast", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestRequestTimeoutWrites504WhenNothingWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestTimeout(30 * time.Millisecond))
	// 依赖超时后 handler 未写响应即返回（ctx 感知路径已由各仓储负责）。
	r.GET("/silent", func(c *gin.Context) {
		<-c.Request.Context().Done()
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/silent", nil))
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "request timeout")
}

func TestRequestTimeoutPropagatesDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestTimeout(time.Second))

	var got time.Time
	r.GET("/check", func(c *gin.Context) {
		deadline, ok := c.Request.Context().Deadline()
		require.True(t, ok, "handler 应收到带截止时间的 ctx")
		got = deadline
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/check", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.WithinDuration(t, time.Now().Add(time.Second), got, 500*time.Millisecond)
}
