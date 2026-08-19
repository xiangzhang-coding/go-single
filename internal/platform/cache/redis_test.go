package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/testsupport"
)

// redisTestDB 供 cache 包独占，避免与业务集成测试的 DB 15-20 并行污染。
const redisTestDB = 14

// 集成测试：需要本地 Redis 就绪（deploy/docker-compose.yml）。
// 未就绪时本地跳过、CI 失败。
func TestRedisPing(t *testing.T) {
	c, err := NewRedis("127.0.0.1:6379", "", 0)
	testsupport.RequireDependency(t, "Redis", err)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.Ping(ctx))
}

func TestRedisUnavailable(t *testing.T) {
	_, err := NewRedis("127.0.0.1:1", "", 0)
	require.Error(t, err)
}

// Get/Set/Del 与 ErrMiss 语义（测试用独立 DB，避免污染）。
func TestRedisGetSetDel(t *testing.T) {
	c, err := NewRedis("127.0.0.1:6379", "", redisTestDB)
	testsupport.RequireDependency(t, "Redis", err)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := "cache_test:getsetdel"
	require.NoError(t, c.Del(ctx, key))

	_, err = c.Get(ctx, key)
	require.ErrorIs(t, err, ErrMiss, "未命中的 key 应返回 ErrMiss")

	require.NoError(t, c.Set(ctx, key, "v1", time.Minute))
	got, err := c.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "v1", got)

	// 覆盖写 + TTL 生效。
	require.NoError(t, c.Set(ctx, key, "v2", time.Second))
	got, err = c.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "v2", got)
	require.Eventually(t, func() bool {
		checkCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, getErr := c.Get(checkCtx, key)
		return errors.Is(getErr, ErrMiss)
	}, 4*time.Second, 50*time.Millisecond, "TTL 过期后应视为未命中")

	require.NoError(t, c.Del(ctx, key))
	_, err = c.Get(ctx, key)
	require.ErrorIs(t, err, ErrMiss)
}
