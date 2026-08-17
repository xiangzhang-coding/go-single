package service

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
)

// SeckillTimeout 负责秒杀订单超时取消的跨模块应用编排。
type SeckillTimeout interface {
	CancelExpired(ctx context.Context) (cancelled, failed, redisFailed int, err error)
}

type seckillTxRunner interface {
	WithinTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}

type seckillCancellationOrders interface {
	ListExpiredSeckill(ctx context.Context) ([]ordersvc.ExpiredSeckillOrder, error)
	CancelSeckill(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error)
}

type seckillRedisRestorer interface {
	RestoreRedis(ctx context.Context, activityID, userID int64, quantity int) error
}

type seckillTimeoutService struct {
	tx         seckillTxRunner
	orders     seckillCancellationOrders
	activities repository.ActivityRepository
	redis      seckillRedisRestorer
	metrics    *metrics.Business
}

// NewSeckillTimeout 将 order 的状态迁移与 flashsale 的库存回补组合成单向
// flashsale -> order 编排；两个业务模块不再互相持有实例。
func NewSeckillTimeout(tx seckillTxRunner, orders seckillCancellationOrders,
	activities repository.ActivityRepository, redis seckillRedisRestorer,
	m *metrics.Business) SeckillTimeout {
	return &seckillTimeoutService{
		tx: tx, orders: orders, activities: activities, redis: redis, metrics: m,
	}
}

func (s *seckillTimeoutService) CancelExpired(ctx context.Context) (cancelled, failed, redisFailed int, err error) {
	orders, err := s.orders.ListExpiredSeckill(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, order := range orders {
		if order.ActivityID <= 0 || order.Quantity < 1 {
			failed++
			continue
		}
		dbErr := s.tx.WithinTx(ctx, func(tx *gorm.DB) error {
			ok, err := s.orders.CancelSeckill(ctx, tx, order.OrderNo)
			if err != nil {
				return err
			}
			if !ok {
				return ordersvc.ErrOrderChanged
			}
			return s.activities.RestoreStock(ctx, tx, order.ActivityID, order.Quantity)
		})
		if dbErr != nil {
			failed++
			continue
		}
		cancelled++
		s.metrics.OrderStatusChanged(ordermodel.OrderStatusCancelled)
		if err := s.redis.RestoreRedis(ctx, order.ActivityID, order.UserID, order.Quantity); err != nil {
			redisFailed++
		}
	}
	return cancelled, failed, redisFailed, nil
}
