package mq

import (
	"context"
	"errors"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// 队列参数：
//   - 主队列持久化（durable），携带死信配置：消息被 Nack 拒收（requeue=false）时
//     经默认交换机按 DLQ 队列名路由进死信队列（x-dead-letter-routing-key）。
//   - 死信队列命名约定：<主队列>.dlq，持久化，供对账/人工补偿消费。
const (
	dlqSuffix = ".dlq"
	// msgTimeout 单条消息处理超时（全链路 context 超时逐层传递）。
	msgTimeout = 15 * time.Second
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

// Publish 投递持久化消息：
//  1. 独立 channel + 发布确认模式（Confirm），WaitForConfirmation 确认送达；
//  2. 队列不存在自动声明（含死信配置，声明幂等）。
//
// 每次发布使用独立 channel（并发安全，学习取舍；高吞吐可换连接级复用）。
func (r *rabbitMQ) Publish(ctx context.Context, queue string, body []byte) error {
	ch, err := r.conn.Channel()
	if err != nil {
		return fmt.Errorf("MQ 发布开 channel: %w", err)
	}
	defer ch.Close()

	if err := declareQueue(ch, queue); err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("MQ 发布确认不可用: %w", err)
	}
	conf, err := ch.PublishWithDeferredConfirm("", queue, false, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent, // 持久化消息：broker 重启不丢
		ContentType:  "application/json",
		Body:         body,
	})
	if err != nil {
		return fmt.Errorf("MQ 发布: %w", err)
	}
	// 发布确认：等 broker 落盘确认；超时（ctx 到点）视为未送达，调用方重试/降级。
	acked, err := conf.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("MQ 发布未获确认: %w", err)
	}
	if !acked {
		return fmt.Errorf("MQ 发布被 broker 拒收（nack）")
	}
	return nil
}

// Consume 消费队列：手动 Ack 三态（成功/重投/死信）+ 每条消息独立超时；
// 运行至 ctx 取消返回 nil，或 channel 异常返回错误（调用方负责重连）。
func (r *rabbitMQ) Consume(ctx context.Context, queue string, handler MessageHandler) error {
	ch, err := r.conn.Channel()
	if err != nil {
		return fmt.Errorf("MQ 消费开 channel: %w", err)
	}
	defer ch.Close()
	// 消费端 QoS：预取 1，单消费者顺序处理、天然串行落单。
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("MQ 消费 QoS: %w", err)
	}
	if err := declareQueue(ch, queue); err != nil {
		return err
	}
	msgs, err := ch.Consume(queue, "", false /* manual ack */, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("MQ 开始消费: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("MQ 消费通道关闭")
			}
			if err := consumeOne(ctx, d, handler); err != nil {
				// Ack/Nack 自身失败（channel 异常）：退出消费循环，
				// 由调用方（main）重连；未确认消息由 broker 自动重投（at-least-once）。
				return err
			}
		}
	}
}

// consumeOne 单条消息：超时执行 handler 后按错误分类确认。
// 返回 nil 表示消息已确认；返回错误表示确认自身失败（channel 异常，调用方重连）。
func consumeOne(ctx context.Context, d amqp.Delivery, handler MessageHandler) error {
	hctx, cancel := context.WithTimeout(ctx, msgTimeout)
	defer cancel()
	derr := handler(hctx, d.Body)

	switch {
	case derr == nil:
		if err := d.Ack(false); err != nil {
			return fmt.Errorf("MQ Ack: %w", err)
		}
	case errors.Is(derr, ErrPermanent):
		// 永久失败：拒收进死信队列（requeue=false，经 x-dead-letter 配置路由）。
		if err := d.Nack(false, false); err != nil {
			return fmt.Errorf("MQ Nack 死信: %w", err)
		}
	default:
		// 瞬时失败：重投（requeue=true），at-least-once 不丢消息。
		if err := d.Nack(false, true); err != nil {
			return fmt.Errorf("MQ Nack 重投: %w", err)
		}
	}
	return nil
}

// declareQueue 声明主队列（含死信配置）与死信队列，声明幂等（已存在则 no-op）。
// 主队列 x-dead-letter-exchange 为空串 = 默认交换机，路由键指向死信队列名。
func declareQueue(ch *amqp.Channel, queue string) error {
	dlq := queue + dlqSuffix
	if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
		return fmt.Errorf("MQ 声明死信队列 %s: %w", dlq, err)
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": dlq,
	}); err != nil {
		return fmt.Errorf("MQ 声明队列 %s: %w", queue, err)
	}
	return nil
}
