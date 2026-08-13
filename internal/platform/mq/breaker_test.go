// 熔断器单元测试（T20 验收：熔断打开→快速失败→恢复后半开探活闭合）：
// fake MQ 模拟消息投递循环，直接驱动包装后的 handler，
// 验证连续失败打开、打开态不执行 handler（快速失败）、冷却后半开探活、成功闭合；
// 永久失败（业务拒绝）不计熔断失败；发布/健康检查直通不受熔断影响。
package mq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errTransient 测试用瞬时失败（模拟基础设施故障）。
var errTransient = errors.New("transient db error")

// fakeConsumerMQ 模拟消费投递循环：依次调用包装后的 handler 并记录结果
// （等价于 rabbitmq 层逐条投递，熔断器只感知 handler 的错误分类）。
type fakeConsumerMQ struct {
	wrapped  MessageHandler
	results  []error
	delivery int // 投递序号（handler 调用参数可观测）
}

func (f *fakeConsumerMQ) Ping(context.Context) error { return nil }

func (f *fakeConsumerMQ) Close() error { return nil }

func (f *fakeConsumerMQ) Publish(context.Context, string, []byte) error { return nil }

func (f *fakeConsumerMQ) Consume(_ context.Context, _ string, handler MessageHandler) error {
	f.wrapped = handler
	return nil
}

// deliver 投递一条消息并记录结果。
func (f *fakeConsumerMQ) deliver(t *testing.T, ctx context.Context) error {
	t.Helper()
	require.NotNil(t, f.wrapped, "应已进入 Consume")
	err := f.wrapped(ctx, []byte("msg"))
	f.results = append(f.results, err)
	return err
}

// newBreakerMQ 构建测试用熔断客户端（参数可注入，Time 控制状态流转速度）。
func newBreakerMQ(inner MQ, timeout time.Duration, threshold int) MQ {
	return WrapCircuitBreaker(inner, BreakerSettings{
		Name:                   "test-breaker",
		MaxConsecutiveFailures: threshold,
		Interval:               0, // 不按时间清零，便于确定性断言
		Timeout:                timeout,
	})
}

// 连续失败达阈值 → 打开 → 快速失败（不执行 handler）→ 冷却后半开探活成功 → 闭合。
func TestCircuitBreakerOpenFastFailHalfOpenClose(t *testing.T) {
	inner := &fakeConsumerMQ{}
	client := newBreakerMQ(inner, 150*time.Millisecond, 3)

	var (
		mu    sync.Mutex
		calls int
		ok    bool // 探活成功后恢复健康
	)
	client.Consume(context.Background(), "q", func(_ context.Context, _ []byte) error {
		mu.Lock()
		calls++
		healthy := ok
		mu.Unlock()
		if !healthy {
			return errTransient // 模拟基础设施故障
		}
		return nil
	})

	ctx := context.Background()

	// 1. 连续 3 次失败 → 熔断打开（第 4 条起不再执行 handler，快速失败）。
	for i := 0; i < 6; i++ {
		err := inner.deliver(t, ctx)
		if i < 3 {
			require.Error(t, err, "关闭态失败透传")
		} else {
			require.ErrorIs(t, err, ErrCircuitOpen, "打开态快速失败")
		}
	}
	mu.Lock()
	require.Equal(t, 3, calls, "打开后 handler 不再被调用（快速失败）")
	mu.Unlock()

	// 2. 冷却期未到：仍快速失败。
	time.Sleep(60 * time.Millisecond)
	require.ErrorIs(t, inner.deliver(t, ctx), ErrCircuitOpen)

	// 3. 故障恢复：冷却期到 → 半开放行单条探活 → 成功 → 闭合。
	mu.Lock()
	ok = true
	mu.Unlock()
	require.Eventually(t, func() bool {
		err := inner.deliver(t, ctx)
		return err == nil // 探活成功
	}, 2*time.Second, 30*time.Millisecond)

	// 4. 闭合后继续正常处理（handler 被调用，错误透传不快速失败）。
	mu.Lock()
	before := calls
	mu.Unlock()
	require.NoError(t, inner.deliver(t, ctx))
	mu.Lock()
	require.Greater(t, calls, before, "闭合后消息正常执行 handler")
	mu.Unlock()
}

