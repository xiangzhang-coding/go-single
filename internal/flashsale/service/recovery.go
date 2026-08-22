package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
)

const preparingRecoveryDelay = 30 * time.Second

func (s *flashsaleService) RecoverPreDeductions(ctx context.Context) (RecoveryStats, error) {
	return s.recoverAll(ctx, false)
}

func (s *flashsaleService) RecoverPreDeductionsAtStartup(ctx context.Context) (RecoveryStats, error) {
	return s.recoverAll(ctx, true)
}

func (s *flashsaleService) recoverAll(ctx context.Context, forcePreparing bool) (RecoveryStats, error) {
	// Stock baselines and every durable reservation form one recovery epoch.
	// Excluding purchases here prevents a request from racing between the two.
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	if err := s.recoverPublishedStocks(ctx); err != nil {
		return RecoveryStats{}, err
	}
	return s.recoverPreDeductions(ctx, forcePreparing)
}

func (s *flashsaleService) recoverPublishedStocks(ctx context.Context) error {
	activities, err := s.store.Activities.List(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	var recoveryErrors []error
	for i := range activities {
		activity := &activities[i]
		if !activity.IsOnSale() || now.After(activity.EndAt) {
			continue
		}
		if err := s.syncStock(ctx, activity, now); err != nil {
			recoveryErrors = append(recoveryErrors,
				fmt.Errorf("recover published flash-sale stock %d: %w", activity.ID, err))
		}
	}
	return errors.Join(recoveryErrors...)
}

func (s *flashsaleService) recoverPreDeductions(ctx context.Context, forcePreparing bool) (RecoveryStats, error) {
	var stats RecoveryStats
	if s.store.PreDeductions == nil {
		return stats, nil
	}
	list, err := s.store.PreDeductions.ListRecoverable(ctx, 0)
	if err != nil {
		return stats, err
	}
	// Rebuild every active reservation before processing rollbacks. If Redis
	// lost an entire unflushed second, the first pass reconstructs the stock
	// baseline and all still-live deductions; missing rollback markers in the
	// second pass can then be treated as already absent without overstocking.
	initialRollback := make([]bool, len(list))
	for i := range list {
		initialRollback[i] = list[i].Status == model.PreDeductionStatusPendingRollback
	}
	for phase := 0; phase < 2; phase++ {
		for i := range list {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			isRollback := initialRollback[i]
			if (phase == 0 && isRollback) || (phase == 1 && !isRollback) {
				continue
			}
			before, after, err := s.recoverPreDeductionByID(ctx, list[i].ID, forcePreparing)
			if err != nil {
				if ctx.Err() != nil {
					return stats, ctx.Err()
				}
				stats.Failed++
				continue
			}
			switch {
			case after == model.PreDeductionStatusPendingOrder && before != model.PreDeductionStatusPendingOrder:
				stats.Published++
			case after == model.PreDeductionStatusRolledBack && before != model.PreDeductionStatusRolledBack:
				stats.RolledBack++
			}
		}
	}
	return stats, nil
}

func (s *flashsaleService) RecoverPreDeduction(ctx context.Context, id int64) error {
	if s.store.PreDeductions == nil {
		return ErrPreDeductionNotFound
	}
	_, _, err := s.recoverPreDeductionByID(ctx, id, false)
	return err
}

func (s *flashsaleService) recoverPreDeductionByID(ctx context.Context, id int64, forcePreparing bool) (model.PreDeductionStatus, model.PreDeductionStatus, error) {
	mu := &s.preDeductionMu[uint64(id)%uint64(len(s.preDeductionMu))]
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	pd, err := s.store.PreDeductions.GetByID(ctx, id)
	if err != nil {
		return "", "", err
	}
	if pd == nil {
		return "", "", ErrPreDeductionNotFound
	}
	before := pd.Status
	recoverErr := s.recoverPreDeductionLocked(ctx, pd, forcePreparing)
	current, getErr := s.store.PreDeductions.GetByID(ctx, id)
	if getErr != nil {
		return before, before, errors.Join(recoverErr, getErr)
	}
	after := before
	if current != nil {
		after = current.Status
	}
	return before, after, recoverErr
}

func (s *flashsaleService) recoverPreDeductionLocked(ctx context.Context, pd *model.PreDeduction, forcePreparing bool) error {
	switch pd.Status {
	case model.PreDeductionStatusPreparing:
		// A live request creates the MySQL fact before running Redis Lua. Do not
		// let startup/cron recovery mistake that normal window for a crash.
		if !forcePreparing && !pd.UpdatedAt.IsZero() && time.Since(pd.UpdatedAt) < preparingRecoveryDelay {
			return nil
		}
		token, err := s.cache.Get(ctx, reservationKey(pd.ID))
		switch {
		case err == nil && token == pd.ReservationToken():
			if err := s.store.PreDeductions.MarkPreDeducted(ctx, pd.ID); err != nil {
				return err
			}
			pd.Status = model.PreDeductionStatusPendingPublish
			return s.dispatchPreDeductionLocked(ctx, pd)
		case errors.Is(err, cache.ErrMiss):
			if err := s.store.PreDeductions.MarkPendingRollback(ctx, pd.ID, "pre-deduction not present in Redis"); err != nil {
				return err
			}
			pd.Status = model.PreDeductionStatusPendingRollback
			return s.rollbackPreDeduction(ctx, pd, true)
		case err != nil:
			return err
		default:
			if err := s.store.PreDeductions.MarkPendingRollback(ctx, pd.ID, "reservation token mismatch"); err != nil {
				return err
			}
			pd.Status = model.PreDeductionStatusPendingRollback
			return s.rollbackPreDeduction(ctx, pd, false)
		}
	case model.PreDeductionStatusPendingPublish:
		if err := s.ensurePreDeductionReservation(ctx, pd); err != nil {
			return err
		}
		return s.dispatchPreDeductionLocked(ctx, pd)
	case model.PreDeductionStatusPendingOrder:
		return s.ensurePreDeductionReservation(ctx, pd)
	case model.PreDeductionStatusPendingRollback:
		return s.rollbackPreDeduction(ctx, pd, true)
	default:
		return nil
	}
}

func (s *flashsaleService) ensurePreDeductionReservation(ctx context.Context, pd *model.PreDeduction) error {
	activity, err := s.store.Activities.GetByID(ctx, pd.ActivityID)
	if err != nil {
		return err
	}
	if activity == nil {
		return ErrActivityNotFound
	}
	if pd.Legacy {
		return adoptLegacyReservation(ctx, s.cache, s.store.PreDeductions, pd, activity)
	}
	stockTTL := remainingTTL(activity)
	if stockTTL <= 0 {
		stockTTL = stockKeyMargin
	}
	_, err = s.cache.EnsureFlashSaleReservationDurably(ctx, cache.FlashSaleEnsureReservationParams{
		StockKey:         stockKey(pd.ActivityID),
		CountKey:         countKey(pd.ActivityID, pd.UserID),
		IdempotencyKey:   preDeductionIdemKey(pd),
		ReservationKey:   reservationKey(pd.ID),
		ReservationToken: pd.ReservationToken(),
		IdempotencyTTL:   idemTTL,
		Quantity:         pd.Quantity,
		FallbackStock:    activity.Stock,
		StockTTL:         stockTTL,
	}, redisAOFTimeout)
	if err != nil {
		return fmt.Errorf("ensure flash-sale reservation: %w", err)
	}
	return nil
}

func (s *flashsaleService) rollbackPreDeduction(ctx context.Context, pd *model.PreDeduction, allowMissing bool) error {
	fallbackStock := 0
	stockTTL := stockKeyMargin
	if allowMissing {
		activity, activityErr := s.store.Activities.GetByID(ctx, pd.ActivityID)
		if activityErr != nil {
			return s.recordRollbackFailure(ctx, pd.ID, activityErr)
		}
		if activity != nil {
			fallbackStock = activity.Stock
			if remainingTTL(activity) > 0 {
				stockTTL = remainingTTL(activity)
			}
		}
	}
	result, err := s.cache.RestoreFlashSaleDurably(ctx, cache.FlashSaleRestoreParams{
		StockKey:                 stockKey(pd.ActivityID),
		CountKey:                 countKey(pd.ActivityID, pd.UserID),
		IdempotencyKey:           preDeductionIdemKey(pd),
		ReservationKey:           reservationKey(pd.ID),
		ReservationToken:         pd.ReservationToken(),
		AllowIdempotencyFallback: pd.Legacy,
		AllowMissingReservation:  allowMissing,
		Quantity:                 pd.Quantity,
		FallbackStock:            fallbackStock,
		StockTTL:                 stockTTL,
	}, redisAOFTimeout)
	if err != nil {
		return s.recordRollbackFailure(ctx, pd.ID, err)
	}
	if result == cache.FlashSaleReservationMissing && !allowMissing {
		return s.recordRollbackFailure(ctx, pd.ID,
			errors.New("flash-sale reservation marker is missing before rollback"))
	}
	if err := s.store.PreDeductions.MarkRolledBack(ctx, pd.ID); err != nil {
		return err
	}
	// MySQL 已记录终态后 tombstone 才可清理；此前保留它用于识别“Redis 已回补、
	// MySQL 状态写入前崩溃”的重试。清理失败只造成一个小 key 残留，不影响一致性。
	_ = s.cache.Del(ctx, reservationKey(pd.ID))
	s.refreshStockGauge(ctx, pd.ActivityID)
	return nil
}

func (s *flashsaleService) recordRollbackFailure(ctx context.Context, id int64, cause error) error {
	if recordErr := s.store.PreDeductions.RecordRollbackFailure(ctx, id, cause.Error()); recordErr != nil {
		return fmt.Errorf("%v; persist rollback failure: %w", cause, recordErr)
	}
	return cause
}
