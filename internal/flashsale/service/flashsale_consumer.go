// 秒杀异步落单消费者（T12）：订阅 SeckillOrderQueue 队列处理"抢购成功"消息，
// 落单链路为 查活动 → 查默认地址（固化地址快照）→ order 服务单事务建单 +
// 扣活动库存（(user_id, activity_id) 唯一约束幂等，预扣成功绝不丢单）。
// 失败分类：业务拒绝/数据缺失 = 永久失败（先持久化回退意图，再进死信队列）；
// 基础设施故障 = 瞬时失败（Nack 重投，at-least-once）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
)

// ErrSeckillStockInsufficient 表示 Redis 已预扣但 MySQL 活动库存不足。
// 该错误不可通过 MQ 重投恢复，消费者将预扣转为待回退后送入死信。
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
// 瞬时失败由 MQ 重投；永久失败与死信由持久生命周期驱动完整回退。
type SeckillOrderConsumer struct {
	activities    repository.ActivityRepository
	preDeductions repository.PreDeductionRepository
	legacyCache   legacyReservationCache
	tx            seckillTxRunner
	orders        OrderService
	users         UserService
	metrics       *metrics.Business
	log           *zap.Logger
}

// NewSeckillOrderConsumer 构造秒杀落单消费者。
func NewSeckillOrderConsumer(activities repository.ActivityRepository, preDeductions repository.PreDeductionRepository,
	legacyCache legacyReservationCache,
	tx seckillTxRunner, orders OrderService, users UserService, m *metrics.Business,
	log *zap.Logger) *SeckillOrderConsumer {
	return &SeckillOrderConsumer{
		activities: activities, preDeductions: preDeductions, legacyCache: legacyCache,
		tx: tx, orders: orders, users: users, metrics: m, log: log,
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
	if msg.PreDeductionID > 0 {
		pd, err := c.preDeductions.GetByID(ctx, msg.PreDeductionID)
		if err != nil {
			return err
		}
		if pd == nil {
			return c.permanent(ctx, "pre-deduction not found", &msg, nil)
		}
		if pd.UserID != msg.UserID || pd.ActivityID != msg.ActivityID || pd.OrderNumber() != msg.OrderNo {
			return c.permanent(ctx, "pre-deduction does not match message", &msg, nil)
		}
		switch pd.Status {
		case model.PreDeductionStatusOrdered,
			model.PreDeductionStatusPendingRollback,
			model.PreDeductionStatusRolledBack:
			return nil
		case model.PreDeductionStatusPreparing:
			return errors.New("pre-deduction is not ready for order creation")
		}
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
	if err := c.adoptLegacyMessage(ctx, &msg, activity); err != nil {
		return err
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
		if msg.PreDeductionID > 0 {
			pd, err := c.preDeductions.GetByIDForUpdate(ctx, tx, msg.PreDeductionID)
			if err != nil {
				return err
			}
			if pd == nil {
				return ErrPreDeductionNotFound
			}
			switch pd.Status {
			case model.PreDeductionStatusOrdered,
				model.PreDeductionStatusPendingRollback,
				model.PreDeductionStatusRolledBack:
				return nil
			case model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder:
			default:
				return fmt.Errorf("pre-deduction cannot create order from status %s", pd.Status)
			}
		}
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
		if err != nil {
			return err
		}
		if created {
			ok, err := c.activities.DeductStock(ctx, tx, msg.ActivityID, 1)
			if err != nil {
				return err
			}
			if !ok {
				return ErrSeckillStockInsufficient
			}
		}
		if msg.PreDeductionID > 0 {
			return c.preDeductions.MarkOrdered(ctx, tx, msg.PreDeductionID)
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
	c.log.Error("秒杀落单永久失败（持久化回退意图后进死信）", fields...)
	if msg != nil && msg.PreDeductionID > 0 && c.preDeductions != nil {
		if err := c.preDeductions.MarkPendingRollback(ctx, nil, msg.PreDeductionID, reason); err != nil {
			return fmt.Errorf("persist permanent seckill failure: %w", err)
		}
	}
	if cause != nil {
		return fmt.Errorf("%w: %s: %v", mq.ErrPermanent, reason, cause)
	}
	return fmt.Errorf("%w: %s: %+v", mq.ErrPermanent, reason, msg)
}

// HandleDeadLetter drains a poison message after the main consumer persisted
// its rollback intent. Reprocessing is idempotent and leaves compensation to
// the same recovery worker used after crashes.
func (c *SeckillOrderConsumer) HandleDeadLetter(ctx context.Context, body []byte) error {
	var msg SeckillSuccessMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.OrderNo == "" || msg.UserID <= 0 || msg.ActivityID <= 0 {
		c.log.Error("无法关联预扣事实的秒杀死信已丢弃", zap.ByteString("body", body), zap.Error(err))
		return nil
	}
	if msg.PreDeductionID <= 0 {
		activity, err := c.activities.GetByID(ctx, msg.ActivityID)
		if err != nil {
			return err
		}
		if activity == nil {
			c.log.Error("无法关联活动的秒杀死信已丢弃", zap.Int64("activity_id", msg.ActivityID))
			return nil
		}
		if err := c.adoptLegacyMessage(ctx, &msg, activity); err != nil {
			return err
		}
	}
	return c.preDeductions.MarkPendingRollback(ctx, nil, msg.PreDeductionID, "message reached dead-letter queue")
}

func (c *SeckillOrderConsumer) adoptLegacyMessage(ctx context.Context, msg *SeckillSuccessMessage, activity *model.Activity) error {
	if msg.PreDeductionID > 0 {
		return nil
	}
	orderNo := msg.OrderNo
	pd, err := c.preDeductions.EnsureLegacyPendingOrder(ctx, &model.PreDeduction{
		UserID: msg.UserID, ActivityID: msg.ActivityID, OrderNo: &orderNo, Quantity: 1,
	})
	if err != nil {
		return fmt.Errorf("adopt legacy seckill message: %w", err)
	}
	msg.PreDeductionID = pd.ID
	if !pd.Legacy {
		return nil
	}
	return adoptLegacyReservation(ctx, c.legacyCache, c.preDeductions, pd, activity)
}

type legacyReservationCache interface {
	AdoptLegacyFlashSaleReservationDurably(ctx context.Context, p cache.FlashSaleAdoptLegacyReservationParams, timeout time.Duration) (cache.FlashSaleEnsureReservationResult, error)
}

func adoptLegacyReservation(ctx context.Context, client legacyReservationCache,
	preDeductions repository.PreDeductionRepository, pd *model.PreDeduction, activity *model.Activity) error {
	stockTTL := remainingTTL(activity)
	if stockTTL <= 0 {
		stockTTL = stockKeyMargin
	}
	pendingQuantity, userQuantity, err := preDeductions.ReservationTargets(ctx, pd.ActivityID, pd.UserID)
	if err != nil {
		return err
	}
	targetStock := activity.Stock - pendingQuantity
	if targetStock < 0 || userQuantity <= 0 {
		return fmt.Errorf("legacy flash-sale reservation targets are invalid: stock=%d count=%d", targetStock, userQuantity)
	}
	_, err = client.AdoptLegacyFlashSaleReservationDurably(ctx, cache.FlashSaleAdoptLegacyReservationParams{
		StockKey: stockKey(pd.ActivityID), CountKey: countKey(pd.ActivityID, pd.UserID),
		IdempotencyKey: idemKey(pd.ActivityID, pd.UserID), ReservationKey: reservationKey(pd.ID),
		ReservationToken: pd.ReservationToken(), IdempotencyTTL: idemTTL,
		TargetStock: targetStock, TargetUserCount: userQuantity, StockTTL: stockTTL,
	}, redisAOFTimeout)
	if err != nil {
		return fmt.Errorf("adopt legacy flash-sale reservation: %w", err)
	}
	return nil
}

// classifyCreateError 落单错误分类：
//   - 业务/数据类错误（参数非法、活动库存不足、SKU 缺失/下架）= 永久失败 → 死信；
//   - 其余（DB 连接等基础设施故障）= 瞬时失败 → 重投。
func (c *SeckillOrderConsumer) classifyCreateError(ctx context.Context, msg *SeckillSuccessMessage, err error) error {
	switch {
	case errors.Is(err, ordersvc.ErrInvalidInput),
		errors.Is(err, ErrSeckillStockInsufficient),
		errors.Is(err, ordersvc.ErrSeckillOrderConflict),
		errors.Is(err, ordersvc.ErrSKUNotFound),
		errors.Is(err, ordersvc.ErrSKUUnavailable):
		return c.permanent(ctx, "create seckill order rejected", msg, err)
	}
	c.log.Warn("秒杀落单瞬时失败（将重投）",
		zap.String("order_no", msg.OrderNo), zap.Int64("activity_id", msg.ActivityID), zap.Error(err))
	return err
}
