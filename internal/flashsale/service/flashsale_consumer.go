// 秒杀异步落单消费者（T12）：订阅 SeckillOrderQueue 队列处理"抢购成功"消息，
// 落单链路为 查活动 → 查默认地址（固化地址快照）→ order 服务单事务建单 +
// 扣活动库存（(user_id, activity_id) 唯一约束幂等，预扣成功绝不丢单）。
// 失败分类：业务拒绝/数据缺失 = 永久失败（mq.ErrPermanent → 死信队列，对账兜底）；
// 基础设施故障 = 瞬时失败（Nack 重投，at-least-once）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
)

// ErrSeckillStockInsufficient 表示 Redis 已预扣但 MySQL 活动库存不足。
// 该错误不可通过 MQ 重投恢复，消费者将消息送入死信并由对账兜底。
var ErrSeckillStockInsufficient = errors.New("seckill activity stock insufficient")

// OrderService flashsale 消费侧最小接口（跨模块进程内调用，order 服务实现）：
// 异步落单（幂等建单 + 同事务扣活动库存）。
type OrderService interface {
	CreateSeckillInTx(ctx context.Context, tx *gorm.DB, p ordersvc.SeckillCreateParams) (created bool, err error)
}

// UserService 用户模块最小接口：读取默认地址固化为秒杀订单地址快照。
type UserService interface {
	GetDefaultAddress(ctx context.Context, userID int64) (*usermodel.Address, error)
}

// SeckillOrderConsumer 秒杀异步落单消费者（T12）。
// 消费链路：消息 → 查活动（sku/秒杀价）→ 查默认地址 → 开启事务 →
// order.CreateSeckillInTx + 活动库存条件扣减。
// 预扣成功绝不丢单：重复投递/并发消费由 (user_id, activity_id) 唯一约束幂等；
// 失败重投（瞬时）/死信（永久）由平台 mq 层按返回错误分类执行。
type SeckillOrderConsumer struct {
	activities repository.ActivityRepository
	tx         seckillTxRunner
	orders     OrderService
	users      UserService
	metrics    *metrics.Business
	log        *zap.Logger
}

// NewSeckillOrderConsumer 构造秒杀落单消费者。
func NewSeckillOrderConsumer(activities repository.ActivityRepository,
	tx seckillTxRunner, orders OrderService, users UserService, m *metrics.Business,
	log *zap.Logger) *SeckillOrderConsumer {
	return &SeckillOrderConsumer{
		activities: activities, tx: tx, orders: orders, users: users, metrics: m, log: log,
	}
}

// Handle 处理单条"抢购成功"消息（幂等；返回错误即触发 mq 层重投/死信）。
func (c *SeckillOrderConsumer) Handle(ctx context.Context, body []byte) error {
	var msg SeckillSuccessMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return c.permanent(ctx, "unmarshal seckill message", nil, err)
	}
	if msg.OrderNo == "" || msg.UserID <= 0 || msg.ActivityID <= 0 {
		return c.permanent(ctx, "invalid seckill message", &msg, nil)
	}

	// 活动必须仍存在（sku_id/秒杀价作为订单快照来源；活动删除受限外键 RESTRICT）。
	activity, err := c.activities.GetByID(ctx, msg.ActivityID)
	if err != nil {
		c.log.Warn("秒杀落单读取活动失败（瞬时，将重投）",
			zap.String("order_no", msg.OrderNo), zap.Int64("activity_id", msg.ActivityID), zap.Error(err))
		return err // 瞬时：DB 故障重投
	}
	if activity == nil {
		return c.permanent(ctx, "activity not found", &msg, nil)
	}

	// 默认地址固化为地址快照（秒杀下单无选地址步骤；无地址为永久失败，对账兜底）。
	address, err := c.users.GetDefaultAddress(ctx, msg.UserID)
	if err != nil {
		c.log.Warn("秒杀落单读取默认地址失败（瞬时，将重投）",
			zap.String("order_no", msg.OrderNo), zap.Int64("user_id", msg.UserID), zap.Error(err))
		return err // 瞬时：DB 故障重投
	}
	if address == nil {
		return c.permanent(ctx, "user has no default address", &msg, nil)
	}

	created := false
	err = c.tx.WithinTx(ctx, func(tx *gorm.DB) error {
		var err error
		created, err = c.orders.CreateSeckillInTx(ctx, tx, ordersvc.SeckillCreateParams{
			OrderNo:    msg.OrderNo,
			UserID:     msg.UserID,
			ActivityID: msg.ActivityID,
			SKUID:      activity.SKUID,
			Price:      activity.Price,
			Quantity:   1,
			Address:    address,
		})
		if err != nil || !created {
			return err
		}
		ok, err := c.activities.DeductStock(ctx, tx, msg.ActivityID, 1)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSeckillStockInsufficient
		}
		return nil
	})
	if err != nil {
		return c.classifyCreateError(ctx, &msg, err)
	}
	if created {
		c.metrics.OrderCreated(ordermodel.OrderTypeSeckill)
		c.metrics.OrderStatusChanged(ordermodel.OrderStatusPendingPayment)
	}
	return nil
}

// permanent 永久失败：记日志（对账/人工补偿线索）后包装 ErrPermanent（进死信）。
func (c *SeckillOrderConsumer) permanent(ctx context.Context, reason string, msg *SeckillSuccessMessage, cause error) error {
	fields := []zap.Field{zap.String("reason", reason)}
	if msg != nil {
		fields = append(fields, zap.String("order_no", msg.OrderNo),
			zap.Int64("user_id", msg.UserID), zap.Int64("activity_id", msg.ActivityID))
	}
	if cause != nil {
		fields = append(fields, zap.Error(cause))
	}
	c.log.Error("秒杀落单永久失败（进死信，对账兜底）", fields...)
	if cause != nil {
		return fmt.Errorf("%w: %s: %v", mq.ErrPermanent, reason, cause)
	}
	return fmt.Errorf("%w: %s: %+v", mq.ErrPermanent, reason, msg)
}

// classifyCreateError 落单错误分类：
//   - 业务/数据类错误（参数非法、活动库存不足、SKU 缺失/下架）= 永久失败 → 死信；
//   - 其余（DB 连接等基础设施故障）= 瞬时失败 → 重投。
func (c *SeckillOrderConsumer) classifyCreateError(ctx context.Context, msg *SeckillSuccessMessage, err error) error {
	switch {
	case errors.Is(err, ordersvc.ErrInvalidInput),
		errors.Is(err, ErrSeckillStockInsufficient),
		errors.Is(err, ordersvc.ErrSKUNotFound),
		errors.Is(err, ordersvc.ErrSKUUnavailable):
		return c.permanent(ctx, "create seckill order rejected", msg, err)
	}
	c.log.Warn("秒杀落单瞬时失败（将重投）",
		zap.String("order_no", msg.OrderNo), zap.Int64("activity_id", msg.ActivityID), zap.Error(err))
	return err
}
