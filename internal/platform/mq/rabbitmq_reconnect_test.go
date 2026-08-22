package mq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

type fakeDialer struct {
	mu      sync.Mutex
	results []fakeDialResult
	calls   int
	urls    []string
	configs []amqp.Config
}

type fakeDialResult struct {
	conn amqpConnection
	err  error
}

func (d *fakeDialer) dial(_ context.Context, url string, config amqp.Config) (amqpConnection, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.urls = append(d.urls, url)
	d.configs = append(d.configs, config)
	if len(d.results) == 0 {
		return nil, errors.New("unexpected dial")
	}
	result := d.results[0]
	d.results = d.results[1:]
	return result.conn, result.err
}

func (d *fakeDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *fakeDialer) inputs() ([]string, []amqp.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.urls...), append([]amqp.Config(nil), d.configs...)
}

type fakeConnection struct {
	mu      sync.Mutex
	closed  bool
	channel amqpChannel
}

type blockingStage string

const (
	blockChannel      blockingStage = "channel"
	blockQos          blockingStage = "qos"
	blockQueueDeclare blockingStage = "queue-declare"
	blockConfirm      blockingStage = "confirm"
	blockPublish      blockingStage = "publish"
	blockConsume      blockingStage = "consume"
)

type blockingConnection struct {
	blockAt   blockingStage
	closed    chan struct{}
	started   chan struct{}
	closeOnce sync.Once
	startOnce sync.Once
	channel   *blockingChannel
}

func newBlockingConnection(blockAt blockingStage) *blockingConnection {
	conn := &blockingConnection{
		blockAt: blockAt,
		closed:  make(chan struct{}),
		started: make(chan struct{}),
	}
	conn.channel = &blockingChannel{conn: conn}
	return conn
}

func (c *blockingConnection) wait(blockAt blockingStage) error {
	if c.blockAt != blockAt {
		return nil
	}
	c.startOnce.Do(func() { close(c.started) })
	<-c.closed
	return amqp.ErrClosed
}

func (c *blockingConnection) Channel() (amqpChannel, error) {
	if err := c.wait(blockChannel); err != nil {
		return nil, err
	}
	if c.IsClosed() {
		return nil, amqp.ErrClosed
	}
	return c.channel, nil
}

func (c *blockingConnection) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *blockingConnection) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

type blockingChannel struct {
	conn *blockingConnection
}

func (c *blockingChannel) Close() error { return nil }

func (c *blockingChannel) Qos(int, int, bool) error {
	return c.conn.wait(blockQos)
}

func (c *blockingChannel) QueueDeclare(name string, _, _, _, _ bool, _ amqp.Table) (amqp.Queue, error) {
	if err := c.conn.wait(blockQueueDeclare); err != nil {
		return amqp.Queue{}, err
	}
	return amqp.Queue{Name: name}, nil
}

func (c *blockingChannel) Confirm(bool) error {
	return c.conn.wait(blockConfirm)
}

func (c *blockingChannel) PublishWithDeferredConfirm(string, string, bool, bool, amqp.Publishing) (deferredConfirmation, error) {
	if err := c.conn.wait(blockPublish); err != nil {
		return nil, err
	}
	return &fakeConfirmation{acked: true}, nil
}

func (c *blockingChannel) Consume(string, string, bool, bool, bool, bool, amqp.Table) (<-chan amqp.Delivery, error) {
	if err := c.conn.wait(blockConsume); err != nil {
		return nil, err
	}
	return make(chan amqp.Delivery), nil
}

func (c *fakeConnection) Channel() (amqpChannel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, amqp.ErrClosed
	}
	return c.channel, nil
}

func (c *fakeConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConnection) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConnection) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

type fakeConfirmation struct {
	acked bool
	err   error
}

func (c *fakeConfirmation) WaitContext(context.Context) (bool, error) {
	return c.acked, c.err
}

type fakeChannel struct {
	mu             sync.Mutex
	declaredQueues []fakeQueueDeclaration
	published      []amqp.Publishing
	confirmCalls   int
	confirmation   deferredConfirmation
	deliveries     <-chan amqp.Delivery
	qosCalls       int
	consumeAutoAck []bool
}

