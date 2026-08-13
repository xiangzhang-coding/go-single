// 业务指标集合（T19c）：秒杀/订单/支付/MQ/优惠券指标点，
// 经 Registry.Business 构建并注册到同一 registry，随 /metrics 抓取。
// 命名与 Grafana 大盘（deploy/monitoring/grafana/dashboards/business.json）逐项对应。
package metrics

import (
	"context"
	"errors"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
)

// 指标标签取值（result/reason/order_type/status 与大盘面板表达式对应）。
const (
	resultSuccess = "success"
	resultFail    = "fail"

	reasonPermanent = "permanent" // 消费失败进死信（Nack 拒收）
	reasonTransient = "transient" // 消费失败重投（Nack requeue）
)

// Business 业务指标集合：各模块服务经方法打点（进程内调用），
// 全部 collector 注册于创建它的 Registry。
type Business struct {
	seckillPreDeduct *prometheus.CounterVec // seckill_prededuct_total{result}
	seckillStock     *prometheus.GaugeVec   // seckill_stock_remaining{activity_id}

	ordersCreated *prometheus.CounterVec // orders_created_total{order_type}
	ordersStatus  *prometheus.CounterVec // orders_status_total{status}
	payments      *prometheus.CounterVec // orders_payment_total{result}

	mqPublished     *prometheus.CounterVec // mq_published_total{queue,result}
	mqConsumed      *prometheus.CounterVec // mq_consumed_total{queue}
	mqConsumeFailed *prometheus.CounterVec // mq_consume_failed_total{queue,reason}

	couponIssued   prometheus.Counter // coupon_issued_total
	couponRedeemed prometheus.Counter // coupon_redeemed_total
}

// Business 构建并注册全部业务指标到本 registry（重复调用对同一 registry 会 panic）。
func (r *Registry) Business() *Business {
	b := &Business{
		seckillPreDeduct: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seckill_prededuct_total",
			Help: "秒杀 Lua 原子预扣次数，按 result（success/fail）分桶；失败含业务拒绝与基础设施故障",
		}, []string{"result"}),
		seckillStock: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "seckill_stock_remaining",
			Help: "秒杀活动 Redis 预扣库存余量（gauge），按 activity_id 分桶；随秒杀页浏览/上架预热/下架更新",
		}, []string{"activity_id"}),
		ordersCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_created_total",
			Help: "订单创建数（幂等命中不重复计数），按 order_type（normal/seckill）分桶",
		}, []string{"order_type"}),
		ordersStatus: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_status_total",
			Help: "订单进入各状态的累计次数（创建计入 pending_payment），按 status 分桶",
		}, []string{"status"}),
		payments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "orders_payment_total",
			Help: "模拟支付回调处理数（流水落库后计数），按 result（success/fail）分桶",
		}, []string{"result"}),
		mqPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mq_published_total",
			Help: "MQ 发布次数，按 queue 与 result（success/fail）分桶",
		}, []string{"queue", "result"}),
		mqConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mq_consumed_total",
			Help: "MQ 消费消息数（handler 被调用即计数），按 queue 分桶",
		}, []string{"queue"}),
		mqConsumeFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mq_consume_failed_total",
			Help: "MQ 消费失败数，按 queue 与 reason（permanent 死信/transient 重投）分桶",
		}, []string{"queue", "reason"}),
		couponIssued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coupon_issued_total",
			Help: "优惠券发放（领取成功落库）累计数",
		}),
		couponRedeemed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coupon_redeemed_total",
			Help: "优惠券核销（unused→used 条件更新成功）累计数；下单事务回滚可能乐观多计",
		}),
	}
	r.reg.MustRegister(
		b.seckillPreDeduct, b.seckillStock,
		b.ordersCreated, b.ordersStatus, b.payments,
		b.mqPublished, b.mqConsumed, b.mqConsumeFailed,
		b.couponIssued, b.couponRedeemed,
	)
	return b
}

// ---- 秒杀 ----

// SeckillPreDeduct 记录一次预扣尝试结果（success/fail）。
func (b *Business) SeckillPreDeduct(success bool) {
	b.incResult(b.seckillPreDeduct, success)
}

