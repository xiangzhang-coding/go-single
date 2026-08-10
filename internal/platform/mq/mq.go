package mq

import (
	"context"
	"errors"
)

// ErrPermanent 永久失败标记：消息无法通过重试成功（业务拒绝/消息损坏等），
// 消费者把该错误包装进返回错误（fmt.Errorf("%w: ...", mq.ErrPermanent)）后，
// Consume 将 Nack 拒收（requeue=false）进入死信队列，由对账/人工补偿。
// 其余错误视为瞬时失败（基础设施抖动），Nack 重投（requeue=true，at-least-once）。
var ErrPermanent = errors.New("mq permanent failure")

// MessageHandler 单条消息处理回调：返回 nil → Ack；
// 返回普通错误 → Nack 重投（requeue，消费侧下轮重试）；
// 返回包装了 ErrPermanent 的错误 → Nack 拒收（进死信队列）。
// 处理应幂等（同一消息可能被重投多次），如落单经唯一约束去重。
type MessageHandler func(ctx context.Context, body []byte) error

// MQ 消息层接口（ADR-0003 seam），RabbitMQ 实现，Kafka 可换。
type MQ interface {
	// Ping 检查连接可用性。
	Ping(ctx context.Context) error
	// Close 释放底层连接。
	Close() error
	// Publish 向队列投递持久化消息（队列不存在自动声明，声明幂等）；
	// 经发布确认（publisher confirm）确保送达，失败返回错误（调用方决定重试/降级）。
	Publish(ctx context.Context, queue string, body []byte) error
	// Consume 消费队列消息直到 ctx 取消；每条消息以独立超时执行 handler，
	// 按 handler 返回错误分类 Ack / 重投 / 死信（见 MessageHandler）。
	// 消费者退出/连接断开时未 Ack 的消息由 RabbitMQ 自动重投（at-least-once）。
	Consume(ctx context.Context, queue string, handler MessageHandler) error
}