type fakeQueueDeclaration struct {
	name    string
	durable bool
	args    amqp.Table
}

func (c *fakeChannel) Close() error { return nil }

func (c *fakeChannel) Qos(prefetchCount, prefetchSize int, global bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prefetchCount == 1 && prefetchSize == 0 && !global {
		c.qosCalls++
	}
	return nil
}

func (c *fakeChannel) QueueDeclare(name string, durable, _, _, _ bool, args amqp.Table) (amqp.Queue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.declaredQueues = append(c.declaredQueues, fakeQueueDeclaration{name: name, durable: durable, args: args})
	return amqp.Queue{Name: name}, nil
}

func (c *fakeChannel) Confirm(bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.confirmCalls++
	return nil
}

func (c *fakeChannel) PublishWithDeferredConfirm(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) (deferredConfirmation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.published = append(c.published, msg)
	return c.confirmation, nil
}

func (c *fakeChannel) Consume(_ string, _ string, autoAck, _ bool, _ bool, _ bool, _ amqp.Table) (<-chan amqp.Delivery, error) {
	c.mu.Lock()
	c.consumeAutoAck = append(c.consumeAutoAck, autoAck)
	c.mu.Unlock()
	return c.deliveries, nil
}

type ackCounter struct {
	mu       sync.Mutex
	ackCalls int
}

func (a *ackCounter) Ack(uint64, bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ackCalls++
	return nil
}

func (a *ackCounter) Nack(uint64, bool, bool) error { return nil }
func (a *ackCounter) Reject(uint64, bool) error     { return nil }

func (a *ackCounter) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ackCalls
}

func requireHandled(t *testing.T, handled <-chan string, want string) {
	t.Helper()
	select {
	case got := <-handled:
		require.Equal(t, want, got)
	case <-time.After(time.Second):
		t.Fatalf("未收到消息 %q", want)
	}
}

func TestRabbitMQSetupHonorsContextDeadline(t *testing.T) {
	tests := []struct {
		name           string
		blockAt        blockingStage
		run            func(MQ, context.Context) error
		wantContextErr bool
	}{
		{
			name:    "ping channel",
			blockAt: blockChannel,
			run: func(client MQ, ctx context.Context) error {
				return client.Ping(ctx)
			},
			wantContextErr: true,
		},
		{
			name:    "publish queue declare",
			blockAt: blockQueueDeclare,
			run: func(client MQ, ctx context.Context) error {
				return client.Publish(ctx, "orders", []byte("order-1"))
			},
			wantContextErr: true,
		},
		{
			name:    "publish confirm",
			blockAt: blockConfirm,
			run: func(client MQ, ctx context.Context) error {
				return client.Publish(ctx, "orders", []byte("order-1"))
			},
			wantContextErr: true,
		},
		{
			name:    "publish write",
			blockAt: blockPublish,
			run: func(client MQ, ctx context.Context) error {
				return client.Publish(ctx, "orders", []byte("order-1"))
			},
			wantContextErr: true,
		},
		{
			name:    "consume qos",
			blockAt: blockQos,
			run: func(client MQ, ctx context.Context) error {
				return client.Consume(ctx, "orders", func(context.Context, []byte) error { return nil })
			},
		},
		{
			name:    "consume queue declare",
			blockAt: blockQueueDeclare,
			run: func(client MQ, ctx context.Context) error {
				return client.Consume(ctx, "orders", func(context.Context, []byte) error { return nil })
			},
		},
		{
			name:    "consume start",
			blockAt: blockConsume,
			run: func(client MQ, ctx context.Context) error {
				return client.Consume(ctx, "orders", func(context.Context, []byte) error { return nil })
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := newBlockingConnection(tc.blockAt)
			dialer := &fakeDialer{results: []fakeDialResult{{conn: conn}}}
			client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.run(client, ctx) }()

			select {
			case <-conn.started:
			case <-time.After(time.Second):
				t.Fatal("调用未进入预期的 AMQP 阻塞阶段")
			}

			select {
			case err := <-done:
				if tc.wantContextErr {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.NoError(t, err, "Consume 在 context 到期时应优雅退出")
				}
				require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
				require.True(t, conn.IsClosed(), "context 到期应关闭阻塞调用所在的 connection")
			case <-time.After(time.Second):
				_ = client.Close()
				<-done
				t.Fatal("context 到期后 AMQP 同步调用仍阻塞")
			}
		})
	}
}

