package mq

import "context"

// MQ 消息层接口（ADR-0003 seam），RabbitMQ 实现，Kafka 可换。
type MQ interface {
	// Ping 检查连接可用性。
	Ping(ctx context.Context) error
	// Close 释放底层连接。
	Close() error
}
