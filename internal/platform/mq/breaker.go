// 消费者熔断（T20）：gobreaker 包住 MQ 消费者（仅消费者，发布不包）。
//
// 语义（DESIGN.md 容错约定）：
//   - 关闭：逐条消息经熔断器执行 handler；瞬时失败（基础设施故障）累计连续失败数，
//     永久失败（业务拒绝进死信）视为"已处理"，不计失败；
//   - 连续失败达阈值 → 打开：消息快速失败（不执行 handler，Nack 重投由 mq 层
//     按普通错误分类处理），防止故障下游被打爆；
//   - 冷却期（Timeout）后 → 半开：放行单条探活消息，成功 → 闭合恢复；
//     失败 → 重新打开。
//
// 进程内调用与本地 Redis/MySQL 不包（跨模块 service 调用不带熔断）。
package mq

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sony/gobreaker"
)

// ErrCircuitOpen 熔断打开标记：消费者快速失败时包装返回（mq 层按瞬时失败
// Nack 重投，消息保留，待熔断闭合后重新处理；对账兜底不受影响）。
var ErrCircuitOpen = errors.New("mq circuit breaker open")

// BreakerSettings 熔断参数：由装配方经 config 构建（mq.circuit.*）。
type BreakerSettings struct {
	// Name 熔断器名称（日志/状态变化事件标识）。
	Name string
	// MaxConsecutiveFailures 连续失败阈值：达到即打开（ReadyToTrip）。
	MaxConsecutiveFailures int
	// Interval 关闭态失败计数清零周期（<=0 不按时间清零）。
	Interval time.Duration
	// Timeout 打开态冷却时长，到点进入半开（放行探活）。
	Timeout time.Duration
}

// WrapCircuitBreaker 装饰 MQ 客户端：仅 Consume 的每条消息经熔断器执行，
// Publish/Ping/Close 直通（熔断仅包消费者）。半开并发放行固定 1（MaxRequests=1）。
func WrapCircuitBreaker(inner MQ, settings BreakerSettings) MQ {
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        settings.Name,
		MaxRequests: 1,
		Interval:    settings.Interval,
		Timeout:     settings.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			threshold := settings.MaxConsecutiveFailures
			if threshold <= 0 {
				threshold = 3
			}
			return counts.ConsecutiveFailures >= uint32(threshold)
		},
	})
	return &breakerMQ{inner: inner, cb: cb}
}

type breakerMQ struct {
	inner MQ
	cb    *gobreaker.CircuitBreaker
}

func (b *breakerMQ) Ping(ctx context.Context) error { return b.inner.Ping(ctx) }

func (b *breakerMQ) Close() error { return b.inner.Close() }

// Publish 直通不熔断：发布失败由调用方有限重试（T20，幂等操作）或对账兜底。
func (b *breakerMQ) Publish(ctx context.Context, queue string, body []byte) error {
	return b.inner.Publish(ctx, queue, body)
}

// Consume 包装每条消息的 handler：经熔断器执行，错误分类与 mq 层约定一致
// （ErrPermanent → 死信；其余 → 重投）；熔断打开时快速失败（不执行 handler）。
func (b *breakerMQ) Consume(ctx context.Context, queue string, handler MessageHandler) error {
	wrapped := func(hctx context.Context, body []byte) error {
		return b.handle(queue, hctx, body, handler)
	}
	return b.inner.Consume(ctx, queue, wrapped)
}

// handle 单条消息经熔断器执行：
//   - handler 成功 → 熔断计成功，返回 nil（Ack）；
//   - handler 返回 ErrPermanent → 业务拒绝视为"已处理"，熔断计成功，原样返回
//     （mq 层 Nack 拒收进死信，不因个别毒消息误伤整个消费者）；
//   - handler 返回普通错误 → 熔断计失败，原样返回（mq 层 Nack 重投）；
//   - 熔断打开 → 不执行 handler，快速失败返回 ErrCircuitOpen（重投保留消息，
//     半开探活成功即闭合）。
func (b *breakerMQ) handle(queue string, ctx context.Context, body []byte, handler MessageHandler) error {
	var handledErr error
	_, err := b.cb.Execute(func() (any, error) {
		handleErr := handler(ctx, body)
		if errors.Is(handleErr, ErrPermanent) {
			// 已处理（进死信）：不计熔断失败，但错误仍需透传给 mq 层分类。
			handledErr = handleErr
			return nil, nil
		}
		return nil, handleErr
	})
	if errors.Is(err, gobreaker.ErrOpenState) {
		return fmt.Errorf("%w: queue %s", ErrCircuitOpen, queue)
	}
	if handledErr != nil {
		return handledErr
	}
	return err
}