func TestRabbitMQCloseUnblocksSetup(t *testing.T) {
	conn := newBlockingConnection(blockQueueDeclare)
	dialer := &fakeDialer{results: []fakeDialResult{{conn: conn}}}
	client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- client.Publish(context.Background(), "orders", []byte("order-1"))
	}()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("Publish 未进入 AMQP setup 阻塞阶段")
	}

	require.NoError(t, client.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close 未解阻正在执行的 AMQP setup")
	}
}

func TestRabbitMQPublishReconnectsAfterConnectionCloses(t *testing.T) {
	oldConn := &fakeConnection{}
	newChannel := &fakeChannel{confirmation: &fakeConfirmation{acked: true}}
	newConn := &fakeConnection{channel: newChannel}
	dialer := &fakeDialer{results: []fakeDialResult{
		{conn: oldConn},
		{err: errors.New("broker is restarting")},
		{conn: newConn},
	}}

	client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	oldConn.disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, client.Publish(ctx, "orders", []byte("order-1")))
	require.Equal(t, 3, dialer.callCount())
	urls, configs := dialer.inputs()
	require.Equal(t, []string{"amqp://rabbit/", "amqp://rabbit/", "amqp://rabbit/"}, urls)
	require.Equal(t, 10*time.Second, configs[2].Heartbeat)

	newChannel.mu.Lock()
	defer newChannel.mu.Unlock()
	require.Equal(t, 1, newChannel.confirmCalls, "重连后仍应启用 publisher confirm")
	require.Len(t, newChannel.published, 1)
	require.Equal(t, uint8(amqp.Persistent), newChannel.published[0].DeliveryMode)
	require.Equal(t, []byte("order-1"), newChannel.published[0].Body)
	require.Len(t, newChannel.declaredQueues, 2)
	require.True(t, newChannel.declaredQueues[0].durable, "死信队列应持久化")
	require.True(t, newChannel.declaredQueues[1].durable, "主队列应持久化")
}

func TestRabbitMQConsumeReconnectsAndContinues(t *testing.T) {
	oldDeliveries := make(chan amqp.Delivery, 1)
	newDeliveries := make(chan amqp.Delivery, 1)
	oldChannel := &fakeChannel{deliveries: oldDeliveries}
	newChannel := &fakeChannel{deliveries: newDeliveries}
	oldConn := &fakeConnection{channel: oldChannel}
	newConn := &fakeConnection{channel: newChannel}
	dialer := &fakeDialer{results: []fakeDialResult{{conn: oldConn}, {conn: newConn}}}
	client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	consumeDone := make(chan error, 1)
	handled := make(chan string, 2)
	go func() {
		consumeDone <- client.Consume(ctx, "orders", func(_ context.Context, body []byte) error {
			handled <- string(body)
			return nil
		})
	}()

	firstAck := &ackCounter{}
	oldDeliveries <- amqp.Delivery{Acknowledger: firstAck, DeliveryTag: 1, Body: []byte("before-restart")}
	requireHandled(t, handled, "before-restart")
	require.Eventually(t, func() bool { return firstAck.calls() == 1 }, time.Second, time.Millisecond)

	oldConn.disconnect()
	close(oldDeliveries)
	require.Eventually(t, func() bool { return dialer.callCount() == 2 }, time.Second, time.Millisecond)

	secondAck := &ackCounter{}
	newDeliveries <- amqp.Delivery{Acknowledger: secondAck, DeliveryTag: 2, Body: []byte("after-restart")}
	requireHandled(t, handled, "after-restart")
	require.Eventually(t, func() bool { return secondAck.calls() == 1 }, time.Second, time.Millisecond)

	cancel()
	select {
	case err := <-consumeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("取消 context 后 Consume 未退出")
	}

	newChannel.mu.Lock()
	defer newChannel.mu.Unlock()
	require.Equal(t, 1, newChannel.qosCalls, "重连后仍应设置 prefetch=1")
	require.Equal(t, []bool{false}, newChannel.consumeAutoAck, "重连后仍应使用 manual ack")
	require.Len(t, newChannel.declaredQueues, 2)
	require.True(t, newChannel.declaredQueues[0].durable)
	require.True(t, newChannel.declaredQueues[1].durable)
}

