package service

import (
	"context"
	"fmt"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// Cancel 取消待支付订单：事务内 条件更新 待支付→已取消 + 回补库存 + 回退券。
// 重复取消（或状态已变）由条件更新 RowsAffected=0 兜底，不重复回补。
// 秒杀订单交给 flashsale 编排回补活动库存、Redis 库存与用户计数；
// 禁止走普通订单的 SKU 回补路径。
func (s *orderService) Cancel(ctx context.Context, userID int64, orderNo string) error {
	view, err := s.loadOwned(ctx, userID, orderNo)
	if err != nil {
		return err
	}
	if view.OrderType == model.OrderTypeSeckill {
		return ErrSeckillCancellationRequired
	}
	if !model.CanTransition(view.Status, model.OrderStatusCancelled) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, view.Status, model.OrderStatusCancelled)
	}

	mutatingProducts, err := s.beginProductDetailMutations(ctx, productIDsFromItems(view.Items))
	if err != nil {
		return err
	}
	err = s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		return s.cancelInTx(ctx, tx, view)
	})
	s.finishProductDetailMutations(ctx, mutatingProducts)
	if err != nil {
		return err
	}
	// 状态流转打点（T19c）：事务提交后计数（回补失败回滚不计数）。
	s.metrics.OrderStatusChanged(model.OrderStatusCancelled)
	return nil
}

// CancelExpired 超时取消（cron 每分钟扫描调用）：
//  1. 扫描待支付且已过 expire_at 的普通订单（分批上限 cancelExpiredBatch）
//  2. 逐个事务内取消：条件更新 待支付→已取消 → 回补库存 → 回退券
//
// 单订单失败不阻断整轮：跳过并继续其余订单，失败订单停留待支付、下个 tick
// 重试（at-least-once），失败数供调用方记录日志。并发下已被支付/取消
// （ErrOrderChanged）属正常跳过；其余失败（如券状态异常）同样计入失败数——
// 孤立异常订单不应阻塞全部超时取消。扫描/批量读取等系统性错误向上传播
// （cron 记录日志，下个 tick 重试）。
func (s *orderService) CancelExpired(ctx context.Context) (cancelled, failed int, err error) {
	orders, err := s.store.Orders.ListExpiredPending(ctx, time.Now(), cancelExpiredBatch)
	if err != nil {
		return 0, 0, err
	}
	if len(orders) == 0 {
		return 0, 0, nil
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return 0, 0, err
	}

	for _, o := range orders {
		view := &model.OrderView{Order: o, Items: itemsByOrder[o.OrderNo]}
		mutatingProducts, beginErr := s.beginProductDetailMutations(ctx, productIDsFromItems(view.Items))
		if beginErr != nil {
			failed++
			continue
		}
		err := s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
			return s.cancelInTx(ctx, tx, view)
		})
		s.finishProductDetailMutations(ctx, mutatingProducts)
		if err != nil {
			failed++
			continue
		}
		cancelled++
		// 状态流转打点（T19c）：事务提交后计数。
		s.metrics.OrderStatusChanged(model.OrderStatusCancelled)
	}
	return cancelled, failed, nil
}

// ListExpiredSeckill 聚合超时秒杀订单与订单项数量；数据异常保留为零值，
// 由调用方计入失败并跳过，避免在缺少活动或数量信息时错误回补库存。
func (s *orderService) ListExpiredSeckill(ctx context.Context) ([]SeckillCancellationOrder, error) {
	orders, err := s.store.Orders.ListExpiredSeckillPending(ctx, time.Now(), cancelExpiredBatch)
	if err != nil {
		return nil, err
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return nil, err
	}

	result := make([]SeckillCancellationOrder, 0, len(orders))
	for _, o := range orders {
		items := itemsByOrder[o.OrderNo]
		item := SeckillCancellationOrder{
			OrderNo:  o.OrderNo,
			UserID:   o.UserID,
			Quantity: sumItemQuantity(items),
		}
		if o.ActivityID != nil {
			item.ActivityID = *o.ActivityID
		}
		if o.PurchaseSlot != nil {
			item.PurchaseSlot = *o.PurchaseSlot
		}
		if len(items) > 0 {
			item.SKUID = items[0].SKUID
			item.Price = items[0].Price
		}
		result = append(result, item)
	}
	return result, nil
}

// CancelSeckill 在调用方开启的事务中执行条件状态迁移；库存回补由 flashsale
// 模块在同一事务中紧随其后完成。
func (s *orderService) CancelSeckill(ctx context.Context, tx *transaction.Handle, orderNo string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("%w: transaction required", ErrInvalidInput)
	}
	return s.store.Orders.Cancel(ctx, tx, orderNo)
}

