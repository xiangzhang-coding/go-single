package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
)

type fakeSeckillTx struct{}

func (fakeSeckillTx) WithinTx(_ context.Context, fn func(*gorm.DB) error) error {
	return fn(nil)
}

type fakeSeckillCancellations struct {
	expired   []ordersvc.ExpiredSeckillOrder
	cancelled []string
	changed   map[string]bool
	listErr   error
}

func (f *fakeSeckillCancellations) ListExpiredSeckill(context.Context) ([]ordersvc.ExpiredSeckillOrder, error) {
	return f.expired, f.listErr
}

func (f *fakeSeckillCancellations) CancelSeckill(_ context.Context, _ *gorm.DB, orderNo string) (bool, error) {
	f.cancelled = append(f.cancelled, orderNo)
	return !f.changed[orderNo], nil
}

type fakeSeckillRedisRestorer struct {
	restored []string
	err      error
}

func (f *fakeSeckillRedisRestorer) RestoreRedis(_ context.Context, activityID, userID int64, quantity int) error {
	f.restored = append(f.restored, formatRestore(activityID, userID, quantity))
	return f.err
}

func TestSeckillTimeoutCountsRedisFailureAfterDatabaseCommit(t *testing.T) {
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 4}
	require.NoError(t, activities.Create(context.Background(), activity))
	orders := &fakeSeckillCancellations{expired: []ordersvc.ExpiredSeckillOrder{{
		OrderNo: "S1", UserID: 42, ActivityID: activity.ID, Quantity: 1,
	}}}
	redis := &fakeSeckillRedisRestorer{err: errors.New("redis down")}
	timeout := NewSeckillTimeout(fakeSeckillTx{}, orders, activities, redis, metrics.New().Business())

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Equal(t, 1, cancelled)
	require.Zero(t, failed)
	require.Equal(t, 1, redisFailed)
	require.Equal(t, 5, activity.Stock, "Redis 故障不得回滚已提交的 MySQL 回补")
}

func TestSeckillTimeoutSkipsChangedAndMalformedOrders(t *testing.T) {
	activities := newFakeActivities()
	activity := &model.Activity{Stock: 4}
	require.NoError(t, activities.Create(context.Background(), activity))
	orders := &fakeSeckillCancellations{
		expired: []ordersvc.ExpiredSeckillOrder{
			{OrderNo: "changed", UserID: 42, ActivityID: activity.ID, Quantity: 1},
			{OrderNo: "malformed", UserID: 43, Quantity: 1},
		},
		changed: map[string]bool{"changed": true},
	}
	redis := &fakeSeckillRedisRestorer{}
	timeout := NewSeckillTimeout(fakeSeckillTx{}, orders, activities, redis, metrics.New().Business())

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Zero(t, cancelled)
	require.Equal(t, 2, failed)
	require.Zero(t, redisFailed)
	require.Equal(t, 4, activity.Stock)
	require.Empty(t, redis.restored)
}

func TestSeckillTimeoutPropagatesScanFailure(t *testing.T) {
	timeout := NewSeckillTimeout(
		fakeSeckillTx{},
		&fakeSeckillCancellations{listErr: errors.New("mysql down")},
		newFakeActivities(),
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
	orders := &fakeSeckillCancellations{expired: []ordersvc.ExpiredSeckillOrder{{
		OrderNo: "S1", UserID: 42, ActivityID: activity.ID, Quantity: 1,
	}}}
	redis := &fakeSeckillRedisRestorer{}
	timeout := NewSeckillTimeout(fakeSeckillTx{}, orders, activities, redis, metrics.New().Business())

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Equal(t, 1, cancelled)
	require.Zero(t, failed)
	require.Zero(t, redisFailed)
	require.Equal(t, []string{"S1"}, orders.cancelled)
	require.Equal(t, 10, activity.Stock)
	require.Equal(t, []string{"1:42:1"}, redis.restored)
}