func TestRabbitMQReconnectHonorsContextWithBoundedBackoff(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(MQ, context.Context) error
	}{
		{name: "ping", run: func(client MQ, ctx context.Context) error { return client.Ping(ctx) }},
		{name: "publish", run: func(client MQ, ctx context.Context) error {
			return client.Publish(ctx, "orders", []byte("order-1"))
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			oldConn := &fakeConnection{}
			dialer := &fakeDialer{results: []fakeDialResult{
				{conn: oldConn},
				{err: errors.New("broker unavailable")},
			}}
			client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			oldConn.disconnect()

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			err = operation.run(client, ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.GreaterOrEqual(t, dialer.callCount(), 2)
			require.Less(t, dialer.callCount(), 10, "重拨必须退避，不能形成忙循环")
		})
	}
}

func TestRabbitMQConsumeReconnectHonorsContext(t *testing.T) {
	oldConn := &fakeConnection{}
	dialer := &fakeDialer{results: []fakeDialResult{
		{conn: oldConn},
		{err: errors.New("broker unavailable")},
	}}
	client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	oldConn.disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	require.NoError(t, client.Consume(ctx, "orders", func(context.Context, []byte) error { return nil }))
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.GreaterOrEqual(t, dialer.callCount(), 2)
	require.Less(t, dialer.callCount(), 10, "消费重拨必须退避，不能形成忙循环")
}

func TestRabbitMQCloseCancelsReconnectAndPreventsFutureDial(t *testing.T) {
	oldConn := &fakeConnection{}
	reconnectStarted := make(chan struct{})
	var (
		dialMu    sync.Mutex
		dialCalls int
	)
	dial := func(ctx context.Context, _ string, _ amqp.Config) (amqpConnection, error) {
		dialMu.Lock()
		dialCalls++
		call := dialCalls
		dialMu.Unlock()
		if call == 1 {
			return oldConn, nil
		}
		if call == 2 {
			close(reconnectStarted)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	client, err := newRabbitMQWithDialer("amqp://rabbit/", dial)
	require.NoError(t, err)
	oldConn.disconnect()

	pingDone := make(chan error, 1)
	go func() { pingDone <- client.Ping(context.Background()) }()
	select {
	case <-reconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("Ping 未开始重拨")
	}

	require.NoError(t, client.Close())
	select {
	case err := <-pingDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close 后正在重拨的 Ping 未退出")
	}

	require.Error(t, client.Ping(context.Background()))
	require.Error(t, client.Publish(context.Background(), "orders", []byte("order-1")))
	require.Error(t, client.Consume(context.Background(), "orders", func(context.Context, []byte) error { return nil }))
	dialMu.Lock()
	defer dialMu.Unlock()
	require.Equal(t, 2, dialCalls, "Close 后不得再次拨号")
}

func TestRabbitMQConcurrentPingSharesReconnect(t *testing.T) {
	oldConn := &fakeConnection{}
	newConn := &fakeConnection{channel: &fakeChannel{}}
	dialer := &fakeDialer{results: []fakeDialResult{{conn: oldConn}, {conn: newConn}}}
	client, err := newRabbitMQWithDialer("amqp://rabbit/", dialer.dial)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	oldConn.disconnect()

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			errs <- client.Ping(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 2, dialer.callCount(), "并发调用应共享一次重拨")
}
