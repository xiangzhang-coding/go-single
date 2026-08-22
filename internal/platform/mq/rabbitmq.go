package mq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
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
	// transientRequeueDelay 限制瞬时失败和熔断打开时的重投频率。
	transientRequeueDelay = 500 * time.Millisecond
	// 重拨采用共享的有界指数退避，避免 broker 故障时并发调用形成拨号风暴。
	reconnectMinBackoff  = 25 * time.Millisecond
	reconnectMaxBackoff  = time.Second
	reconnectDialTimeout = 5 * time.Second
)

type requeueBackoff func(context.Context) error

var errRabbitMQClosed = errors.New("RabbitMQ 已关闭")

type deferredConfirmation interface {
	WaitContext(context.Context) (bool, error)
}

type amqpChannel interface {
	Close() error
	Qos(prefetchCount, prefetchSize int, global bool) error
	QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error)
	Confirm(noWait bool) error
	PublishWithDeferredConfirm(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) (deferredConfirmation, error)
	Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
}

type amqpConnection interface {
	Channel() (amqpChannel, error)
	Close() error
	IsClosed() bool
}

type connectionDialer func(context.Context, string, amqp.Config) (amqpConnection, error)

type realConnection struct {
	conn *amqp.Connection
}

func (c *realConnection) Channel() (amqpChannel, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, err
	}
	return &realChannel{Channel: ch}, nil
}

func (c *realConnection) Close() error {
	err := c.conn.CloseDeadline(time.Now())
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil
	}
	return err
}

func (c *realConnection) IsClosed() bool { return c.conn.IsClosed() }

type realChannel struct {
	*amqp.Channel
}

func (c *realChannel) PublishWithDeferredConfirm(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) (deferredConfirmation, error) {
	return c.Channel.PublishWithDeferredConfirm(exchange, key, mandatory, immediate, msg)
}

type managedConnection struct {
	conn amqpConnection
}

type contextChannel struct {
	ctx       context.Context
	interrupt func()
	channel   amqpChannel
}

func (c *contextChannel) Close() error {
	return callErrorWithContext(c.ctx, c.interrupt, c.channel.Close)
}

func (c *contextChannel) Qos(prefetchCount, prefetchSize int, global bool) error {
	return callErrorWithContext(c.ctx, c.interrupt, func() error {
		return c.channel.Qos(prefetchCount, prefetchSize, global)
	})
}

func (c *contextChannel) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	return callWithContext(c.ctx, c.interrupt, func() (amqp.Queue, error) {
		return c.channel.QueueDeclare(name, durable, autoDelete, exclusive, noWait, args)
	})
}

func (c *contextChannel) Confirm(noWait bool) error {
	return callErrorWithContext(c.ctx, c.interrupt, func() error {
		return c.channel.Confirm(noWait)
	})
}

func (c *contextChannel) PublishWithDeferredConfirm(exchange, key string, mandatory, immediate bool, msg amqp.Publishing) (deferredConfirmation, error) {
	return callWithContext(c.ctx, c.interrupt, func() (deferredConfirmation, error) {
		return c.channel.PublishWithDeferredConfirm(exchange, key, mandatory, immediate, msg)
	})
}

func (c *contextChannel) Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error) {
	return callWithContext(c.ctx, c.interrupt, func() (<-chan amqp.Delivery, error) {
		return c.channel.Consume(queue, consumer, autoAck, exclusive, noLocal, noWait, args)
	})
}

func callErrorWithContext(ctx context.Context, interrupt func(), call func() error) error {
	_, err := callWithContext(ctx, interrupt, func() (struct{}, error) {
		return struct{}{}, call()
	})
	return err
}

func callWithContext[T any](ctx context.Context, interrupt func(), call func() (T, error)) (T, error) {
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		interrupt()
	})
	result, err := call()
	stopped := stopInterrupt()
	if !stopped {
		<-interrupted
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if stopped {
			interrupt()
		}
		var zero T
		return zero, ctxErr
	}
	return result, err
}

type rabbitMQ struct {
	mu           sync.Mutex
	conn         *managedConnection
	url          string
	config       amqp.Config
	dial         connectionDialer
	closed       bool
	closeCtx     context.Context
	closeCancel  context.CancelFunc
	reconnecting chan struct{}
	retryAt      time.Time
	retryDelay   time.Duration
}

