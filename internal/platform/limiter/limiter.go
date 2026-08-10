// Package limiter 共享限流基础设施：
//   - TokenBucket：进程内全局令牌桶中间件（golang.org/x/time/rate，QPS 可配，
//     单实例；多实例替代方案 = Redis 分布式限流，见 BACKLOG）；
//   - RedisCounter：基于 Redis INCR+TTL 的固定窗口计数限流（跨请求状态，
//     如秒杀接口按用户限流；Lua 脚本封装在本包，业务模块只面向方法）。
package limiter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
)

// ErrConfig 配置非法（构造校验用，防 x/time/rate 参数非法 panic）。
var ErrConfig = errors.New("invalid limiter config")

// TokenBucketConfig 全局令牌桶配置。
type TokenBucketConfig struct {
	// QPS 每秒令牌补充速率（桶容量为 Burst 时即满）。
	QPS float64
	// Burst 桶容量：允许的瞬时突发请求数。
	Burst int
}

// NewTokenBucket 构造全局令牌桶限流中间件：桶空时返回 429，不进入业务逻辑。
// QPS/Burst 来自配置，进程内单实例（多实例请见 BACKLOG 分布式限流）。
func NewTokenBucket(cfg TokenBucketConfig) (gin.HandlerFunc, error) {
	if cfg.QPS <= 0 || cfg.Burst < 1 {
		return nil, fmt.Errorf("%w: qps=%v burst=%d", ErrConfig, cfg.QPS, cfg.Burst)
	}
	lim := rate.NewLimiter(rate.Limit(cfg.QPS), cfg.Burst)
	return func(c *gin.Context) {
		if !lim.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}, nil
}

// RedisCounterConfig 固定窗口计数限流配置。
type RedisCounterConfig struct {
	// Max 窗口内允许的最大请求数（<=0 表示不启用限流）。
	Max int
	// Window 固定窗口长度；窗口结束计数随 key 过期自清理。
	Window time.Duration
}

// RedisCounter Redis 固定窗口计数限流器（跨请求状态）：
// 同一 key 在窗口内计数，超过 Max 拒绝。key 由调用方按业务约定提供。
type RedisCounter struct {
	cfg   RedisCounterConfig
	cache cache.Cache
}

// NewRedisCounter 构造限流器；Max<=0 时 Allow 恒放行。
func NewRedisCounter(c cache.Cache, cfg RedisCounterConfig) *RedisCounter {
	return &RedisCounter{cfg: cfg, cache: c}
}

// countScript Lua 原子计数（固定窗口）：key 不存在则 SET 1 + EXPIRE
// （窗口起点），已存在则 INCR。返回窗口内当前计数。
// KEYS[1] 计数 key；ARGV[1] 窗口秒数。
const countScript = `
if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], 1, 'EX', ARGV[1])
    return 1
end
return redis.call('INCR', KEYS[1])
`

// Allow 计数一次并判定放行：窗口内计数未超上限返回 true；
// 超过上限返回 false（计数不回落，由 TTL 自清理，与 Lua 预扣同"只增不删"原则）。
// 基础设施失败返回错误（fail-closed：限流不可用时拒绝放行，保护后端）。
func (r *RedisCounter) Allow(ctx context.Context, key string) (bool, error) {
	if r.cfg.Max <= 0 || r.cfg.Window <= 0 {
		return true, nil
	}
	n, err := r.cache.Eval(ctx, countScript, []string{key}, int(r.cfg.Window.Seconds()))
	if err != nil {
		return false, err
	}
	return n <= int64(r.cfg.Max), nil
}