// SetSeckillStock 设置活动库存余量（gauge）；remaining 为当前 Redis 预扣余量。
func (b *Business) SetSeckillStock(activityID int64, remaining int) {
	b.seckillStock.WithLabelValues(strconv.FormatInt(activityID, 10)).Set(float64(remaining))
}

// DeleteSeckillStock 删除活动的库存余量序列（与 Redis key 生命周期一致，下架时调用）。
func (b *Business) DeleteSeckillStock(activityID int64) {
	b.seckillStock.DeleteLabelValues(strconv.FormatInt(activityID, 10))
}

// ---- 订单 / 支付 ----

// OrderCreated 记录一次订单创建（幂等命中不计数）。
func (b *Business) OrderCreated(orderType string) {
	b.ordersCreated.WithLabelValues(orderType).Inc()
}

// OrderStatusChanged 记录订单进入某状态的累计次数。
func (b *Business) OrderStatusChanged(status string) {
	b.ordersStatus.WithLabelValues(status).Inc()
}

// PaymentResult 记录一次支付回调处理结果（success/fail）。
func (b *Business) PaymentResult(success bool) {
	b.incResult(b.payments, success)
}

// ---- MQ ----

// MQPublish 记录一次发布结果（success/fail）。
func (b *Business) MQPublish(queue string, success bool) {
	b.incResult(b.mqPublished, success, queue)
}

// MQConsumed 记录消费到一条消息（handler 被调用即计数）。
func (b *Business) MQConsumed(queue string) {
	b.mqConsumed.WithLabelValues(queue).Inc()
}

// MQConsumeFailed 记录一次消费失败（permanent 进死信 / transient 重投）。
func (b *Business) MQConsumeFailed(queue string, permanent bool) {
	if permanent {
		b.mqConsumeFailed.WithLabelValues(queue, reasonPermanent).Inc()
		return
	}
	b.mqConsumeFailed.WithLabelValues(queue, reasonTransient).Inc()
}

// ---- 优惠券 ----

// CouponIssued 记录一次优惠券发放（领取成功落库）。
func (b *Business) CouponIssued() {
	b.couponIssued.Inc()
}

// CouponRedeemed 记录一次优惠券核销（条件更新成功；下单事务回滚可能乐观多计）。
func (b *Business) CouponRedeemed() {
	b.couponRedeemed.Inc()
}

// incResult 布尔结果计数分桶到 success/fail 标签（非成功即失败）。
func (b *Business) incResult(vec *prometheus.CounterVec, success bool, labels ...string) {
	result := resultFail
	if success {
		result = resultSuccess
	}
	vec.WithLabelValues(append(labels, result)...).Inc()
}

// ---- MQ 装饰器 ----

// WrapMQ 装饰 MQ 客户端：Publish 按 queue 记录成功/失败；
// Consume 包装 handler 记录消费数与消费失败（按 ErrPermanent 区分死信/重投）。
// 与 mq 层保持透明（Ping/Close 直通），main 装配处整体包裹一次即可。
func WrapMQ(inner mq.MQ, b *Business) mq.MQ {
	return &mqMetrics{inner: inner, b: b}
}

type mqMetrics struct {
	inner mq.MQ
	b     *Business
}

func (m *mqMetrics) Ping(ctx context.Context) error { return m.inner.Ping(ctx) }

func (m *mqMetrics) Close() error { return m.inner.Close() }

func (m *mqMetrics) Publish(ctx context.Context, queue string, body []byte) error {
	err := m.inner.Publish(ctx, queue, body)
	m.b.MQPublish(queue, err == nil)
	return err
}

// Consume 包装每条消息的 handler：消费即计数，失败按永久/瞬时分类计数。
func (m *mqMetrics) Consume(ctx context.Context, queue string, handler mq.MessageHandler) error {
	wrapped := func(hctx context.Context, body []byte) error {
		m.b.MQConsumed(queue)
		err := handler(hctx, body)
		if err != nil {
			m.b.MQConsumeFailed(queue, errors.Is(err, mq.ErrPermanent))
		}
		return err
	}
	return m.inner.Consume(ctx, queue, wrapped)
}