// NewRabbitMQ 建立 RabbitMQ 连接（amqp091 客户端）。
func NewRabbitMQ(url string) (MQ, error) {
	return newRabbitMQWithDialer(url, dialAMQP)
}

func newRabbitMQWithDialer(url string, dial connectionDialer) (MQ, error) {
	config := amqp.Config{
		Heartbeat: 10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), reconnectDialTimeout)
	defer cancel()
	conn, err := dial(ctx, url, config)
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ 连接失败: %w", err)
	}
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &rabbitMQ{
		conn:        &managedConnection{conn: conn},
		url:         url,
		config:      config,
		dial:        dial,
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
		retryDelay:  reconnectMinBackoff,
	}, nil
}

func dialAMQP(ctx context.Context, url string, config amqp.Config) (amqpConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stopContextClose func() bool
	var rawConn net.Conn
	config.Dial = func(network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		rawConn = conn
		stopContextClose = context.AfterFunc(ctx, func() { _ = conn.Close() })
		deadline := time.Now().Add(reconnectDialTimeout)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
	conn, err := amqp.DialConfig(url, config)
	if stopContextClose != nil {
		stopContextClose()
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if rawConn != nil {
		if err := rawConn.SetDeadline(time.Time{}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("清除 RabbitMQ 拨号超时: %w", err)
		}
	}
	return &realConnection{conn: conn}, nil
}

// Ping 通过打开/关闭一个 channel 验证连接可用，受 ctx 超时约束。
func (r *rabbitMQ) Ping(ctx context.Context) error {
	for {
		conn, ch, err := r.openChannel(ctx)
		if err != nil {
			return fmt.Errorf("RabbitMQ 不可用: %w", err)
		}
		err = ch.Close()
		if err == nil {
			return nil
		}
		if !r.connectionClosed(conn, err) {
			return fmt.Errorf("RabbitMQ 不可用: %w", err)
		}
		r.invalidate(conn)
	}
}

func (r *rabbitMQ) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.closeCancel()
	conn := r.conn
	r.conn = nil
	r.mu.Unlock()
	if conn == nil {
		return nil
	}
	err := conn.conn.Close()
	if errors.Is(err, amqp.ErrClosed) {
		return nil
	}
	return err
}

// Publish 投递持久化消息：
//  1. 独立 channel + 发布确认模式（Confirm），WaitForConfirmation 确认送达；
//  2. 队列不存在自动声明（含死信配置，声明幂等）。
//
// 每次发布使用独立 channel（并发安全，学习取舍；高吞吐可换连接级复用）。
func (r *rabbitMQ) Publish(ctx context.Context, queue string, body []byte) error {
	for {
		conn, ch, err := r.openChannel(ctx)
		if err != nil {
			return fmt.Errorf("MQ 发布开 channel: %w", err)
		}
		err = publishOnce(ctx, ch, queue, body)
		_ = ch.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if !r.connectionClosed(conn, err) {
			return err
		}
		r.invalidate(conn)
	}
}

func publishOnce(ctx context.Context, ch amqpChannel, queue string, body []byte) error {
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
// 连接中断时自动重拨并恢复消费，运行至 ctx 取消返回 nil。
func (r *rabbitMQ) Consume(ctx context.Context, queue string, handler MessageHandler) error {
	for {
		conn, ch, err := r.openChannel(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("MQ 消费开 channel: %w", err)
		}
		err = consumeSession(ctx, ch, queue, handler)
		_ = ch.Close()
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if !r.connectionClosed(conn, err) {
			return err
		}
		r.invalidate(conn)
	}
}

func consumeSession(ctx context.Context, ch amqpChannel, queue string, handler MessageHandler) error {
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
				return fmt.Errorf("MQ 消费通道关闭: %w", amqp.ErrClosed)
			}
			if err := consumeOne(ctx, d, handler, waitTransientRequeue); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return nil
				}
				return err
			}
		}
	}
}

