package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
)

// GORMOrderStore 订单仓储实现（GORM）：开启事务 + 订单读写。
type GORMOrderStore struct {
	db *gorm.DB
}

// NewGORMOrder 构造订单仓储。
func NewGORMOrder(db *gorm.DB) *GORMOrderStore {
	return &GORMOrderStore{db: db}
}

// GORMOrderItemStore 订单项仓储实现（GORM）。
type GORMOrderItemStore struct {
	db *gorm.DB
}

// NewGORMOrderItem 构造订单项仓储。
func NewGORMOrderItem(db *gorm.DB) *GORMOrderItemStore {
	return &GORMOrderItemStore{db: db}
}

// WithinTx 开启跨模块事务；fn 返回错误则整体回滚。
func (s *GORMOrderStore) WithinTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(fn)
}

func (s *GORMOrderStore) Create(ctx context.Context, tx *gorm.DB, order *model.Order) error {
	return tx.WithContext(ctx).Create(order).Error
}

func (s *GORMOrderStore) GetByNo(ctx context.Context, orderNo string) (*model.Order, error) {
	var o model.Order
	if err := s.db.WithContext(ctx).First(&o, "order_no = ?", orderNo).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

func (s *GORMOrderStore) List(ctx context.Context, userID int64, status string, offset, limit int) ([]model.Order, int64, error) {
	q := s.db.WithContext(ctx).Model(&model.Order{}).Where("user_id = ?", userID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []model.Order
	if err := q.Order("created_at DESC, order_no DESC").Offset(offset).Limit(limit).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// Cancel 条件更新 待支付→已取消；RowsAffected=0 表示状态已变（并发/非法跃迁）。
func (s *GORMOrderStore) Cancel(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error) {
	return s.transition(ctx, tx, orderNo, model.OrderStatusPendingPayment, model.OrderStatusCancelled, "cancelled_at")
}

// MarkPaid 条件更新 待支付→已支付（支付回调）；WHERE 同时校验 status 与
// pay_amount：RowsAffected=0 表示状态已变（并发/非法跃迁）或回调金额与应付不符。
func (s *GORMOrderStore) MarkPaid(ctx context.Context, tx *gorm.DB, orderNo string, payAmount int64) (bool, error) {
	exec := tx
	if exec == nil {
		exec = s.db.WithContext(ctx)
	}
	res := exec.Model(&model.Order{}).
		Where("order_no = ? AND status = ? AND pay_amount = ?", orderNo, model.OrderStatusPendingPayment, payAmount).
		Updates(map[string]any{"status": model.OrderStatusPaid, "paid_at": time.Now()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// Ship 条件更新 已支付→已发货（admin 发货）。
func (s *GORMOrderStore) Ship(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error) {
	return s.transition(ctx, tx, orderNo, model.OrderStatusPaid, model.OrderStatusShipped, "shipped_at")
}

// ConfirmReceipt 条件更新 已发货→已完成（用户确认收货）。
func (s *GORMOrderStore) ConfirmReceipt(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error) {
	return s.transition(ctx, tx, orderNo, model.OrderStatusShipped, model.OrderStatusCompleted, "completed_at")
}

// transition 状态机条件更新：WHERE status=from → status=to，并记录迁移时间。
// 迁移时间经 Go 传入而非 NOW(3)：DATETIME 按 Go 本地墙钟写入（go-sql-driver
// 的 loc=Local 行为），与 created_at/expire_at 同源，与 MySQL 服务器时区解耦。
// tx 为 nil 时使用仓储自身连接（单条 UPDATE，无需事务）。
func (s *GORMOrderStore) transition(ctx context.Context, tx *gorm.DB, orderNo, from, to, atColumn string) (bool, error) {
	exec := tx
	if exec == nil {
		exec = s.db.WithContext(ctx)
	}
	res := exec.Model(&model.Order{}).
		Where("order_no = ? AND status = ?", orderNo, from).
		Updates(map[string]any{"status": to, atColumn: time.Now()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ---- 订单项 ----

func (s *GORMOrderItemStore) Create(ctx context.Context, tx *gorm.DB, item *model.OrderItem) error {
	return tx.WithContext(ctx).Create(item).Error
}

func (s *GORMOrderItemStore) ListByOrder(ctx context.Context, orderNo string) ([]model.OrderItem, error) {
	var list []model.OrderItem
	if err := s.db.WithContext(ctx).Where("order_no = ?", orderNo).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (s *GORMOrderItemStore) ListByOrders(ctx context.Context, orderNos []string) (map[string][]model.OrderItem, error) {
	result := make(map[string][]model.OrderItem)
	if len(orderNos) == 0 {
		return result, nil
	}
	var list []model.OrderItem
	if err := s.db.WithContext(ctx).Where("order_no IN ?", orderNos).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	for i := range list {
		result[list[i].OrderNo] = append(result[list[i].OrderNo], list[i])
	}
	return result, nil
}

// 编译期断言：GORM 实现满足仓储接口。
var _ OrderRepository = (*GORMOrderStore)(nil)
var _ OrderItemRepository = (*GORMOrderItemStore)(nil)
var _ TxRunner = (*GORMOrderStore)(nil)
