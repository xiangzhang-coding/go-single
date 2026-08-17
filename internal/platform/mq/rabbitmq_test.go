package mq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/testsupport"
)

func testRabbitURL() string {
	if url := os.Getenv("GO_SINGLE_MQ_URL"); url != "" {
		return url
	}
	return "amqp://guest:guest@127.0.0.1:5672/"
}

// 集成测试：需要本地 RabbitMQ 就绪（deploy/docker-compose.yml）。
func TestRabbitMQPing(t *testing.T) {
	m, err := NewRabbitMQ(testRabbitURL())
	testsupport.RequireDependency(t, "RabbitMQ", err)
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, m.Ping(ctx))
}

func TestRabbitMQUnavailable(t *testing.T) {
	_, err := NewRabbitMQ("amqp://guest:guest@127.0.0.1:1/")
	require.Error(t, err)
}

// newTestMQ 连接测试 RabbitMQ；不可达时本地跳过、CI 失败。
func newTestMQ(t *testing.T) MQ {
	t.Helper()
	m, err := NewRabbitMQ(testRabbitURL())
	testsupport.RequireDependency(t, "RabbitMQ", err)
	t.Cleanup(func() { m.Close() })
	return m
}

// uniqueQueue 每次测试独立队列，避免跨测试消息串扰。
func uniqueQueue(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// cleanupQueue 测试结束删除随机队列（含死信队列），避免跨运行残留堆积；
// 有消费者仍占用时 RabbitMQ 拒绝删除（无副作用，下次运行 uniqueQueue 不受影响）。
func cleanupQueue(t *testing.T, queue string) {
	t.Helper()
	t.Cleanup(func() {
		conn, err := amqp.Dial(testRabbitURL())
		if err != nil {
			return // broker 不可达时无残留可清
		}
		defer conn.Close()
		ch, err := conn.Channel()
		if err != nil {
			return
		}
		defer ch.Close()
		for _, q := range []string{queue, queue + ".dlq"} {
			_, _ = ch.QueueDelete(q, false, false, false)
		}
	})
}

// receiveOne 从指定队列取一条消息（3s 超时）；不存在返回 nil。
// 测试直连 amqp：Consume 为常驻循环，验收场景直接读队列更直接。
func receiveOne(t *testing.T, queue string) []byte {
	t.Helper()
	conn, err := amqp.Dial(testRabbitURL())
	require.NoError(t, err)
	defer conn.Close()
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("声明队列 %s: %v", queue, err)
	}
	msgs, err := ch.Consume(queue, "", true, false, false, false, nil)
	require.NoError(t, err)
	select {
	case d := <-msgs:
		return d.Body
	case <-time.After(3 * time.Second):
		return nil
	}
}

// 发布 → 消费闭环：发布的消息可被消费者（Ack）收到；失败重投；永久失败进死信。
func TestRabbitMQPublishConsumeRoundtrip(t *testing.T) {
	m := newTestMQ(t)
	queue := uniqueQueue("mq.roundtrip")
	cleanupQueue(t, queue)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 消费者：收到后 Ack（成功路径）。
	done := make(chan []byte, 1)
	go func() {
		err := m.Consume(ctx, queue, func(_ context.Context, body []byte) error {
			done <- body
			return nil
		})
		require.NoError(t, err)
	}()

	require.NoError(t, m.Publish(ctx, queue, []byte("hello-mq")))
	select {
	case body := <-done:
		require.Equal(t, "hello-mq", string(body))
	case <-time.After(5 * time.Second):
		t.Fatal("消费者未在超时内收到消息")
	}
}

// 消费者返回普通错误 → Nack 重投（requeue）：消息再次投递，at-least-once 不丢。
func TestRabbitMQConsumeRequeuesOnTransientFailure(t *testing.T) {
	m := newTestMQ(t)
	queue := uniqueQueue("mq.retry")
	cleanupQueue(t, queue)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		deliver int
	)
	delivered := make(chan []byte, 3)
	go func() {
		err := m.Consume(ctx, queue, func(_ context.Context, body []byte) error {
			mu.Lock()
			deliver++
			n := deliver
			mu.Unlock()
			delivered <- body
			if n < 3 { // 前两次投递失败重投，第三次成功
				return errors.New("transient db error")
			}
			return nil
		})
		require.NoError(t, err)
	}()

	require.NoError(t, m.Publish(ctx, queue, []byte("retry-me")))
	for i := 0; i < 3; i++ {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 次投递未收到", i+1)
		}
	}
	mu.Lock()
	require.Equal(t, 3, deliver, "失败应重投，第三次消费成功")
	mu.Unlock()
}

// 消费者返回包装 ErrPermanent 的错误 → Nack 拒收进死信队列（DLQ）。
func TestRabbitMQConsumeDeadLettersOnPermanentFailure(t *testing.T) {
	m := newTestMQ(t)
	queue := uniqueQueue("mq.dlq")
	cleanupQueue(t, queue)
	dlq := queue + dlqSuffix
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		err := m.Consume(ctx, queue, func(context.Context, []byte) error {
			return fmt.Errorf("%w: activity gone", ErrPermanent)
		})
		require.NoError(t, err)
	}()

	require.NoError(t, m.Publish(ctx, queue, []byte("dead-letter-me")))
	body := receiveOne(t, dlq)
	require.NotNil(t, body, "永久失败消息应进入死信队列")
	require.Equal(t, "dead-letter-me", string(body))
}

// 消费上下文取消：Consume 正常返回 nil（优雅退出）。
func TestRabbitMQConsumeStopsOnCancel(t *testing.T) {
	m := newTestMQ(t)
	queue := uniqueQueue("mq.cancel")
	cleanupQueue(t, queue)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan error, 1)
	go func() { done <- m.Consume(ctx, queue, func(context.Context, []byte) error { return nil }) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 Consume 应返回")
	}
	cancel()
}
