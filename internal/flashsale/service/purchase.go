package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
)

// SeckillOrderQueue 秒杀异步落单队列：预扣成功后发布"抢购成功"消息，
// 消费者落单（唯一约束幂等）+ 同事务扣活动库存；失败重投/死信（平台 mq 层）。
const SeckillOrderQueue = "flashsale.order.create"

const SeckillOrderDeadLetterQueue = SeckillOrderQueue + ".dlq"

// SeckillSuccessMessage 抢购成功消息体：稳定预扣 ID 关联持久状态，订单号
// 在首次发布前持久化，确认不确定时重投同一消息。
type SeckillSuccessMessage struct {
	PreDeductionID int64  `json:"pre_deduction_id"`
	OrderNo        string `json:"order_no"`
	UserID         int64  `json:"user_id"`
	ActivityID     int64  `json:"activity_id"`
	SKUID          int64  `json:"sku_id"`
	Price          int64  `json:"price"`
	Quantity       int    `json:"quantity"`
	PurchaseSlot   int64  `json:"purchase_slot"`
}

// idemTTL 槽位所有权键 TTL：与规格一致（30min，DESIGN.md）。
const idemTTL = 30 * time.Minute

const maxPublishAttempts = 10

// Seckill 抢购全流程（DESIGN.md 秒杀时序）：限流 → 持久 preparing 事实 →
// 以事实 ID 抢占幂等键 → Redis 原子预扣与 marker → pending_publish →
// 持久订单号并发布。成功预扣后即使当前请求的订单号生成或发布失败，也返回
// 可查询的事实 ID，由启动恢复和 cron 继续发布或完整回退。
func (s *flashsaleService) Seckill(ctx context.Context, userID, activityID int64, clientRequestID string) (*PurchaseResult, error) {
	if strings.TrimSpace(clientRequestID) == "" || utf8.RuneCountInString(clientRequestID) > 64 {
		return nil, fmt.Errorf("%w: invalid client_request_id", ErrInvalidInput)
	}
	if s.store.PreDeductions == nil {
		return nil, errors.New("flashsale pre-deduction repository is not configured")
	}
	existing, err := s.store.PreDeductions.GetByRequestID(ctx, userID, activityID, clientRequestID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return purchaseResult(existing), nil
	}
	s.adminMu.RLock()
	activityLocked := true
	defer func() {
		if activityLocked {
			s.adminMu.RUnlock()
		}
	}()
	if s.PurchasesBlocked() {
		return nil, ErrRecoveryIncomplete
	}
	activity, err := s.store.Activities.GetByID(ctx, activityID)
	if err != nil {
		return nil, err
	}
	if activity == nil {
		return nil, ErrActivityNotFound
	}
	// 2. 先持久化 preparing 事实，再触碰 Redis。若进程在任一外部写入前后崩溃，
	// 恢复任务都能用 reservation marker 判断预扣是否真正发生。
	pd := &model.PreDeduction{
		UserID: userID, ActivityID: activityID, ClientRequestID: clientRequestID,
		SKUID: activity.SKUID, Price: activity.Price, Quantity: 1,
		Status: model.PreDeductionStatusPreparing,
	}
	if err := s.store.PreDeductions.Create(ctx, pd); err != nil {
		if errors.Is(err, repository.ErrPreDeductionDuplicate) {
			existing, getErr := s.store.PreDeductions.GetByRequestID(ctx, userID, activityID, clientRequestID)
			if getErr != nil {
				return nil, getErr
			}
			if existing != nil {
				return purchaseResult(existing), nil
			}
		}
		return nil, err
	}
	// Request identity is durable before rate limiting, so concurrent retries
	// converge on the same fact instead of one retry being rejected as new work.
	ok, err := s.perUser.Allow(ctx, rlKey(userID))
	if err != nil {
		if markErr := s.store.PreDeductions.MarkRolledBack(ctx, pd.ID); markErr != nil {
			return nil, errors.Join(fmt.Errorf("flashsale rate limit: %w", err), markErr)
		}
		return nil, fmt.Errorf("flashsale rate limit: %w", err)
	}
	if !ok {
		if err := s.store.PreDeductions.MarkRolledBack(ctx, pd.ID); err != nil {
			return nil, err
		}
		return nil, ErrRateLimited
	}

	// 3. 每个预扣事实就是一个稳定购买槽位；Redis 所有权键按槽位隔离。
	key := slotIdemKey(activityID, userID, pd.PurchaseSlot)
	result, err := s.cache.AcquireIdempotency(ctx, key, pd.ReservationToken(), idemTTL)
	if err != nil {
		return purchaseResult(pd), nil
	}
	if result == cache.IdempotencyExists {
		return purchaseResult(pd), nil
	}
	if result != cache.IdempotencyAcquired {
		return purchaseResult(pd), nil
	}

	// 4. 库存、用户计数与 reservation marker 在同一段 Lua 中提交。
	if err := s.preDeductActivity(ctx, userID, activity, pd); err != nil {
		if isBusinessReject(err) {
			if delErr := s.cache.ReleaseIdempotencyDurably(ctx, key, pd.ReservationToken(), redisAOFTimeout); delErr != nil {
				return nil, fmt.Errorf("%w: release idempotency key: %v", err, delErr)
			}
			if markErr := s.store.PreDeductions.MarkRolledBack(ctx, pd.ID); markErr != nil {
				return nil, fmt.Errorf("%w: mark rejected pre-deduction rolled back: %v", err, markErr)
			}
			return nil, err
		}
		return purchaseResult(pd), nil
	}
	s.adminMu.RUnlock()
	activityLocked = false
	if err := s.store.PreDeductions.MarkPreDeducted(ctx, pd.ID); err != nil {
		return purchaseResult(pd), nil
	}
	pd.Status = model.PreDeductionStatusPendingPublish

	// 5. 同步尝试生成订单号并发布，任何失败都只写入持久状态；HTTP 仍返回
	// 已被系统接管的稳定预扣 ID，后台任务会继续发布或完整回退。
	_ = s.RecoverPreDeduction(ctx, pd.ID)
	if current, getErr := s.store.PreDeductions.GetByID(ctx, pd.ID); getErr == nil && current != nil {
		pd = current
	}
	return purchaseResult(pd), nil
}

