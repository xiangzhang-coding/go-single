package mq

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 集成测试：需要本地 RabbitMQ 就绪（deploy/docker-compose.yml）。
func TestRabbitMQPing(t *testing.T) {
	m, err := NewRabbitMQ("amqp://guest:guest@127.0.0.1:5672/")
	if err != nil {
		t.Skipf("RabbitMQ 不可用，跳过: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, m.Ping(ctx))
}

func TestRabbitMQUnavailable(t *testing.T) {
	_, err := NewRabbitMQ("amqp://guest:guest@127.0.0.1:1/")
	require.Error(t, err)
}
