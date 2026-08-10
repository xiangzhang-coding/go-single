package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 集成测试：需要本地 Redis 就绪（deploy/docker-compose.yml）。
// 未就绪时自动跳过，不影响单元测试。
func TestRedisPing(t *testing.T) {
	c, err := NewRedis("127.0.0.1:6379", "", 0)
	if err != nil {
		t.Skipf("Redis 不可用，跳过: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.Ping(ctx))
}

func TestRedisUnavailable(t *testing.T) {
	_, err := NewRedis("127.0.0.1:1", "", 0)
	require.Error(t, err)
}
