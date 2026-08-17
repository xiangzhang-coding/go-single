package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
)

type fakeSeckillOrderStatuses map[string]string

func (f fakeSeckillOrderStatuses) SeckillOrderStatus(_ context.Context, orderNo string) (string, bool, error) {
	status, ok := f[orderNo]
	return status, ok, nil
}

func TestReservationCleanupReleasesOnlyOrdersThatCannotBeCancelled(t *testing.T) {
	pdRepo := newFakePreDeductions()
	cache := newFakeCache()
	paidOrder, pendingOrder := "paid-1", "pending-1"
	paid := &model.PreDeduction{UserID: 1, ActivityID: 10, OrderNo: &paidOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	pending := &model.PreDeduction{UserID: 2, ActivityID: 10, OrderNo: &pendingOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	require.NoError(t, pdRepo.Create(context.Background(), paid))
	require.NoError(t, pdRepo.Create(context.Background(), pending))
	cache.reservations[reservationKey(paid.ID)] = paid.ReservationToken()
	cache.reservations[reservationKey(pending.ID)] = pending.ReservationToken()
	cleanup := NewReservationCleanup(pdRepo, cache, fakeSeckillOrderStatuses{
		paidOrder: ordermodel.OrderStatusPaid, pendingOrder: ordermodel.OrderStatusPendingPayment,
	})

	cleaned, err := cleanup.CleanupOrderedReservations(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, cleaned)
	require.NotContains(t, cache.reservations, reservationKey(paid.ID))
	require.Contains(t, cache.reservations, reservationKey(pending.ID))
	stored, err := pdRepo.GetByID(context.Background(), paid.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.ReservationReleasedAt)
}