func (r *rabbitMQ) openChannel(ctx context.Context) (*managedConnection, amqpChannel, error) {
	for {
		conn, err := r.connection(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		interrupt := func() { r.invalidate(conn) }
		ch, err := callWithContext(ctx, interrupt, conn.conn.Channel)
		if err == nil {
			return conn, &contextChannel{ctx: ctx, interrupt: interrupt, channel: ch}, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !r.connectionClosed(conn, err) {
			return nil, nil, err
		}
		r.invalidate(conn)
	}
}

func (r *rabbitMQ) connection(ctx context.Context) (*managedConnection, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, errRabbitMQClosed
		}
		if r.conn != nil && !r.conn.conn.IsClosed() {
			conn := r.conn
			r.mu.Unlock()
			return conn, nil
		}
		if r.conn != nil {
			r.conn = nil
		}
		if r.reconnecting != nil {
			done := r.reconnecting
			closeDone := r.closeCtx.Done()
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-closeDone:
				return nil, errRabbitMQClosed
			case <-done:
				continue
			}
		}
		if wait := time.Until(r.retryAt); wait > 0 {
			closeDone := r.closeCtx.Done()
			r.mu.Unlock()
			if err := waitReconnect(ctx, closeDone, wait); err != nil {
				return nil, err
			}
			continue
		}

		done := make(chan struct{})
		r.reconnecting = done
		url, config, dial := r.url, r.config, r.dial
		closeCtx := r.closeCtx
		r.mu.Unlock()

		dialCtx, cancel := context.WithCancel(ctx)
		stopCloseCancel := context.AfterFunc(closeCtx, cancel)
		newConn, err := dial(dialCtx, url, config)
		stopCloseCancel()
		cancel()

		r.mu.Lock()
		r.reconnecting = nil
		close(done)
		if r.closed {
			r.mu.Unlock()
			if newConn != nil {
				_ = newConn.Close()
			}
			return nil, errRabbitMQClosed
		}
		if err != nil {
			if ctx.Err() == nil {
				r.retryAt = time.Now().Add(r.retryDelay)
				r.retryDelay = min(r.retryDelay*2, reconnectMaxBackoff)
			}
			r.mu.Unlock()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		conn := &managedConnection{conn: newConn}
		r.conn = conn
		r.retryAt = time.Time{}
		r.retryDelay = reconnectMinBackoff
		r.mu.Unlock()
		return conn, nil
	}
}

func waitReconnect(ctx context.Context, closed <-chan struct{}, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return errRabbitMQClosed
	case <-timer.C:
		return nil
	}
}

func (r *rabbitMQ) connectionClosed(conn *managedConnection, err error) bool {
	return conn.conn.IsClosed() || errors.Is(err, amqp.ErrClosed)
}

func (r *rabbitMQ) invalidate(conn *managedConnection) {
	r.mu.Lock()
	if r.conn != conn {
		r.mu.Unlock()
		return
	}
	r.conn = nil
	r.mu.Unlock()
	_ = conn.conn.Close()
}

// consumeOne 单条消息：超时执行 handler 后按错误分类确认。
// 返回 nil 表示消息已确认；返回错误表示确认失败，或重投退避被 ctx 中断。
func consumeOne(ctx context.Context, d amqp.Delivery, handler MessageHandler, backoff requeueBackoff) error {
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
		// 瞬时失败：退避后重投，避免故障或熔断打开时形成热循环。
		if err := backoff(ctx); err != nil {
			// ctx 取消时保留未确认状态；关闭 channel 后 broker 自动重投。
			return err
		}
		if err := d.Nack(false, true); err != nil {
			return fmt.Errorf("MQ Nack 重投: %w", err)
		}
	}
	return nil
}

func waitTransientRequeue(ctx context.Context) error {
	timer := time.NewTimer(transientRequeueDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// declareQueue 声明主队列（含死信配置）与死信队列，声明幂等（已存在则 no-op）。
// 主队列 x-dead-letter-exchange 为空串 = 默认交换机，路由键指向死信队列名。
func declareQueue(ch amqpChannel, queue string) error {
	if strings.HasSuffix(queue, dlqSuffix) {
		if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
			return fmt.Errorf("MQ 声明死信队列 %s: %w", queue, err)
		}
		return nil
	}
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