func (s *orderService) SeckillCancellation(ctx context.Context, userID int64, orderNo string) (*SeckillCancellationOrder, error) {
	view, err := s.loadOwned(ctx, userID, orderNo)
	if err != nil {
		return nil, err
	}
	if view.OrderType != model.OrderTypeSeckill || !model.CanTransition(view.Status, model.OrderStatusCancelled) {
		return nil, fmt.Errorf("%w: %s → %s", ErrIllegalTransition, view.Status, model.OrderStatusCancelled)
	}
	result := &SeckillCancellationOrder{OrderNo: view.OrderNo, UserID: view.UserID, Quantity: sumItemQuantity(view.Items)}
	if view.ActivityID != nil {
		result.ActivityID = *view.ActivityID
	}
	if view.PurchaseSlot != nil {
		result.PurchaseSlot = *view.PurchaseSlot
	}
	if len(view.Items) > 0 {
		result.SKUID = view.Items[0].SKUID
		result.Price = view.Items[0].Price
	}
	if result.ActivityID <= 0 || result.Quantity < 1 {
		return nil, fmt.Errorf("%w: invalid seckill cancellation snapshot", ErrInvalidInput)
	}
	return result, nil
}

// sumItemQuantity 订单项数量合计（秒杀订单固定单条订单项 Quantity=1，
// 求和以数量维度正确回补，防未来多数量秒杀扩展）。
func sumItemQuantity(items []model.OrderItem) int {
	n := 0
	for _, it := range items {
		n += it.Quantity
	}
	return n
}

// cancelInTx 事务内取消：条件更新 待支付→已取消 + 回补库存 + 回退券；
// 用户取消与超时取消共用（库存/券补偿逻辑单点维护）。
func (s *orderService) cancelInTx(ctx context.Context, tx *transaction.Handle, view *model.OrderView) error {
	ok, err := s.store.Orders.Cancel(ctx, tx, view.OrderNo)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOrderChanged
	}
	for _, it := range view.Items {
		if err := s.products.RestoreStock(ctx, tx, it.SKUID, it.Quantity); err != nil {
			return translateProductError(err)
		}
	}
	if view.CouponID != nil {
		if err := s.coupons.RollbackCoupon(ctx, tx, view.UserID, *view.CouponID); err != nil {
			return translateCouponError(err)
		}
	}
	return nil
}

// MarkPaid 支付成功状态迁移：待支付 → 已支付（事务由支付模块开启）。
// 状态机、金额核对与支付期限由条件更新 WHERE 原子兜底；false 表示状态、
// 金额或期限不再允许支付，由支付模块统一按订单已变化处理。
func (s *orderService) MarkPaid(ctx context.Context, tx *transaction.Handle, orderNo string, payAmount int64) (bool, error) {
	return s.store.Orders.MarkPaid(ctx, tx, orderNo, payAmount)
}

func (s *orderService) CanRecordFailedPayment(ctx context.Context, tx *transaction.Handle, orderNo string) (bool, error) {
	return s.store.Orders.CanRecordFailedPayment(ctx, tx, orderNo)
}

// Ship 后台发货：已支付 → 已发货（admin；发货不校验归属）。
func (s *orderService) Ship(ctx context.Context, orderNo string) error {
	order, err := s.store.Orders.GetByNo(ctx, orderNo)
	if err != nil {
		return err
	}
	if order == nil {
		return ErrOrderNotFound
	}
	if !model.CanTransition(order.Status, model.OrderStatusShipped) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, order.Status, model.OrderStatusShipped)
	}
	ok, err := s.store.Orders.Ship(ctx, orderNo)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOrderChanged
	}
	s.metrics.OrderStatusChanged(model.OrderStatusShipped)
	return nil
}

// ConfirmReceipt 确认收货：已发货 → 已完成（owner 校验）。
func (s *orderService) ConfirmReceipt(ctx context.Context, userID int64, orderNo string) error {
	view, err := s.loadOwned(ctx, userID, orderNo)
	if err != nil {
		return err
	}
	if !model.CanTransition(view.Status, model.OrderStatusCompleted) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, view.Status, model.OrderStatusCompleted)
	}
	ok, err := s.store.Orders.ConfirmReceipt(ctx, orderNo)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOrderChanged
	}
	s.metrics.OrderStatusChanged(model.OrderStatusCompleted)
	return nil
}
