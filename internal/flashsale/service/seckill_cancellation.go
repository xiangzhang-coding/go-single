package service

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// SeckillCancellation 负责用户主动取消与超时取消的跨模块应用编排。
type SeckillCancellation interface {
	CancelExpired(ctx context.Context) (cancelled, failed, redisFailed int, err error)
	Cancel(ctx context.Context, userID int64, orderNo string) error
}

type seckillTxRunner interface {
	WithinTx(ctx context.Context, fn func(tx *transaction.Handle) error) error
}

type seckillCancellationOrders interface {
	ListExpiredSeckill(ctx context.Context) ([]ordersvc.SeckillCancellationOrder, error)
	CancelSeckill(ctx context.Context, tx *transaction.Handle, orderNo string) (bool, error)
	SeckillCancellation(ctx context.Context, userID int64, orderNo string) (*ordersvc.SeckillCancellationOrder, error)
}

type preDeductionRecoverer interface {
	RecoverPreDeduction(ctx context.Context, id int64) error
}

type seckillCancellationService struct {
	tx            seckillTxRunner
	orders        seckillCancellationOrders
	activities    repository.ActivityRepository
	preDeductions repository.PreDeductionRepository
	recovery      preDeductionRecoverer
	metrics       *metrics.Business
}

// NewSeckillCancellation 将 order 的状态迁移与 flashsale 的库存回补组合成单向
// flashsale -> order 编排；两个业务模块不再互相持有实例。
func NewSeckillCancellation(tx seckillTxRunner, orders seckillCancellationOrders,
	activities repository.ActivityRepository, preDeductions repository.PreDeductionRepository,
	recovery preDeductionRecoverer,
	m *metrics.Business) SeckillCancellation {
	return &seckillCancellationService{
		tx: tx, orders: orders, activities: activities,
		preDeductions: preDeductions, recovery: recovery, metrics: m,
	}
}

func (s *seckillCancellationService) CancelExpired(ctx context.Context) (cancelled, failed, redisFailed int, err error) {
	orders, err := s.orders.ListExpiredSeckill(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, order := range orders {
		if order.ActivityID <= 0 || order.Quantity < 1 {
			failed++
			continue
		}
		redisErr, cancelErr := s.cancel(ctx, order)
		if cancelErr != nil {
			failed++
			continue
		}
		cancelled++
		if redisErr {
			redisFailed++
		}
	}
	return cancelled, failed, redisFailed, nil
}

func (s *seckillCancellationService) Cancel(ctx context.Context, userID int64, orderNo string) error {
	order, err := s.orders.SeckillCancellation(ctx, userID, orderNo)
	if err != nil {
		return err
	}
	_, err = s.cancel(ctx, *order)
	return err
}

func (s *seckillCancellationService) cancel(ctx context.Context, order ordersvc.SeckillCancellationOrder) (bool, error) {
	if order.ActivityID <= 0 || order.Quantity < 1 {
		return false, ordersvc.ErrInvalidInput
	}
	var pdID int64
	err := s.tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		ok, err := s.orders.CancelSeckill(ctx, tx, order.OrderNo)
		if err != nil {
			return err
		}
		if !ok {
			return ordersvc.ErrOrderChanged
		}
		if err := s.activities.RestoreStock(ctx, tx, order.ActivityID, order.Quantity); err != nil {
			return err
		}
		orderNo := order.OrderNo
		pd, err := s.preDeductions.EnsurePendingRollback(ctx, tx, &model.PreDeduction{
			UserID: order.UserID, ActivityID: order.ActivityID,
			ClientRequestID: "legacy-cancel:" + order.OrderNo, OrderNo: &orderNo,
			SKUID: order.SKUID, Price: order.Price, Quantity: order.Quantity,
			PurchaseSlot: order.PurchaseSlot, LastError: "seckill order cancelled",
		})
		if err != nil {
			return err
		}
		pdID = pd.ID
		return nil
	})
	if err != nil {
		return false, err
	}
	s.metrics.OrderStatusChanged(ordermodel.OrderStatusCancelled)
	return s.recovery.RecoverPreDeduction(ctx, pdID) != nil, nil
}