func purchaseResult(p *model.PreDeduction) *PurchaseResult {
	return &PurchaseResult{
		PreDeductionID: p.ID,
		OrderNo:        p.OrderNumber(),
		Status:         p.Status,
	}
}

func (s *flashsaleService) dispatchPreDeductionLocked(ctx context.Context, pd *model.PreDeduction) error {
	if pd.Status != model.PreDeductionStatusPendingPublish {
		return nil
	}
	orderNo := pd.OrderNumber()
	if orderNo == "" {
		no, err := s.nos.Next()
		if err != nil {
			return s.recordPublishFailure(ctx, pd, fmt.Errorf("generate seckill order no: %w", err))
		}
		orderNo = strconv.FormatInt(no, 10)
		if err := s.store.PreDeductions.SetOrderNo(ctx, pd.ID, orderNo); err != nil {
			return s.recordPublishFailure(ctx, pd, fmt.Errorf("persist seckill order no: %w", err))
		}
		current, err := s.store.PreDeductions.GetByID(ctx, pd.ID)
		if err != nil {
			return s.recordPublishFailure(ctx, pd, fmt.Errorf("reload seckill order no: %w", err))
		}
		if current == nil || current.OrderNumber() == "" {
			return s.recordPublishFailure(ctx, pd, errors.New("persisted seckill order no is missing"))
		}
		if current.Status != model.PreDeductionStatusPendingPublish {
			return nil
		}
		pd = current
		orderNo = current.OrderNumber()
	}
	body, err := json.Marshal(SeckillSuccessMessage{
		PreDeductionID: pd.ID,
		OrderNo:        orderNo,
		UserID:         pd.UserID,
		ActivityID:     pd.ActivityID,
		SKUID:          pd.SKUID,
		Price:          pd.Price,
		Quantity:       pd.Quantity,
		PurchaseSlot:   pd.PurchaseSlot,
	})
	if err != nil {
		return s.recordPublishFailure(ctx, pd, fmt.Errorf("marshal seckill message: %w", err))
	}
	if err := retry.Do(ctx, s.retryCfg, func(ctx context.Context) error {
		return s.publisher.Publish(ctx, SeckillOrderQueue, body)
	}); err != nil {
		return s.recordPublishFailure(ctx, pd, fmt.Errorf("publish seckill success message: %w", err))
	}
	if err := s.store.PreDeductions.MarkPendingOrder(ctx, pd.ID); err != nil {
		if errors.Is(err, repository.ErrPreDeductionStateChanged) {
			current, getErr := s.store.PreDeductions.GetByID(ctx, pd.ID)
			if getErr == nil && current != nil && (current.Status == model.PreDeductionStatusPendingOrder ||
				current.Status == model.PreDeductionStatusOrdered) {
				return nil
			}
		}
		return fmt.Errorf("mark seckill message published: %w", err)
	}
	pd.Status = model.PreDeductionStatusPendingOrder
	pd.LastError = ""
	return nil
}

