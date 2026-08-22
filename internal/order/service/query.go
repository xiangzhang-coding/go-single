package service

import (
	"context"
	"fmt"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
)

// List 我的订单：状态筛选（空 = 全部）+ 分页；订单项随列表一次取出。
func (s *orderService) List(ctx context.Context, userID int64, status string, page, pageSize int) ([]model.OrderView, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if status != "" && !validStatus(status) {
		return nil, 0, fmt.Errorf("%w: invalid status", ErrInvalidInput)
	}

	orders, total, err := s.store.Orders.List(ctx, userID, status, (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, 0, err
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return nil, 0, err
	}

	views := make([]model.OrderView, 0, len(orders))
	for _, o := range orders {
		views = append(views, model.OrderView{Order: o, Items: itemsByOrder[o.OrderNo]})
	}
	return views, total, nil
}

// ListAll 后台全量订单（T25）：跨用户，状态筛选 + 分页；与 List 同构。
func (s *orderService) ListAll(ctx context.Context, status string, page, pageSize int) ([]model.OrderView, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if status != "" && !validStatus(status) {
		return nil, 0, fmt.Errorf("%w: invalid status", ErrInvalidInput)
	}

	orders, total, err := s.store.Orders.ListAll(ctx, status, (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, 0, err
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return nil, 0, err
	}

	views := make([]model.OrderView, 0, len(orders))
	for _, o := range orders {
		views = append(views, model.OrderView{Order: o, Items: itemsByOrder[o.OrderNo]})
	}
	return views, total, nil
}

// GetDetail 订单详情（owner 校验）。
func (s *orderService) GetDetail(ctx context.Context, userID int64, orderNo string) (*model.OrderView, error) {
	return s.loadOwned(ctx, userID, orderNo)
}

// CountValidSeckill 活动的秒杀有效订单数（非取消），对账端口实现
// （flashsale ReconcileActive 以此解释 Redis/MySQL 库存差额）。
func (s *orderService) CountValidSeckill(ctx context.Context, activityID int64) (int, error) {
	return s.store.Orders.CountValidByActivity(ctx, activityID)
}

func (s *orderService) SeckillOrderStatus(ctx context.Context, orderNo string) (string, bool, error) {
	order, err := s.store.Orders.GetByNo(ctx, orderNo)
	if err != nil {
		return "", false, err
	}
	if order == nil || order.OrderType != model.OrderTypeSeckill {
		return "", false, nil
	}
	return order.Status, true, nil
}

// HasPurchasedSKU 好友圈分享校验：存在 已支付/已发货/已完成 订单含该 SKU。
func (s *orderService) HasPurchasedSKU(ctx context.Context, userID, skuID int64) (bool, error) {
	return s.store.Items.HasPurchased(ctx, userID, skuID)
}

// loadOwned 读取订单 + 订单项并校验归属（防 IDOR）。
func (s *orderService) loadOwned(ctx context.Context, userID int64, orderNo string) (*model.OrderView, error) {
	view, err := s.loadView(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, ErrOrderNotFound
	}
	if view.UserID != userID {
		return nil, ErrOrderForbidden
	}
	return view, nil
}

// loadView 读取订单 + 订单项；不存在返回 (nil, nil)。
func (s *orderService) loadView(ctx context.Context, orderNo string) (*model.OrderView, error) {
	order, err := s.store.Orders.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, nil
	}
	items, err := s.store.Items.ListByOrder(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	return &model.OrderView{Order: *order, Items: items}, nil
}

func validStatus(status string) bool {
	switch status {
	case model.OrderStatusPendingPayment, model.OrderStatusPaid, model.OrderStatusShipped,
		model.OrderStatusCompleted, model.OrderStatusCancelled:
		return true
	}
	return false
}
