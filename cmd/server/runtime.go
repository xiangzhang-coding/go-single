package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
)

type consumerBinding struct {
	queue   string
	name    string
	handler mq.MessageHandler
}

type applicationRuntime struct {
	log                *zap.Logger
	mq                 mq.MQ
	cron               *platformcron.Registry
	recovery           flashsalesvc.PreDeductionRecovery
	recoveryGate       flashsalesvc.PurchaseRecoveryGate
	reservationCleanup flashsalesvc.ReservationCleanup
	consumers          []consumerBinding

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (a *applicationRuntime) Start() {
	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Unlock()

	if a.recoveryGate != nil {
		a.recoveryGate.BlockPurchases()
	}
	recoveryComplete := false
	if stats, err := recoverPreDeductionsAtStartup(a.recovery, 10*time.Second); err != nil {
		a.log.Error("秒杀预扣启动恢复失败（定时任务将重试）", zap.Error(err))
	} else if stats.Failed > 0 {
		a.log.Error("秒杀预扣启动恢复不完整，抢购保持关闭（定时任务将重试）",
			zap.Int("published", stats.Published), zap.Int("rolled_back", stats.RolledBack), zap.Int("failed", stats.Failed))
	} else if stats.Published+stats.RolledBack+stats.Failed > 0 {
		a.log.Info("秒杀预扣启动恢复完成", zap.Int("published", stats.Published),
			zap.Int("rolled_back", stats.RolledBack), zap.Int("failed", stats.Failed))
		recoveryComplete = true
	} else {
		recoveryComplete = true
	}
	cleanupComplete := false
	if cleaned, err := cleanupReservationsAtStartup(a.reservationCleanup, 10*time.Second); err != nil {
		a.log.Error("秒杀 ordered reservation 启动修复失败（定时任务将重试）", zap.Error(err))
	} else if cleaned > 0 {
		a.log.Info("秒杀 ordered reservation 启动清理完成", zap.Int("cleaned", cleaned))
		cleanupComplete = true
	} else {
		cleanupComplete = true
	}
	if recoveryComplete && cleanupComplete && a.recoveryGate != nil {
		a.recoveryGate.AllowPurchases()
	}

	for _, binding := range a.consumers {
		binding := binding
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			for {
				err := a.mq.Consume(ctx, binding.queue, binding.handler)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					return
				}
				a.log.Error(binding.name+"中断，3s 后重连", zap.Error(err))
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
			}
		}()
	}
	a.cron.Start()
}

func (a *applicationRuntime) Stop(ctx context.Context) error {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	a.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	cronErr := a.cron.Stop(ctx)
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return cronErr
	case <-ctx.Done():
		return errors.Join(cronErr, ctx.Err())
	}
}

func recoverPreDeductionsAtStartup(recovery flashsalesvc.PreDeductionRecovery, timeout time.Duration) (flashsalesvc.RecoveryStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return recovery.RecoverPreDeductionsAtStartup(ctx)
}

func cleanupReservationsAtStartup(cleanup flashsalesvc.ReservationCleanup, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return cleanup.CleanupOrderedReservations(ctx)
}