func (s *flashsaleService) recordPublishFailure(ctx context.Context, pd *model.PreDeduction, cause error) error {
	if err := s.store.PreDeductions.RecordPublishFailure(ctx, pd.ID, maxPublishAttempts, cause.Error()); err != nil {
		return fmt.Errorf("%v; persist publish failure: %w", cause, err)
	}
	pd.PublishAttempts++
	pd.LastError = cause.Error()
	if pd.Status == model.PreDeductionStatusPendingPublish && pd.PublishAttempts >= maxPublishAttempts {
		pd.Status = model.PreDeductionStatusPendingRollback
	}
	return cause
}

func (s *flashsaleService) GetPreDeduction(ctx context.Context, userID, id int64) (*model.PreDeduction, error) {
	if s.store.PreDeductions == nil {
		return nil, ErrPreDeductionNotFound
	}
	pd, err := s.store.PreDeductions.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if pd == nil || pd.UserID != userID {
		return nil, ErrPreDeductionNotFound
	}
	return pd, nil
}

// isBusinessReject 预扣的业务拒绝分支（非基础设施故障）。
func isBusinessReject(err error) bool {
	return errors.Is(err, ErrSoldOut) || errors.Is(err, ErrNotInWindow) ||
		errors.Is(err, ErrLimitReached) || errors.Is(err, ErrOffline) ||
		errors.Is(err, ErrActivityNotFound)
}

// refreshStockGauge 回读 Redis 库存余量并同步 gauge（best-effort：
// key 缺失（预热过期自清理）或读失败时不更新，不影响业务）。
func (s *flashsaleService) refreshStockGauge(ctx context.Context, activityID int64) {
	if remaining, err := s.cache.Get(ctx, stockKey(activityID)); err == nil {
		if n, convErr := strconv.Atoi(remaining); convErr == nil {
			s.metrics.SetSeckillStock(activityID, n)
		}
	}
}

// ---- 抢购预扣 ----

func (s *flashsaleService) PreDeduct(ctx context.Context, userID, activityID int64) error {
	a, err := s.store.Activities.GetByID(ctx, activityID)
	if err != nil {
		s.preDeductFailed()
		return err
	}
	if a == nil {
		s.preDeductFailed()
		return ErrActivityNotFound
	}
	return s.preDeductActivity(ctx, userID, a, nil)
}

func (s *flashsaleService) preDeductActivity(ctx context.Context, userID int64, a *model.Activity, pd *model.PreDeduction) error {
	now := time.Now()
	params := cache.FlashSalePreDeductParams{
		StockKey: stockKey(a.ID), CountKey: countKey(a.ID, userID), PauseKey: pauseKey(a.ID), Now: now,
		StartAt: a.StartAt, EndAt: a.EndAt, OnSale: a.Status == model.ActivityStatusOnSale,
		PerUserLimit: a.PerUserLimit,
	}
	if pd != nil {
		params.ReservationKey = reservationKey(pd.ID)
		params.ReservationToken = pd.ReservationToken()
		params.IdempotencyKey = slotIdemKey(a.ID, userID, pd.PurchaseSlot)
		params.IdempotencyTTL = idemTTL
	}
	var result cache.FlashSalePreDeductResult
	var err error
	if pd != nil {
		result, err = s.cache.PreDeductFlashSaleDurably(ctx, params, redisAOFTimeout)
	} else {
		result, err = s.cache.PreDeductFlashSale(ctx, params)
	}
	if err != nil {
		s.preDeductFailed()
		return err
	}

	switch result {
	case cache.FlashSalePreDeducted, cache.FlashSaleAlreadyPreDeducted:
		s.preDeductSuccess()
		// 库存余量 gauge 随预扣刷新（best-effort 回读，T19c）。
		s.refreshStockGauge(ctx, a.ID)
		return nil
	case cache.FlashSaleSoldOut:
		s.preDeductFailed()
		return ErrSoldOut
	case cache.FlashSaleNotInWindow:
		s.preDeductFailed()
		return ErrNotInWindow
	case cache.FlashSaleLimitReached:
		s.preDeductFailed()
		return ErrLimitReached
	case cache.FlashSaleOffline, cache.FlashSalePaused:
		s.preDeductFailed()
		return ErrOffline
	default:
		s.preDeductFailed()
		return fmt.Errorf("%w: unexpected pre-deduct result %d", ErrInvalidInput, result)
	}
}

func preDeductionIdemKey(pd *model.PreDeduction) string {
	if pd.Legacy {
		return legacyIdemKey(pd.ActivityID, pd.UserID)
	}
	return slotIdemKey(pd.ActivityID, pd.UserID, pd.PurchaseSlot)
}

// preDeductSuccess/preDeductFailed 预扣结果打点（T19c）。
func (s *flashsaleService) preDeductSuccess() { s.metrics.SeckillPreDeduct(true) }

func (s *flashsaleService) preDeductFailed() { s.metrics.SeckillPreDeduct(false) }
