package service

import (
	"context"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
)

type SeckillOrderStatusReader interface {
	SeckillOrderStatus(ctx context.Context, orderNo string) (status string, found bool, err error)
}

type ReservationCleanup interface {
	CleanupOrderedReservations(ctx context.Context) (int, error)
}

type reservationMarkerCache interface {
	Del(ctx context.Context, key string) error
	EnsureOrderedFlashSaleReservationDurably(ctx context.Context, p cache.FlashSaleEnsureOrderedReservationParams, timeout time.Duration) (cache.FlashSaleEnsureReservationResult, error)
}

type reservationCleanupService struct {
	activities    repository.ActivityRepository
	preDeductions repository.PreDeductionRepository
	cache         reservationMarkerCache
	orders        SeckillOrderStatusReader
}

func NewReservationCleanup(activities repository.ActivityRepository,
	preDeductions repository.PreDeductionRepository, cache reservationMarkerCache,
	orders SeckillOrderStatusReader) ReservationCleanup {
	return &reservationCleanupService{
		activities: activities, preDeductions: preDeductions, cache: cache, orders: orders,
	}
}

func (s *reservationCleanupService) CleanupOrderedReservations(ctx context.Context) (int, error) {
	rows, err := s.preDeductions.ListOrdered(ctx, 0)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return cleaned, err
		}
		orderNo := rows[i].OrderNumber()
		if orderNo == "" {
			continue
		}
		if !rows[i].Legacy {
			activity, err := s.activities.GetByID(ctx, rows[i].ActivityID)
			if err != nil {
				return cleaned, err
			}
			if activity == nil {
				continue
			}
			stockTTL := remainingTTL(activity)
			if stockTTL <= 0 {
				stockTTL = stockKeyMargin
			}
			_, err = s.cache.EnsureOrderedFlashSaleReservationDurably(ctx, cache.FlashSaleEnsureOrderedReservationParams{
				StockKey: stockKey(activity.ID), CountKey: countKey(rows[i].ActivityID, rows[i].UserID),
				IdempotencyKey: preDeductionIdemKey(&rows[i]),
				ReservationKey: reservationKey(rows[i].ID), ReservationToken: rows[i].ReservationToken(),
				IdempotencyTTL: idemTTL, Quantity: rows[i].Quantity,
				FallbackStock: activity.Stock, StockTTL: stockTTL,
			}, redisAOFTimeout)
			if err != nil {
				return cleaned, err
			}
		}
		status, found, err := s.orders.SeckillOrderStatus(ctx, orderNo)
		if err != nil {
			return cleaned, err
		}
		if !found {
			continue
		}
		switch status {
		case ordermodel.OrderStatusPaid, ordermodel.OrderStatusShipped, ordermodel.OrderStatusCompleted:
		default:
			continue
		}
		if err := s.preDeductions.MarkReservationReleased(ctx, rows[i].ID); err != nil {
			return cleaned, err
		}
		// Persist release before deleting the marker. A crash can leave one
		// harmless marker behind, but can no longer make the next run INCR count twice.
		_ = s.cache.Del(ctx, reservationKey(rows[i].ID))
		cleaned++
	}
	return cleaned, nil
}
