// limiter 单元测试：令牌桶中间件（429 语义）+ RedisCounter（原子计数语义以
// fake cache 模拟；真实 Redis 语义经 cache 适配器测试覆盖）。
package limiter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---- fake cache（镜像固定窗口原子计数语义）----

type fakeCache struct {
	mu   map[string]int
	err  error
	keys []string
}

func (f *fakeCache) IncrementFixedWindow(_ context.Context, key string, _ time.Duration) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.keys = append(f.keys, key)
	if f.mu[key] == 0 {
		f.mu[key] = 1
	} else {
		f.mu[key]++
	}
	return int64(f.mu[key]), nil
}

// ---- 令牌桶中间件 ----

func TestTokenBucketAllowsWithinBurst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limit, err := NewTokenBucket(TokenBucketConfig{QPS: 100, Burst: 10})
	require.NoError(t, err)

	r := gin.New()
	r.GET("/", limit, func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusOK, w.Code, "第 %d 次请求应在突发容量内放行", i+1)
	}
}

func TestTokenBucketRejectsOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limit, err := NewTokenBucket(TokenBucketConfig{QPS: 1, Burst: 1})
	require.NoError(t, err)

	r := gin.New()
	r.GET("/", limit, func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, w.Code)

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusTooManyRequests, w2.Code, "桶空应 429")

	// 限流拒绝时不进入业务逻辑（response 为中间件直接写入）。
	require.Contains(t, w2.Body.String(), "rate limit exceeded")
}

func TestTokenBucketInvalidConfig(t *testing.T) {
	_, err := NewTokenBucket(TokenBucketConfig{QPS: 0, Burst: 10})
	require.ErrorIs(t, err, ErrConfig)
	_, err = NewTokenBucket(TokenBucketConfig{QPS: 10, Burst: 0})
	require.ErrorIs(t, err, ErrConfig)
}

// ---- RedisCounter（固定窗口计数）----

func TestRedisCounterAllowsWithinMax(t *testing.T) {
	fc := &fakeCache{mu: map[string]int{}}
	counter := NewRedisCounter(fc, RedisCounterConfig{Max: 3, Window: time.Minute})

	ok, err := counter.Allow(context.Background(), "rl:user:1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = counter.Allow(context.Background(), "rl:user:1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = counter.Allow(context.Background(), "rl:user:1")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestRedisCounterRejectsOverMax(t *testing.T) {
	fc := &fakeCache{mu: map[string]int{}}
	counter := NewRedisCounter(fc, RedisCounterConfig{Max: 2, Window: time.Minute})

	for i := 0; i < 2; i++ {
		ok, err := counter.Allow(context.Background(), "rl:user:1")
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := counter.Allow(context.Background(), "rl:user:1")
	require.NoError(t, err)
	require.False(t, ok, "窗口内超过 Max 应拒绝")
}

func TestRedisCounterKeysAreIsolated(t *testing.T) {
	fc := &fakeCache{mu: map[string]int{}}
	counter := NewRedisCounter(fc, RedisCounterConfig{Max: 1, Window: time.Minute})

	ok, _ := counter.Allow(context.Background(), "rl:user:1")
	require.True(t, ok)
	ok, err := counter.Allow(context.Background(), "rl:user:2")
	require.NoError(t, err)
	require.True(t, ok, "不同 key 计数互不影响")
}

func TestRedisCounterDisabled(t *testing.T) {
	counter := NewRedisCounter(&fakeCache{}, RedisCounterConfig{})
	ok, err := counter.Allow(context.Background(), "rl:user:1")
	require.NoError(t, err)
	require.True(t, ok, "Max<=0 应恒放行且不触碰缓存")
}

func TestRedisCounterFailClosed(t *testing.T) {
	fc := &fakeCache{mu: map[string]int{}, err: strconv.ErrSyntax}
	counter := NewRedisCounter(fc, RedisCounterConfig{Max: 10, Window: time.Minute})

	ok, err := counter.Allow(context.Background(), "rl:user:1")
	require.Error(t, err)
	require.False(t, ok, "基础设施失败应拒绝放行（fail-closed）")
}

// 计数脚本语义：key 缺失首次计数为 1（不依赖 INCR 前置 GET）。
func TestCountScriptFirstCountIsOne(t *testing.T) {
	fc := &fakeCache{mu: map[string]int{}}
	counter := NewRedisCounter(fc, RedisCounterConfig{Max: 1, Window: time.Minute})

	ok, err := counter.Allow(context.Background(), "rl:user:9")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, fc.mu["rl:user:9"])
	require.True(t, strings.HasPrefix(fc.keys[0], "rl:"))
}