// 永久失败（业务拒绝进死信）不计熔断失败：连续永久失败不会打开熔断。
func TestCircuitBreakerPermanentFailureDoesNotTrip(t *testing.T) {
	inner := &fakeConsumerMQ{}
	client := newBreakerMQ(inner, time.Second, 3)

	var calls int
	client.Consume(context.Background(), "q", func(context.Context, []byte) error {
		calls++
		return fmt.Errorf("%w: activity gone", ErrPermanent)
	})

	// 5 次永久失败（超过阈值 3）后，熔断仍闭合：消息继续执行 handler。
	for i := 0; i < 5; i++ {
		require.ErrorIs(t, inner.deliver(t, context.Background()), ErrPermanent, "永久失败透传（进死信）")
	}
	require.Equal(t, 5, calls, "永久失败不累计连续失败，熔断不打开")
}

// 打开态快速失败的错误应按瞬时失败分类（重投保留消息）：非 ErrPermanent。
func TestCircuitBreakerOpenIsTransient(t *testing.T) {
	inner := &fakeConsumerMQ{}
	client := newBreakerMQ(inner, time.Hour, 1)

	client.Consume(context.Background(), "q", func(context.Context, []byte) error {
		return errTransient
	})
	ctx := context.Background()
	// 阈值 1：首次失败即触发打开（gobreaker 语义：触发请求自身仍透传原错误）。
	require.ErrorIs(t, inner.deliver(t, ctx), errTransient)
	require.ErrorIs(t, inner.deliver(t, ctx), ErrCircuitOpen, "打开后快速失败")
	require.False(t, errors.Is(inner.results[len(inner.results)-1], ErrPermanent),
		"快速失败不得进死信（消息需保留重投）")
}

// 发布/健康检查直通：熔断打开不影响 Publish/Ping（熔断仅包消费者）。
func TestCircuitBreakerPublishPingPassthrough(t *testing.T) {
	inner := &fakeConsumerMQ{}
	client := newBreakerMQ(inner, time.Hour, 1)

	client.Consume(context.Background(), "q", func(context.Context, []byte) error {
		return errTransient
	})
	inner.deliver(t, context.Background()) // 触发打开
	require.ErrorIs(t, inner.deliver(t, context.Background()), ErrCircuitOpen)

	require.NoError(t, client.Publish(context.Background(), "q", []byte("body")), "发布不熔断")
	require.NoError(t, client.Ping(context.Background()), "健康检查不熔断")
}

// 集成测试（需本地 RabbitMQ 就绪，deploy/docker-compose.yml）：
// 模拟 MQ 故障——消费者处理失败（重投循环）连续达阈值 → 熔断打开 → 快速失败
// （handler 不再执行）→ 故障恢复 → 冷却后半开探活成功 → 闭合恢复消费。
func TestCircuitBreakerRabbitMQFailRecover(t *testing.T) {
	m, err := NewRabbitMQ("amqp://guest:guest@127.0.0.1:5672/")
	if err != nil {
		t.Skipf("RabbitMQ 不可用，跳过: %v", err)
	}
	defer m.Close()

	// 故障期快速失败、恢复期正常：timeout 1s 让半开探活尽快发生。
	client := WrapCircuitBreaker(m, BreakerSettings{
		Name:                   "it-breaker",
		MaxConsecutiveFailures: 3,
		Interval:               0,
		Timeout:                time.Second,
	})

	queue := uniqueQueue("mq.breaker")
	cleanupQueue(t, queue)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		calls   int
		failing bool
	)
	failing = true
	consumeDone := make(chan error, 1)
	go func() {
		consumeDone <- client.Consume(ctx, queue, func(_ context.Context, _ []byte) error {
			mu.Lock()
			calls++
			f := failing
			mu.Unlock()
			if f {
				return errTransient // 模拟 MQ/DB 故障：失败重投
			}
			return nil
		})
	}()
	select {
	case err := <-consumeDone:
		t.Fatalf("Consume 提前返回: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// 投递一条消息：失败重投循环累计连续失败 → 熔断打开 → 快速失败（handler 不再执行）。
	require.NoError(t, client.Publish(ctx, queue, []byte("breaker-me")))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 3 // 阈值 3 内 handler 逐条执行
	}, 10*time.Second, 50*time.Millisecond)
	// 打开后快速失败：消息持续重投但 handler 调用数稳定在阈值。
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	require.Equal(t, 3, got, "熔断打开后消息快速失败，handler 不再执行")

	// 故障恢复：冷却期后半开探活放行一条 → 成功 → 闭合 → 正常处理。
	mu.Lock()
	failing = false
	mu.Unlock()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls > 3 // 半开探活执行 handler 并成功
	}, 10*time.Second, 50*time.Millisecond)
}
