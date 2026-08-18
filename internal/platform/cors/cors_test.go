package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestRouter 挂载 CORS 中间件与一个受保护示例路由。
func newTestRouter(allowOrigins []string) *gin.Engine {
	r := gin.New()
	r.Use(Middleware(allowOrigins))
	r.GET("/api/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func TestSameOriginNoOriginHeader(t *testing.T) {
	r := newTestRouter([]string{"https://shop.example.com"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestAllowedOriginGetsHeaders(t *testing.T) {
	r := newTestRouter([]string{"https://shop.example.com"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "https://shop.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, w.Header().Get("Access-Control-Allow-Methods"), "OPTIONS")
	require.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "Content-Disposition")
}

func TestDisallowedOriginNoHeaders(t *testing.T) {
	r := newTestRouter([]string{"https://shop.example.com"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestPreflightAllowedOrigin(t *testing.T) {
	r := newTestRouter([]string{"https://shop.example.com"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/ping", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, "https://shop.example.com", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestPreflightDisallowedOrigin(t *testing.T) {
	r := newTestRouter([]string{"https://shop.example.com"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestEmptyAllowListAllowsAll(t *testing.T) {
	r := newTestRouter(nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "https://anything.example.com", w.Header().Get("Access-Control-Allow-Origin"))
}
