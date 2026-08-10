package mq

import (
	"context"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type rabbitMQ struct {
	conn *amqp.Connection
}

// NewRabbitMQ 建立 RabbitMQ 连接（amqp091 客户端）。
func NewRabbitMQ(url string) (MQ, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ 连接失败: %w", err)
	}
	return &rabbitMQ{conn: conn}, nil
}

// Ping 通过打开/关闭一个 channel 验证连接可用，受 ctx 超时约束。
func (r *rabbitMQ) Ping(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		ch, err := r.conn.Channel()
		if err != nil {
			done <- fmt.Errorf("RabbitMQ 不可用: %w", err)
			return
		}
		ch.Close()
		done <- nil
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (r *rabbitMQ) Close() error {
	return r.conn.Close()
}
