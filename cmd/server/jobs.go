package main

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
)

func recoverFlashSalePeriodically(ctx context.Context, recovery flashsalesvc.PreDeductionRecovery,
	cleanup flashsalesvc.ReservationCleanup, gate flashsalesvc.PurchaseRecoveryGate,
) (flashsalesvc.RecoveryStats, int, error) {
	// Keep the gate closed across stock/reservation recovery and ordered marker
	// cleanup so no purchase can enter between those phases.
	gate.BlockPurchases()
	stats, err := recovery.RecoverPreDeductions(ctx)
	if err != nil {
		return stats, 0, err
	}
	if stats.Failed > 0 {
		return stats, 0, fmt.Errorf("%d flash-sale pre-deductions failed recovery", stats.Failed)
	}
	cleaned, err := cleanup.CleanupOrderedReservations(ctx)
	if err != nil {
		return stats, cleaned, err
	}
	gate.AllowPurchases()
	return stats, cleaned, nil
}

// registerCron 注册全部定时任务并返回调度器（Start 由调用方执行）。
func registerCron(log *zap.Logger, orderSvc ordersvc.Service, recovery flashsalesvc.PreDeductionRecovery,
	recoveryGate flashsalesvc.PurchaseRecoveryGate,
	reservationCleanup flashsalesvc.ReservationCleanup,
	uploadRecovery interface {
		ReconcilePendingUploads(context.Context) (int, error)
	},
	seckillCancellation flashsalesvc.SeckillCancellation,
	reconcile flashsalesvc.Reconciliation) (*platformcron.Registry, []platformcron.Job, error) {
	registry := platformcron.New(log, 5*time.Minute)
	jobs := make([]platformcron.Job, 0, 6)
	register := func(job platformcron.Job) error {
		if err := registry.Register(job); err != nil {
			return err
		}
		jobs = append(jobs, job)
		return nil
	}
	if err := register(platformcron.Job{
		Name: "order-timeout-cancel",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			cancelled, failed, err := orderSvc.CancelExpired(ctx)
			if err != nil {
				return err
			}
			log.Info("订单超时取消完成", zap.Int("cancelled", cancelled), zap.Int("failed", failed))
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("注册超时取消任务: %w", err)
	}
	if err := register(platformcron.Job{
		Name: "flashsale-recovery",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			stats, cleaned, err := recoverFlashSalePeriodically(ctx, recovery, reservationCleanup, recoveryGate)
			if err != nil {
				return err
			}
			if stats.Published+stats.RolledBack+stats.Failed > 0 {
				log.Info("秒杀预扣恢复完成", zap.Int("published", stats.Published),
					zap.Int("rolled_back", stats.RolledBack), zap.Int("failed", stats.Failed))
			}
			if cleaned > 0 {
				log.Info("秒杀 reservation marker 清理完成", zap.Int("cleaned", cleaned))
			}
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("注册秒杀预扣恢复任务: %w", err)
	}
	if err := register(platformcron.Job{
		Name: "upload-reservation-recovery",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			resolved, err := uploadRecovery.ReconcilePendingUploads(ctx)
			if err != nil {
				return err
			}
			if resolved > 0 {
				log.Warn("未完成上传对账已清理", zap.Int("resolved", resolved))
			}
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("注册未完成上传对账任务: %w", err)
	}
	if err := register(platformcron.Job{
		Name: "seckill-timeout-cancel",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			cancelled, failed, redisFailed, err := seckillCancellation.CancelExpired(ctx)
			if err != nil {
				return err
			}
			log.Info("秒杀订单超时取消完成",
				zap.Int("cancelled", cancelled), zap.Int("failed", failed), zap.Int("redis_failed", redisFailed))
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("注册秒杀超时取消任务: %w", err)
	}
	if err := register(platformcron.Job{
		Name: "flashsale-reconcile-active",
		Spec: "0 * * * *",
		Fn: func(ctx context.Context) error {
			warnings, err := reconcile.ReconcileActive(ctx)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				log.Warn("秒杀库存对账差异（进行中，仅告警不写回）",
					zap.Int64("pre_deduction_id", w.PreDeductionID), zap.Int64("user_id", w.UserID),
					zap.String("order_no", w.OrderNo), zap.String("status", w.Status),
					zap.Int64("activity_id", w.ActivityID), zap.String("title", w.Title),
					zap.Int("redis_stock", w.RedisStock), zap.Int("mysql_stock", w.MySQLStock),
					zap.Int("order_count", w.OrderCount), zap.String("detail", w.Detail))
			}
			if len(warnings) == 0 {
				log.Info("秒杀库存对账无差异")
			}
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("注册秒杀对账任务: %w", err)
	}
	if err := register(platformcron.Job{
		Name: "flashsale-reconcile-ended",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			aligned, err := reconcile.ReconcileEnded(ctx)
			if err != nil {
				return err
			}
			if aligned > 0 {
				log.Warn("秒杀收尾对账已对齐", zap.Int("aligned", aligned))
			}
			return nil
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("注册秒杀收尾对账任务: %w", err)
	}
	return registry, jobs, nil
}
