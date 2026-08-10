// Package repository 定义 order 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 订单聚合的写路径横跨订单/订单项/库存/券/购物车（单事务），
// 事务由 TxRunner 开启，跨模块写操作经各模块 service 的 tx 参数汇入同一事务。
package repository

import (
	"context"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
)

// TxRunner 事务运行器：开启跨模块单事务（订单 + 订单项 + 库存扣减 +
// 券核销 + 删除购物车条目），fn 内任一错误整体回滚。
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}

// OrderRepository 订单数据访问接口。
type OrderRepository interface {
	// Create 事务内创建订单（order_no 为主键，雪花 ID）。
	Create(ctx context.Context, tx *gorm.DB, order *model.Order) error
	// GetByNo 按订单号读取（不含归属过滤，归属校验在 service 层）。
	GetByNo(ctx context.Context, orderNo string) (*model.Order, error)
	// List 我的订单：状态筛选（空 = 全部）+ 分页，返回条目与总数。
	List(ctx context.Context, userID int64, status string, offset, limit int) ([]model.Order, int64, error)
	// Cancel 事务内条件更新 待支付→已取消；返回是否更新成功。
	Cancel(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error)
	// MarkPaid 事务内条件更新 待支付→已支付（支付回调）；WHERE 同时校验
	// status 与 pay_amount（状态机 + 金额核对原子兜底），返回是否更新成功。
	MarkPaid(ctx context.Context, tx *gorm.DB, orderNo string, payAmount int64) (bool, error)
	// Ship 事务内条件更新 已支付→已发货（admin 发货）。
	Ship(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error)
	// ConfirmReceipt 事务内条件更新 已发货→已完成（用户确认收货）。
	ConfirmReceipt(ctx context.Context, tx *gorm.DB, orderNo string) (bool, error)
}

// OrderItemRepository 订单项数据访问接口。
type OrderItemRepository interface {
	// Create 事务内创建订单项。
	Create(ctx context.Context, tx *gorm.DB, item *model.OrderItem) error
	// ListByOrder 订单详情：该订单的全部订单项。
	ListByOrder(ctx context.Context, orderNo string) ([]model.OrderItem, error)
	// ListByOrders 订单列表：按订单号分组返回全部订单项（一次查询避免 N+1）。
	ListByOrders(ctx context.Context, orderNos []string) (map[string][]model.OrderItem, error)
}

// Store 聚合仓储，作为 service 的构造入参。
type Store struct {
	Orders OrderRepository
	Items  OrderItemRepository
	Tx     TxRunner
}
