package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
)

// stockKeyMargin 预热库存 TTL 的余量：剩余时长 + 1h，活动结束后自清理。
const stockKeyMargin = time.Hour

const (
	stockEditPauseTTL     = 24 * time.Hour
	failClosedStepTimeout = redisAOFTimeout + time.Second
)

func (s *flashsaleService) CreateActivity(ctx context.Context, p ActivityParams) (*model.Activity, error) {
	if err := validateActivity(&p); err != nil {
		return nil, err
	}
	if err := s.checkSKU(ctx, p.SKUID); err != nil {
		return nil, err
	}
	a := &model.Activity{
		SKUID:        p.SKUID,
		Title:        p.Title,
		Price:        p.Price,
		Stock:        p.Stock,
		PerUserLimit: p.PerUserLimit,
		Status:       model.ActivityStatusOffSale,
		StartAt:      p.StartAt,
		EndAt:        p.EndAt,
	}
	if err := s.store.Activities.Create(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *flashsaleService) UpdateActivity(ctx context.Context, id int64, p ActivityParams) error {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	if err := validateActivity(&p); err != nil {
		return err
	}
	old, err := s.store.Activities.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if old == nil {
		return ErrActivityNotFound
	}
	if p.SKUID != old.SKUID {
		if err := s.checkSKU(ctx, p.SKUID); err != nil {
			return err
		}
	}

	if s.store.Tx == nil {
		return errors.New("flashsale transaction runner is not configured")
	}
	if !old.IsOnSale() || !old.InProgress(time.Now()) {
		if err := s.settleActivityReservations(ctx, id); err != nil {
			return err
		}
	}
	var current *model.Activity
	var inProgress bool
	err = s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		current, err = s.store.Activities.GetByIDForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrActivityNotFound
		}
		now := time.Now()
		inProgress = current.IsOnSale() && (current.InProgress(now) || inWindow(p, now))
		if inProgress {
			return nil
		}
		pendingQuantity, err := s.store.PreDeductions.PendingReservationQuantityForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if pendingQuantity > 0 {
			return ErrReservationsUnsettled
		}
		protectedFieldsChanged := p.SKUID != current.SKUID || p.Price != current.Price ||
			p.PerUserLimit != current.PerUserLimit || !p.StartAt.Equal(current.StartAt) || !p.EndAt.Equal(current.EndAt)
		if protectedFieldsChanged {
			hasAccepted, err := s.store.PreDeductions.HasAcceptedReservationForUpdate(ctx, tx, id)
			if err != nil {
				return err
			}
			if hasAccepted {
				if now.After(current.EndAt) {
					return ErrActivityEnded
				}
				if !now.Before(current.StartAt) {
					return ErrActivityFieldsLocked
				}
			}
		}
		return s.store.Activities.UpdateInTx(ctx, tx, &model.Activity{
			ID: id, SKUID: p.SKUID, Title: p.Title, Price: p.Price, Stock: p.Stock,
			PerUserLimit: p.PerUserLimit, Status: current.Status, StartAt: p.StartAt, EndAt: p.EndAt,
		})
	})
	if err != nil {
		return err
	}
	if inProgress {
		return s.updateInProgressActivity(ctx, id, p)
	}
	// 已上架的活动编辑后同步预热库存。
	if current.IsOnSale() {
		newA := &model.Activity{
			ID: id, SKUID: p.SKUID, Title: p.Title, Price: p.Price, Stock: p.Stock,
			PerUserLimit: p.PerUserLimit, Status: current.Status, StartAt: p.StartAt, EndAt: p.EndAt,
		}
		syncErr := s.syncStock(ctx, newA, time.Now())
		if syncErr != nil {
			return s.failClosedActivity(ctx, id, syncErr)
		}
	}
	return nil
}

