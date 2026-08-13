// 业务指标单元测试（T19c）：黑盒验证 Business 各打点方法写入 /metrics 抓取结果，
// 以及 WrapMQ 装饰器对发布/消费/消费失败的分桶计数（不依赖 MySQL/Redis/RabbitMQ）。
package metrics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
)

// fakeMQ 记录调用参数的 MQ 替身（Publish 可注入错误）。
type fakeMQ struct {
	publishErr error
	published  []string // 队列名序列
}

func (f *fakeMQ) Ping(context.Context) error { return nil }

func (f *fakeMQ) Close() error { return nil }

func (f *fakeMQ) Publish(_ context.Context, queue string, _ []byte) error {
	f.published = append(f.published, queue)
	return f.publishErr
}

// Consume 同步执行一次 handler 后返回（模拟消费单条消息）。
func (f *fakeMQ) Consume(_ context.Context, queue string, handler mq.MessageHandler) error {
	return handler(context.Background(), []byte("{}"))
}

func TestBusinessMetricsRecordedOnScrape(t *testing.T) {
	reg := metrics.New()
	biz := reg.Business()

	// 秒杀：预扣成功 2 次、失败 1 次；库存余量 gauge。
	biz.SeckillPreDeduct(true)
	biz.SeckillPreDeduct(true)
	biz.SeckillPreDeduct(false)
	biz.SetSeckillStock(101, 50)

	// 订单/支付：创建 + 状态流转 + 支付成功/失败。
	biz.OrderCreated("normal")
	biz.OrderCreated("seckill")
	biz.OrderStatusChanged("pending_payment")
	biz.OrderStatusChanged("pending_payment")
	biz.OrderStatusChanged("paid")
	biz.PaymentResult(true)
	biz.PaymentResult(false)

	// MQ：发布成功 1、失败 1；消费 2；消费失败 permanent 1、transient 1。
	biz.MQPublish("flashsale.order.create", true)
	biz.MQPublish("flashsale.order.create", false)
	biz.MQConsumed("flashsale.order.create")
	biz.MQConsumed("flashsale.order.create")
	biz.MQConsumeFailed("flashsale.order.create", true)
	biz.MQConsumeFailed("flashsale.order.create", false)

	// 优惠券：发放 1、核销 1。
	biz.CouponIssued()
	biz.CouponRedeemed()

	families := scrape(t, reg.Handler())

	cases := []struct {
		name    string
		labels  map[string]string
		wantVal float64
	}{
		{"seckill_prededuct_total", map[string]string{"result": "success"}, 2},
		{"seckill_prededuct_total", map[string]string{"result": "fail"}, 1},
		{"seckill_stock_remaining", map[string]string{"activity_id": "101"}, 50},
		{"orders_created_total", map[string]string{"order_type": "normal"}, 1},
		{"orders_created_total", map[string]string{"order_type": "seckill"}, 1},
		{"orders_status_total", map[string]string{"status": "pending_payment"}, 2},
		{"orders_status_total", map[string]string{"status": "paid"}, 1},
		{"orders_payment_total", map[string]string{"result": "success"}, 1},
		{"orders_payment_total", map[string]string{"result": "fail"}, 1},
		{"mq_published_total", map[string]string{"queue": "flashsale.order.create", "result": "success"}, 1},
		{"mq_published_total", map[string]string{"queue": "flashsale.order.create", "result": "fail"}, 1},
		{"mq_consumed_total", map[string]string{"queue": "flashsale.order.create"}, 2},
		{"mq_consume_failed_total", map[string]string{"queue": "flashsale.order.create", "reason": "permanent"}, 1},
		{"mq_consume_failed_total", map[string]string{"queue": "flashsale.order.create", "reason": "transient"}, 1},
		{"coupon_issued_total", nil, 1},
		{"coupon_redeemed_total", nil, 1},
	}
	for _, tc := range cases {
		v, ok := findSample(t, families, tc.name, tc.labels)
		require.Truef(t, ok, "%s{%v} 缺失", tc.name, tc.labels)
		require.Equalf(t, tc.wantVal, v, "%s{%v} 值不符", tc.name, tc.labels)
	}
}

func TestDeleteSeckillStockRemovesSeries(t *testing.T) {
	reg := metrics.New()
	biz := reg.Business()

	biz.SetSeckillStock(7, 30)
	biz.DeleteSeckillStock(7)

	families := scrape(t, reg.Handler())
	_, ok := findSample(t, families, "seckill_stock_remaining", map[string]string{"activity_id": "7"})
	require.False(t, ok, "下架后 gauge 序列应被删除")
}

func TestWrapMQCountsPublishAndConsume(t *testing.T) {
	reg := metrics.New()
	biz := reg.Business()
	inner := &fakeMQ{}
	wrapped := metrics.WrapMQ(inner, biz)

	// 发布成功 + 发布失败。
	require.NoError(t, wrapped.Publish(context.Background(), "q1", nil))
	inner.publishErr = errors.New("broker down")
	require.Error(t, wrapped.Publish(context.Background(), "q1", nil))
	require.Equal(t, []string{"q1", "q1"}, inner.published)

	// 消费：成功 1 次 + 瞬时失败 1 次 + 永久失败 1 次。
	okHandler := func(context.Context, []byte) error { return nil }
	transientHandler := func(context.Context, []byte) error { return errors.New("db timeout") }
	permanentHandler := func(context.Context, []byte) error {
		return errors.Join(mq.ErrPermanent, errors.New("invalid message"))
	}
	require.NoError(t, wrapped.Consume(context.Background(), "q1", okHandler))
	require.Error(t, wrapped.Consume(context.Background(), "q1", transientHandler))
	require.Error(t, wrapped.Consume(context.Background(), "q1", permanentHandler))

	families := scrape(t, reg.Handler())
	v, ok := findSample(t, families, "mq_published_total", map[string]string{"queue": "q1", "result": "success"})
	require.True(t, ok, "mq_published_total{q1,success} 缺失")
	require.Equal(t, float64(1), v)
	v, ok = findSample(t, families, "mq_published_total", map[string]string{"queue": "q1", "result": "fail"})
	require.True(t, ok, "mq_published_total{q1,fail} 缺失")
	require.Equal(t, float64(1), v)
	v, ok = findSample(t, families, "mq_consumed_total", map[string]string{"queue": "q1"})
	require.True(t, ok, "mq_consumed_total{q1} 缺失")
	require.Equal(t, float64(3), v)
	v, ok = findSample(t, families, "mq_consume_failed_total", map[string]string{"queue": "q1", "reason": "transient"})
	require.True(t, ok, "transient 分类缺失")
	require.Equal(t, float64(1), v)
	v, ok = findSample(t, families, "mq_consume_failed_total", map[string]string{"queue": "q1", "reason": "permanent"})
	require.True(t, ok, "permanent 分类缺失")
	require.Equal(t, float64(1), v)
}

func TestWrapMQDelegatesPassthroughMethods(t *testing.T) {
	inner := &fakeMQ{}
	wrapped := metrics.WrapMQ(inner, metrics.New().Business())

	require.NoError(t, wrapped.Ping(context.Background()))
	require.NoError(t, wrapped.Close())
}
