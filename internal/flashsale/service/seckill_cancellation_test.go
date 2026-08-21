package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

type fakeSeckillTx struct{}

func (fakeSeckillTx) WithinTx(_ context.Context, fn func(*transaction.Handle) error) error {
	return fn(nil)
}

type fakeSeckillCancellations struct {
	expired   []ordersvc.SeckillCancellationOrder
	cancelled []string
	changed   map[string]bool
	listErr   error
}

func (f *fakeSeckillCancellations) ListExpiredSeckill(context.Context) ([]ordersvc.SeckillCancellationOrder, error) {
	return f.expired, f.listErr
}

func (f *fakeSeckillCancellations) CancelSeckill(_ context.Context, _ *transaction.Handle, orderNo string) (bool, error) {
	f.cancelled = append(f.cancelled, orderNo)
	return !f.changed[orderNo], nil
}

func (f *fakeSeckillCancellations) SeckillCancellation(_ context.Context, userID int64, orderNo string) (*ordersvc.SeckillCancellationOrder, error) {
	for i := range f.expired {
		if f.expired[i].OrderNo == orderNo {
			if f.expired[i].UserID != userID {
				return nil, ordersvc.ErrOrderForbidden
			}
			copy := f.expired[i]
			return &copy, nil
		}
	}
	return nil, ordersvc.ErrOrderNotFound
}

type fakeSeckillRedisRestorer struct {
	restored []string
	err      error
}

func (f *fakeSeckillRedisRestorer) RecoverPreDeduction(_ context.Context, id int64) error {
	f.restored = append(f.restored, fmt.Sprint(id))
	return f.err
}

func TestSeckillTimeoutCountsRedisFailureAfterDatabaseCommit(t *testing.T) {
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 4}
	require.NoError(t, activities.Create(context.Background(), activity))
	orders := &fakeSeckillCancellations{expired: []ordersvc.SeckillCancellationOrder{{
		OrderNo: "S1", UserID: 42, ActivityID: activity.ID, Quantity: 1,
	}}}
	redis := &fakeSeckillRedisRestorer{err: errors.New("redis down")}
	pd := newFakePreDeductions()
	timeout := NewSeckillCancellation(fakeSeckillTx{}, orders, activities, pd, redis, metrics.New().Business())

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Equal(t, 1, cancelled)
	require.Zero(t, failed)
	require.Equal(t, 1, redisFailed)
	require.Equal(t, 5, activity.Stock, "Redis 故障不得回滚已提交的 MySQL 回补")
	rows, listErr := pd.ListRecoverable(context.Background(), 10)
	require.NoError(t, listErr)
	require.Len(t, rows, 1)
	require.Equal(t, model.PreDeductionStatusPendingRollback, rows[0].Status,
		"Redis failure must leave durable compensation work")
}

func TestSeckillTimeoutSkipsChangedAndMalformedOrders(t *testing.T) {
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 4}
	require.NoError(t, activities.Create(context.Background(), activity))
	orders := &fakeSeckillCancellations{
		expired: []ordersvc.SeckillCancellationOrder{
			{OrderNo: "changed", UserID: 42, ActivityID: activity.ID, Quantity: 1},
			{OrderNo: "malformed", UserID: 43, Quantity: 1},
		},
		changed: map[string]bool{"changed": true},
	}
	redis := &fakeSeckillRedisRestorer{}
	timeout := NewSeckillCancellation(fakeSeckillTx{}, orders, activities, newFakePreDeductions(), redis, metrics.New().Business())

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Zero(t, cancelled)
	require.Equal(t, 2, failed)
	require.Zero(t, redisFailed)
	require.Equal(t, 4, activity.Stock)
	require.Empty(t, redis.restored)
}

func TestSeckillTimeoutPropagatesScanFailure(t *testing.T) {
	timeout := NewSeckillCancellation(
		fakeSeckillTx{},
		&fakeSeckillCancellations{listErr: errors.New("mysql down")},
		newFakeActivities(),
		newFakePreDeductions(),
		&fakeSeckillRedisRestorer{},
		metrics.New().Business(),
	)

	_, _, _, err := timeout.CancelExpired(context.Background())

	require.ErrorContains(t, err, "mysql down")
}

func formatRestore(activityID, userID int64, quantity int) string {
	return fmt.Sprintf("%d:%d:%d", activityID, userID, quantity)
}

func TestSeckillTimeoutRestoresActivityStockAndRedis(t *testing.T) {
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9}
	require.NoError(t, activities.Create(context.Background(), activity))
	orders := &fakeSeckillCancellations{expired: []ordersvc.SeckillCancellationOrder{{
		OrderNo: "S1", UserID: 42, ActivityID: activity.ID, Quantity: 1,
	}}}
	redis := &fakeSeckillRedisRestorer{}
	timeout := NewSeckillCancellation(fakeSeckillTx{}, orders, activities, newFakePreDeductions(), redis, metrics.New().Business())

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Equal(t, 1, cancelled)
	require.Zero(t, failed)
	require.Zero(t, redisFailed)
	require.Equal(t, []string{"S1"}, orders.cancelled)
	require.Equal(t, 10, activity.Stock)
	require.Len(t, redis.restored, 1)
}

func TestUserCanCancelOwnedPendingSeckillOrder(t *testing.T) {
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 9}
	require.NoError(t, activities.Create(context.Background(), activity))
	orders := &fakeSeckillCancellations{expired: []ordersvc.SeckillCancellationOrder{{
		OrderNo: "S1", UserID: 42, ActivityID: activity.ID, SKUID: 7, Price: 100, Quantity: 1, PurchaseSlot: 99,
	}}}
	redis := &fakeSeckillRedisRestorer{}
	pd := newFakePreDeductions()
	canceller := NewSeckillCancellation(fakeSeckillTx{}, orders, activities, pd, redis, metrics.New().Business())

	require.NoError(t, canceller.Cancel(context.Background(), 42, "S1"))
	require.Equal(t, []string{"S1"}, orders.cancelled)
	require.Equal(t, 10, activity.Stock)
	require.Len(t, redis.restored, 1)
}
