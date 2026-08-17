package service

import (
	"context"
	"errors"
	"testing"
	"time"

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
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9, EndAt: time.Now().Add(time.Hour)}
	require.NoError(t, activities.Create(context.Background(), activity))
	paidOrder, pendingOrder := "paid-1", "pending-1"
	paid := &model.PreDeduction{UserID: 1, ActivityID: activity.ID, OrderNo: &paidOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	pending := &model.PreDeduction{UserID: 2, ActivityID: activity.ID, OrderNo: &pendingOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	require.NoError(t, pdRepo.Create(context.Background(), paid))
	require.NoError(t, pdRepo.Create(context.Background(), pending))
	cache.stock[stockKey(activity.ID)] = 9
	cache.count[countKey(activity.ID, 1)] = 1
	cache.count[countKey(activity.ID, 2)] = 1
	cache.reservations[reservationKey(paid.ID)] = paid.ReservationToken()
	cache.reservations[reservationKey(pending.ID)] = pending.ReservationToken()
	cleanup := NewReservationCleanup(activities, pdRepo, cache, fakeSeckillOrderStatuses{
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

func TestReservationCleanupRepairsOrderedReservationAfterAOFReplay(t *testing.T) {
	pdRepo := newFakePreDeductions()
	cache := newFakeCache()
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9, EndAt: time.Now().Add(time.Hour)}
	require.NoError(t, activities.Create(context.Background(), activity))
	orderNo := "pending-aof"
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: activity.ID, OrderNo: &orderNo, Quantity: 1,
		Status: model.PreDeductionStatusOrdered,
	}
	require.NoError(t, pdRepo.Create(context.Background(), pd))
	cleanup := NewReservationCleanup(activities, pdRepo, cache,
		fakeSeckillOrderStatuses{orderNo: ordermodel.OrderStatusPendingPayment})

	cleaned, err := cleanup.CleanupOrderedReservations(context.Background())
	require.NoError(t, err)
	require.Zero(t, cleaned)
	require.Equal(t, 9, cache.stock[stockKey(activity.ID)], "ordered MySQL stock is the Redis baseline")
	require.Equal(t, 1, cache.count[countKey(activity.ID, 42)])
	require.Equal(t, pd.ReservationToken(), cache.reservations[reservationKey(pd.ID)])
	stored, err := pdRepo.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Nil(t, stored.ReservationReleasedAt, "pending-payment orders keep their durable reservation")
}

func TestReservationCleanupDoesNotReleaseWhenAOFConfirmationFails(t *testing.T) {
	pdRepo := newFakePreDeductions()
	cache := newFakeCache()
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9, EndAt: time.Now().Add(time.Hour)}
	require.NoError(t, activities.Create(context.Background(), activity))
	orderNo := "paid-aof-timeout"
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: activity.ID, OrderNo: &orderNo, Quantity: 1,
		Status: model.PreDeductionStatusOrdered,
	}
	require.NoError(t, pdRepo.Create(context.Background(), pd))
	cache.aofErr = errors.New("WAITAOF timeout")
	cleanup := NewReservationCleanup(activities, pdRepo, cache,
		fakeSeckillOrderStatuses{orderNo: ordermodel.OrderStatusPaid})

	_, err := cleanup.CleanupOrderedReservations(context.Background())
	require.ErrorContains(t, err, "WAITAOF")
	stored, getErr := pdRepo.GetByID(context.Background(), pd.ID)
	require.NoError(t, getErr)
	require.Nil(t, stored.ReservationReleasedAt)
}

func TestReservationCleanupDatabaseFailureCannotDoubleCount(t *testing.T) {
	pdRepo := newFakePreDeductions()
	cache := newFakeCache()
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9, EndAt: time.Now().Add(time.Hour)}
	require.NoError(t, activities.Create(context.Background(), activity))
	orderNo := "paid-db-failure"
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: activity.ID, OrderNo: &orderNo, Quantity: 1,
		Status: model.PreDeductionStatusOrdered,
	}
	require.NoError(t, pdRepo.Create(context.Background(), pd))
	cache.stock[stockKey(activity.ID)] = 9
	cache.count[countKey(activity.ID, 42)] = 1
	cache.reservations[reservationKey(pd.ID)] = pd.ReservationToken()
	pdRepo.markReleasedError = errors.New("mysql down")
	cleanup := NewReservationCleanup(activities, pdRepo, cache,
		fakeSeckillOrderStatuses{orderNo: ordermodel.OrderStatusPaid})

	_, err := cleanup.CleanupOrderedReservations(context.Background())
	require.ErrorContains(t, err, "mysql down")
	require.Equal(t, pd.ReservationToken(), cache.reservations[reservationKey(pd.ID)],
		"marker must remain until the release fact commits")
	pdRepo.markReleasedError = nil
	cleaned, err := cleanup.CleanupOrderedReservations(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, cleaned)
	require.Equal(t, 1, cache.count[countKey(activity.ID, 42)], "retry must not increment count twice")
}
