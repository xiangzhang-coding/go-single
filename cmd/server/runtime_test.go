package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
)

type blockingRuntimeMQ struct {
	started chan string
}

func (*blockingRuntimeMQ) Ping(context.Context) error                    { return nil }
func (*blockingRuntimeMQ) Close() error                                  { return nil }
func (*blockingRuntimeMQ) Publish(context.Context, string, []byte) error { return nil }
func (m *blockingRuntimeMQ) Consume(ctx context.Context, queue string, _ mq.MessageHandler) error {
	m.started <- queue
	<-ctx.Done()
	return ctx.Err()
}

type runtimeRecovery struct{}

func (runtimeRecovery) RecoverPreDeductions(context.Context) (flashsalesvc.RecoveryStats, error) {
	return flashsalesvc.RecoveryStats{}, nil
}

func (runtimeRecovery) RecoverPreDeductionsAtStartup(context.Context) (flashsalesvc.RecoveryStats, error) {
	return flashsalesvc.RecoveryStats{}, nil
}

type runtimeRecoveryGate struct {
	blocked    bool
	allowCalls int
	blockCalls int
}

func (g *runtimeRecoveryGate) BlockPurchases() {
	g.blocked = true
	g.blockCalls++
}
func (g *runtimeRecoveryGate) AllowPurchases() {
	g.blocked = false
	g.allowCalls++
}
func (g *runtimeRecoveryGate) PurchasesBlocked() bool { return g.blocked }

type runtimeCleanup struct{}

func (runtimeCleanup) CleanupOrderedReservations(context.Context) (int, error) { return 0, nil }

type failedRuntimeCleanup struct{}

func (failedRuntimeCleanup) CleanupOrderedReservations(context.Context) (int, error) {
	return 0, errors.New("ordered reservation repair failed")
}

type configurableRuntimeRecovery struct {
	failed int
	err    error
}

func (r *configurableRuntimeRecovery) RecoverPreDeductions(context.Context) (flashsalesvc.RecoveryStats, error) {
	return flashsalesvc.RecoveryStats{Failed: r.failed}, r.err
}

func (r *configurableRuntimeRecovery) RecoverPreDeductionsAtStartup(context.Context) (flashsalesvc.RecoveryStats, error) {
	return r.RecoverPreDeductions(context.Background())
}

type configurableRuntimeCleanup struct{ err error }

func (c *configurableRuntimeCleanup) CleanupOrderedReservations(context.Context) (int, error) {
	return 0, c.err
}

type gateObservingRecovery struct {
	gate             *runtimeRecoveryGate
	blockedDuringRun bool
}

func (r *gateObservingRecovery) RecoverPreDeductions(context.Context) (flashsalesvc.RecoveryStats, error) {
	r.blockedDuringRun = r.gate.PurchasesBlocked()
	return flashsalesvc.RecoveryStats{}, nil
}

func (r *gateObservingRecovery) RecoverPreDeductionsAtStartup(ctx context.Context) (flashsalesvc.RecoveryStats, error) {
	return r.RecoverPreDeductions(ctx)
}

func TestApplicationRuntimeStopsConsumersAndCron(t *testing.T) {
	client := &blockingRuntimeMQ{started: make(chan string, 2)}
	runtime := &applicationRuntime{
		log: zap.NewNop(), mq: client, cron: platformcron.New(zap.NewNop(), time.Second),
		recovery: runtimeRecovery{}, reservationCleanup: runtimeCleanup{},
		consumers: []consumerBinding{
			{queue: "main", name: "main", handler: func(context.Context, []byte) error { return nil }},
			{queue: "dead", name: "dead", handler: func(context.Context, []byte) error { return nil }},
		},
	}

	runtime.Start()
	require.ElementsMatch(t, []string{"main", "dead"}, []string{<-client.started, <-client.started})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, runtime.Stop(ctx))
}