func (s *flashsaleService) updateInProgressActivity(ctx context.Context, id int64, p ActivityParams) error {
	if s.store.Tx == nil {
		return errors.New("flashsale transaction runner is not configured")
	}
	key := pauseKey(id)
	token := strconv.FormatInt(time.Now().UnixNano(), 10)
	redisStock, err := s.cache.PauseFlashSaleStockDurably(ctx, cache.FlashSalePauseParams{
		StockKey: stockKey(id), PauseKey: key, Token: token, TTL: stockEditPauseTTL,
	}, redisAOFTimeout)
	if err != nil {
		return s.failClosedActivity(ctx, id, fmt.Errorf("pause flash-sale stock: %w", err))
	}

	var delta int
	err = s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		current, err := s.store.Activities.GetByIDForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrActivityNotFound
		}
		if !current.IsOnSale() || !current.InProgress(time.Now()) ||
			p.SKUID != current.SKUID || p.Price != current.Price || p.PerUserLimit != current.PerUserLimit ||
			!p.StartAt.Equal(current.StartAt) || !p.EndAt.Equal(current.EndAt) {
			return ErrActivityFieldsLocked
		}
		if p.Stock > current.Stock {
			return ErrStockIncreaseInProgress
		}
		if redisStock > current.Stock {
			return fmt.Errorf("Redis sellable stock %d exceeds MySQL stock %d", redisStock, current.Stock)
		}
		delta = current.Stock - p.Stock
		if delta > redisStock {
			return ErrStockBelowAcceptedReservations
		}
		return s.store.Activities.UpdateInTx(ctx, tx, &model.Activity{
			ID: id, SKUID: current.SKUID, Title: p.Title, Price: current.Price,
			Stock: p.Stock, PerUserLimit: current.PerUserLimit, Status: model.ActivityStatusOffSale,
			StartAt: current.StartAt, EndAt: current.EndAt,
		})
	})
	if err != nil {
		if errors.Is(err, ErrActivityNotFound) || errors.Is(err, ErrActivityFieldsLocked) ||
			errors.Is(err, ErrStockIncreaseInProgress) || errors.Is(err, ErrStockBelowAcceptedReservations) {
			if releaseErr := s.cache.ReleaseFlashSalePauseDurably(ctx, key, token, redisAOFTimeout); releaseErr != nil {
				return s.failClosedActivity(ctx, id, errors.Join(err, releaseErr))
			}
			return err
		}
		return s.failClosedActivity(ctx, id, err)
	}
	if delta > 0 {
		if err := s.cache.DecreaseFlashSaleStockDurably(ctx, cache.FlashSaleDecreaseParams{
			StockKey: stockKey(id), Delta: delta,
		}, redisAOFTimeout); err != nil {
			return s.failClosedActivity(ctx, id, fmt.Errorf("decrease paused flash-sale stock: %w", err))
		}
	}
	if err := s.store.Activities.UpdateStatus(ctx, id, model.ActivityStatusOnSale); err != nil {
		return s.failClosedActivity(ctx, id, fmt.Errorf("restore flash-sale status after edit: %w", err))
	}
	if err := s.cache.ReleaseFlashSalePauseDurably(ctx, key, token, redisAOFTimeout); err != nil {
		return s.failClosedActivity(ctx, id, fmt.Errorf("release flash-sale stock pause: %w", err))
	}
	s.refreshStockGauge(ctx, id)
	return nil
}

func (s *flashsaleService) failClosedActivity(ctx context.Context, id int64, cause error) error {
	deleteCtx, cancelDelete := context.WithTimeout(context.WithoutCancel(ctx), failClosedStepTimeout)
	deleteStockErr := s.cache.Del(deleteCtx, stockKey(id))
	cancelDelete()

	statusCtx, cancelStatus := context.WithTimeout(context.WithoutCancel(ctx), failClosedStepTimeout)
	statusErr := s.store.Activities.UpdateStatus(statusCtx, id, model.ActivityStatusOffSale)
	cancelStatus()

	var releasePauseErr, holdPauseErr error
	if statusErr == nil {
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), failClosedStepTimeout)
		releasePauseErr = s.cache.ReleaseFlashSalePauseDurably(releaseCtx, pauseKey(id), "", redisAOFTimeout)
		cancelRelease()
	} else {
		holdCtx, cancelHold := context.WithTimeout(context.WithoutCancel(ctx), failClosedStepTimeout)
		holdPauseErr = s.cache.HoldFlashSalePauseDurably(holdCtx, pauseKey(id), redisAOFTimeout)
		cancelHold()
	}
	s.metrics.DeleteSeckillStock(id)
	return errors.Join(cause, statusErr, deleteStockErr, releasePauseErr, holdPauseErr)
}

func (s *flashsaleService) settleActivityReservations(ctx context.Context, activityID int64) error {
	rows, err := s.store.PreDeductions.ListRecoverableByActivity(ctx, activityID)
	if err != nil {
		return err
	}
	for i := range rows {
		if err := s.RecoverPreDeduction(ctx, rows[i].ID); err != nil {
			return fmt.Errorf("%w: %v", ErrReservationsUnsettled, err)
		}
		current, err := s.store.PreDeductions.GetByID(ctx, rows[i].ID)
		if err != nil {
			return err
		}
		if current != nil && (current.Status == model.PreDeductionStatusPreparing ||
			current.Status == model.PreDeductionStatusPendingRollback) {
			return ErrReservationsUnsettled
		}
	}
	return nil
}

// PublishActivity 上架：先预热 Redis 库存、后写状态——预热失败时活动保持下架，
// 避免出现"已上架但无预热库存"（那样抢购会误报已抢光）。
func (s *flashsaleService) PublishActivity(ctx context.Context, id int64) error {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	a, err := s.store.Activities.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrActivityNotFound
	}
	if a.IsOnSale() {
		return nil
	}
	if err := s.checkSKUAvailable(ctx, a.SKUID); err != nil {
		return err
	}
	if err := s.settleActivityReservations(ctx, id); err != nil {
		return err
	}
	if s.store.Tx == nil {
		return errors.New("flashsale transaction runner is not configured")
	}
	return s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		current, err := s.store.Activities.GetByIDForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrActivityNotFound
		}
		now := time.Now()
		if now.After(current.EndAt) {
			return fmt.Errorf("%w: activity already ended", ErrInvalidInput)
		}
		stockSnapshot := *current
		pendingQuantity, err := s.store.PreDeductions.PendingReservationQuantityForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if pendingQuantity > current.Stock {
			return ErrStockBelowAcceptedReservations
		}
		stockSnapshot.Stock -= pendingQuantity
		if err := s.syncStock(ctx, &stockSnapshot, now); err != nil {
			return err
		}
		if err := s.cache.ReleaseFlashSalePauseDurably(ctx, pauseKey(id), "", redisAOFTimeout); err != nil {
			return err
		}
		current.Status = model.ActivityStatusOnSale
		return s.store.Activities.UpdateInTx(ctx, tx, current)
	})
}

