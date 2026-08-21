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

func TestReservationCleanupKeepsPerUserMarkersUntilActivityEnds(t *testing.T) {
	pdRepo := newFakePreDeductions()
	cache := newFakeCache()
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9, EndAt: time.Now().Add(time.Hour)}
	require.NoError(t, activities.Create(context.Background(), activity))
	paidOrder, shippedOrder, completedOrder, pendingOrder := "paid-1", "shipped-1", "completed-1", "pending-1"
	paid := &model.PreDeduction{UserID: 1, ActivityID: activity.ID, OrderNo: &paidOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	pending := &model.PreDeduction{UserID: 2, ActivityID: activity.ID, OrderNo: &pendingOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	shipped := &model.PreDeduction{UserID: 3, ActivityID: activity.ID, OrderNo: &shippedOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	completed := &model.PreDeduction{UserID: 4, ActivityID: activity.ID, OrderNo: &completedOrder, Quantity: 1, Status: model.PreDeductionStatusOrdered}
	require.NoError(t, pdRepo.Create(context.Background(), paid))
	require.NoError(t, pdRepo.Create(context.Background(), pending))
	require.NoError(t, pdRepo.Create(context.Background(), shipped))
	require.NoError(t, pdRepo.Create(context.Background(), completed))
	cleanup := NewReservationCleanup(activities, pdRepo, cache, fakeSeckillOrderStatuses{
		paidOrder: ordermodel.OrderStatusPaid, shippedOrder: ordermodel.OrderStatusShipped,
		completedOrder: ordermodel.OrderStatusCompleted, pendingOrder: ordermodel.OrderStatusPendingPayment,
	})

	cleaned, err := cleanup.CleanupOrderedReservations(context.Background())
	require.NoError(t, err)
	require.Zero(t, cleaned)
	for _, pd := range []*model.PreDeduction{paid, pending, shipped, completed} {
		require.Contains(t, cache.reservations, reservationKey(pd.ID))
		require.Equal(t, 1, cache.count[countKey(activity.ID, pd.UserID)])
		stored, getErr := pdRepo.GetByID(context.Background(), pd.ID)
		require.NoError(t, getErr)
		require.Nil(t, stored.ReservationReleasedAt)
	}

	cache.stock = map[string]int{}
	cache.count = map[string]int{}
	cache.idem = map[string]bool{}
	cache.idemToken = map[string]string{}
	cache.reservations = map[string]string{}
	cleaned, err = cleanup.CleanupOrderedReservations(context.Background())
	require.NoError(t, err)
	require.Zero(t, cleaned)
	for _, pd := range []*model.PreDeduction{paid, shipped, completed} {
		require.Equal(t, pd.ReservationToken(), cache.reservations[reservationKey(pd.ID)],
			"non-cancellable order must still participate in Redis reconstruction before activity end")
		require.Equal(t, 1, cache.count[countKey(activity.ID, pd.UserID)])
	}
}

func TestReservationCleanupReleasesNonCancellableOrdersAfterActivityEnds(t *testing.T) {
	pdRepo := newFakePreDeductions()
	cache := newFakeCache()
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 7, EndAt: time.Now().Add(-time.Minute)}
	require.NoError(t, activities.Create(context.Background(), activity))
	statuses := []string{
		ordermodel.OrderStatusPaid,
		ordermodel.OrderStatusShipped,
		ordermodel.OrderStatusCompleted,
	}
	orders := make(fakeSeckillOrderStatuses, len(statuses))
	rows := make([]*model.PreDeduction, 0, len(statuses))
	for i, status := range statuses {
		orderNo := status + "-ended"
		pd := &model.PreDeduction{
			UserID: int64(i + 1), ActivityID: activity.ID, OrderNo: &orderNo, Quantity: 1,
			Status: model.PreDeductionStatusOrdered,
		}
		require.NoError(t, pdRepo.Create(context.Background(), pd))
		cache.reservations[reservationKey(pd.ID)] = pd.ReservationToken()
		orders[orderNo] = status
		rows = append(rows, pd)
	}
	cleanup := NewReservationCleanup(activities, pdRepo, cache, orders)

	cleaned, err := cleanup.CleanupOrderedReservations(context.Background())
	require.NoError(t, err)
	require.Equal(t, len(statuses), cleaned)
	for _, pd := range rows {
		require.NotContains(t, cache.reservations, reservationKey(pd.ID))
		stored, getErr := pdRepo.GetByID(context.Background(), pd.ID)
		require.NoError(t, getErr)
		require.NotNil(t, stored.ReservationReleasedAt)
	}
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
	activity := &model.Activity{Stock: 9, EndAt: time.Now().Add(-time.Minute)}
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