func TestWaitForServerStopReturnsListenerError(t *testing.T) {
	want := errors.New("address already in use")
	quit := make(chan os.Signal)
	serverErr := make(chan error, 1)
	serverErr <- want

	require.ErrorIs(t, waitForServerStop(quit, serverErr), want)
}

type failedRuntimeRecovery struct{}

func (failedRuntimeRecovery) RecoverPreDeductions(context.Context) (flashsalesvc.RecoveryStats, error) {
	return flashsalesvc.RecoveryStats{Failed: 1}, nil
}

func (failedRuntimeRecovery) RecoverPreDeductionsAtStartup(context.Context) (flashsalesvc.RecoveryStats, error) {
	return flashsalesvc.RecoveryStats{Failed: 1}, nil
}

func TestApplicationRuntimeBlocksPurchasesWhenStartupRecoveryIsIncomplete(t *testing.T) {
	gate := &runtimeRecoveryGate{}
	runtime := &applicationRuntime{
		log: zap.NewNop(), mq: &blockingRuntimeMQ{started: make(chan string)},
		cron: platformcron.New(zap.NewNop(), time.Second), recovery: failedRuntimeRecovery{},
		reservationCleanup: runtimeCleanup{}, recoveryGate: gate,
	}

	runtime.Start()
	require.True(t, gate.PurchasesBlocked())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, runtime.Stop(ctx))
}

func TestApplicationRuntimeBlocksPurchasesWhenOrderedReservationRepairFails(t *testing.T) {
	gate := &runtimeRecoveryGate{}
	runtime := &applicationRuntime{
		log: zap.NewNop(), mq: &blockingRuntimeMQ{started: make(chan string)},
		cron: platformcron.New(zap.NewNop(), time.Second), recovery: runtimeRecovery{},
		reservationCleanup: failedRuntimeCleanup{}, recoveryGate: gate,
	}

	runtime.Start()
	require.True(t, gate.PurchasesBlocked())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, runtime.Stop(ctx))
}

func TestPeriodicFlashSaleRecoveryReopensGateOnlyAfterAllRepairsSucceed(t *testing.T) {
	tests := []struct {
		name     string
		recovery *configurableRuntimeRecovery
		cleanup  *configurableRuntimeCleanup
	}{
		{name: "recovery error", recovery: &configurableRuntimeRecovery{err: errors.New("mysql down")}, cleanup: &configurableRuntimeCleanup{}},
		{name: "partial recovery", recovery: &configurableRuntimeRecovery{failed: 1}, cleanup: &configurableRuntimeCleanup{}},
		{name: "cleanup error", recovery: &configurableRuntimeRecovery{}, cleanup: &configurableRuntimeCleanup{err: errors.New("ordered reservation repair failed")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gate := &runtimeRecoveryGate{}
			_, _, err := recoverFlashSalePeriodically(context.Background(), tc.recovery, tc.cleanup, gate)
			require.Error(t, err)
			require.True(t, gate.PurchasesBlocked())
			require.Equal(t, 1, gate.blockCalls)
			require.Zero(t, gate.allowCalls, "失败周期中不得短暂开放抢购")
		})
	}

	gate := &runtimeRecoveryGate{blocked: true}
	_, _, err := recoverFlashSalePeriodically(
		context.Background(), &configurableRuntimeRecovery{}, &configurableRuntimeCleanup{}, gate,
	)
	require.NoError(t, err)
	require.False(t, gate.PurchasesBlocked())
	require.Equal(t, 1, gate.allowCalls)
}

func TestPeriodicFlashSaleRecoveryClosesGateBeforeRepair(t *testing.T) {
	gate := &runtimeRecoveryGate{}
	recovery := &gateObservingRecovery{gate: gate}

	_, _, err := recoverFlashSalePeriodically(context.Background(), recovery, runtimeCleanup{}, gate)

	require.NoError(t, err)
	require.True(t, recovery.blockedDuringRun)
	require.False(t, gate.PurchasesBlocked())
}