func (s *flashsaleService) UnpublishActivity(ctx context.Context, id int64) error {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	a, err := s.store.Activities.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrActivityNotFound
	}
	var pauseErr error
	if a.IsOnSale() {
		token := strconv.FormatInt(time.Now().UnixNano(), 10)
		_, pauseErr = s.cache.PauseFlashSaleStockDurably(ctx, cache.FlashSalePauseParams{
			StockKey: stockKey(id), PauseKey: pauseKey(id), Token: token, TTL: stockEditPauseTTL,
		}, redisAOFTimeout)
	}
	return s.failClosedActivity(ctx, id, pauseErr)
}

// checkSKU 校验 SKU 存在（跨模块经 product 服务接口，与购物车同模式）。
func (s *flashsaleService) checkSKU(ctx context.Context, skuID int64) error {
	if _, err := s.products.GetSKU(ctx, skuID); err != nil {
		if errors.Is(err, productsvc.ErrSKUNotFound) {
			return fmt.Errorf("%w: sku not found", ErrInvalidInput)
		}
		return err
	}
	return nil
}

func (s *flashsaleService) checkSKUAvailable(ctx context.Context, skuID int64) error {
	sku, err := s.products.GetSKU(ctx, skuID)
	if err != nil {
		if errors.Is(err, productsvc.ErrSKUNotFound) {
			return fmt.Errorf("%w: sku not found", ErrInvalidInput)
		}
		return err
	}
	product, err := s.products.GetProduct(ctx, sku.ProductID)
	if err != nil {
		if errors.Is(err, productsvc.ErrProductNotFound) {
			return fmt.Errorf("%w: sku product not found", ErrInvalidInput)
		}
		return err
	}
	if !product.IsOnSale() {
		return fmt.Errorf("%w: sku product is not on sale", ErrInvalidInput)
	}
	return nil
}

// syncStock 以同连接 Lua + WAITAOF 预热/同步活动库存：
//   - 未开始：可覆盖，以配置为准；
//   - 进行中：只减不增（原子缓存能力：key 缺失时写入；已存在时仅配置库存更低才覆盖）；
//   - 已结束：不预热。
func (s *flashsaleService) syncStock(ctx context.Context, a *model.Activity, now time.Time) error {
	key := stockKey(a.ID)
	switch {
	case now.Before(a.StartAt): // 未开始：覆盖
		if _, err := s.cache.WarmFlashSaleStockDurably(ctx, cache.FlashSaleWarmParams{
			StockKey: key, Stock: a.Stock, TTL: remainingTTL(a), Overwrite: true,
		}, redisAOFTimeout); err != nil {
			return err
		}
		s.metrics.SetSeckillStock(a.ID, a.Stock)
		return nil
	case now.After(a.EndAt): // 已结束：不预热
		return nil
	}
	// 进行中：原子只减不增（缓存适配器内含 SETNX 语义与存量保护）。
	if _, err := s.cache.WarmFlashSaleStockDurably(ctx, cache.FlashSaleWarmParams{
		StockKey: key, Stock: a.Stock, TTL: remainingTTL(a),
	}, redisAOFTimeout); err != nil {
		return err
	}
	// 回读实际余量同步 gauge（进行中可能保留更低存量，T19c）。
	s.refreshStockGauge(ctx, a.ID)
	return nil
}

// remainingTTL 库存 key 存活时长：活动结束 + 1h 余量后自清理。
func remainingTTL(a *model.Activity) time.Duration {
	return time.Until(a.EndAt) + stockKeyMargin
}

func inWindow(p ActivityParams, now time.Time) bool {
	return !now.Before(p.StartAt) && !now.After(p.EndAt)
}

func validateActivity(p *ActivityParams) error {
	p.Title = strings.TrimSpace(p.Title)
	if p.Title == "" || len(p.Title) > 128 {
		return fmt.Errorf("%w: invalid title", ErrInvalidInput)
	}
	if p.SKUID <= 0 {
		return fmt.Errorf("%w: invalid sku_id", ErrInvalidInput)
	}
	if p.Price <= 0 || p.Price > productmodel.MaxPriceCents {
		return fmt.Errorf("%w: invalid price", ErrInvalidInput)
	}
	if p.Stock < 1 {
		return fmt.Errorf("%w: invalid stock", ErrInvalidInput)
	}
	if p.PerUserLimit < 1 {
		return fmt.Errorf("%w: invalid per_user_limit", ErrInvalidInput)
	}
	if !p.StartAt.Before(p.EndAt) {
		return fmt.Errorf("%w: end_at must be after start_at", ErrInvalidInput)
	}
	return nil
}
