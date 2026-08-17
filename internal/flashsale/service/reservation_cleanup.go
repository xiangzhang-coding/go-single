package service

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
)

const reservationCleanupBatch = 100

type SeckillOrderStatusReader interface {
	SeckillOrderStatus(ctx context.Context, orderNo string) (status string, found bool, err error)
}

type ReservationCleanup interface {
	CleanupOrderedReservations(ctx context.Context) (int, error)
}

type reservationMarkerCache interface {
	Del(ctx context.Context, key string) error
}

type reservationCleanupService struct {
	preDeductions repository.PreDeductionRepository
	cache         reservationMarkerCache
	orders        SeckillOrderStatusReader
}

func NewReservationCleanup(preDeductions repository.PreDeductionRepository,
	cache reservationMarkerCache, orders SeckillOrderStatusReader) ReservationCleanup {
	return &reservationCleanupService{preDeductions: preDeductions, cache: cache, orders: orders}
}

func (s *reservationCleanupService) CleanupOrderedReservations(ctx context.Context) (int, error) {
	rows, err := s.preDeductions.ListOrdered(ctx, reservationCleanupBatch)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for i := range rows {
		orderNo := rows[i].OrderNumber()
		if orderNo == "" {
			continue
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
		if err := s.cache.Del(ctx, reservationKey(rows[i].ID)); err != nil {
			return cleaned, err
		}
		if err := s.preDeductions.MarkReservationReleased(ctx, rows[i].ID); err != nil {
			return cleaned, err
		}
		cleaned++
	}
	return cleaned, nil
}
