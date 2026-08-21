// 全链路请求超时中间件单元测试（T20）：依赖挂起（handler 不响应 ctx）时
// 快速失败补写 504；正常请求透传；ctx 截止时间逐层可见（handler 收到）。
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	"github.com/xiangzhang-coding/go-single/internal/platform/httpresponse"
)

func testLogger() (*zap.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(&buf),
		zap.DebugLevel,
	)
	return zap.New(core), &buf
}

func TestRequestLogsDoNotContainWebSocketCredentials(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiI0MiJ9.sensitive-signature"
	log, output := testLogger()
	r := gin.New()
	r.Use(requestLogger(log))
	r.GET("/ws", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/ws?token="+token, nil)
	req.Header.Set("Sec-WebSocket-Protocol", "bearer, "+token)
	r.ServeHTTP(httptest.NewRecorder(), req)

	require.NotContains(t, output.String(), token)
	require.Contains(t, output.String(), `"path":"/ws"`)
}

func TestNewRouterRejectsInvalidMode(t *testing.T) {
	_, _, err := newRouter(
		&config.Config{Server: config.Server{Mode: "invalid"}},
		zap.NewNop(), nil, nil, nil, nil, nil, nil,
	)
	require.ErrorContains(t, err, "invalid Gin mode")
}

func TestRecoveryLogsDoNotContainWebSocketCredentials(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiI0MiJ9.sensitive-signature"
	log, output := testLogger()
	r := gin.New()
	r.Use(safeRecovery(log))
	r.GET("/ws", func(*gin.Context) { panic("boom") })

	req := httptest.NewRequest(http.MethodGet, "/ws?token="+token, nil)
	req.Header.Set("Sec-WebSocket-Protocol", "bearer, "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.JSONEq(t, `{"error":"internal error"}`, rec.Body.String())
	require.NotContains(t, output.String(), token)
	require.Contains(t, output.String(), `"path":"/ws"`)
}

func TestTrustedProxyConfigurationFailsClosed(t *testing.T) {
	for _, proxies := range [][]string{nil, {"not-a-cidr"}} {
		r := gin.New()
		configureTrustedProxies(r, proxies, zap.NewNop())
		r.GET("/ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

		req := httptest.NewRequest(http.MethodGet, "/ip", nil)
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113.99")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, "192.0.2.10", rec.Body.String())
	}
}

func TestTrustedProxyRestoresSourceIP(t *testing.T) {
	r := gin.New()
	configureTrustedProxies(r, []string{"192.0.2.10"}, zap.NewNop())
	r.GET("/ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, "203.0.113.99", rec.Body.String())
}

func TestUntrustedDockerPeerCannotSpoofSourceIP(t *testing.T) {
	r := gin.New()
	configureTrustedProxies(r, []string{"172.30.0.10"}, zap.NewNop())
	r.GET("/ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = "172.30.0.11:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, "172.30.0.11", rec.Body.String())
}

func TestRequestTimeoutFastFail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestTimeout(50 * time.Millisecond))
	r.GET("/slow", func(c *gin.Context) {
		// 模拟依赖挂起：不响应 ctx，但底层调用会感知截止时间快速返回。
		select {
		case <-c.Request.Context().Done():
			httpresponse.WriteError(c, c.Request.Context().Err())
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
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.JSONEq(t, `{"error":"request timeout"}`, rec.Body.String())

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fast", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

type blockingRequestBody struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingRequestBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingRequestBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestRequestTimeoutInterruptsSlowRequestBody(t *testing.T) {
	body := &blockingRequestBody{closed: make(chan struct{})}
	r := gin.New()
	r.Use(requestTimeout(20 * time.Millisecond))
	r.POST("/json", func(c *gin.Context) { _, _ = io.ReadAll(c.Request.Body) })
	req := httptest.NewRequest(http.MethodPost, "/json", nil)
	req.Body = body
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	select {
	case <-body.closed:
	default:
		t.Fatal("超时必须关闭请求体以中断慢速上传")
	}
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
	require.JSONEq(t, `{"error":"request timeout"}`, rec.Body.String())
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
