// Package repository 定义 order 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 订单聚合的写路径横跨订单/订单项/库存/券/购物车（单事务），
// 事务由 TxRunner 开启，跨模块写操作经各模块 service 的 tx 参数汇入同一事务。
package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
)

// ErrOrderDuplicate 重复键（MySQL 1062）：order_no 主键或 user_activity_key
// 唯一约束命中——秒杀异步落单的幂等命中（重复投递/并发消费），视为成功。
var ErrOrderDuplicate = errors.New("order duplicate")

// TxRunner 事务运行器：开启跨模块单事务（订单 + 订单项 + 库存扣减 +
// 券核销 + 删除购物车条目），fn 内任一错误整体回滚。
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}

// OrderRepository 订单数据访问接口。
type OrderRepository interface {
	// Create 事务内创建订单（order_no 为主键，雪花 ID）。
	// 重复键（order_no 或秒杀 user_activity_key）返回 ErrOrderDuplicate。
	Create(ctx context.Context, tx *gorm.DB, order *model.Order) error
	// GetByNo 按订单号读取（不含归属过滤，归属校验在 service 层）。
	GetByNo(ctx context.Context, orderNo string) (*model.Order, error)
	// GetByNoInTx 在调用方事务内核验重复键是否确为同一订单。
	GetByNoInTx(ctx context.Context, tx *gorm.DB, orderNo string) (*model.Order, error)
	// List 我的订单：状态筛选（空 = 全部）+ 分页，返回条目与总数。
	List(ctx context.Context, userID int64, status string, offset, limit int) ([]model.Order, int64, error)
	// ListAll 全量订单（后台 T25）：跨用户，状态筛选（空 = 全部）+ 分页。
	ListAll(ctx context.Context, status string, offset, limit int) ([]model.Order, int64, error)
	// ListExpiredPending 超时扫描：待支付且已过 expire_at 的普通订单
	// （超时取消仅针对普通订单；秒杀订单见 ListExpiredSeckillPending）。
	// now 由调用方传入（Go 时钟），limit 分批上限，供 cron 每分钟扫描。
	ListExpiredPending(ctx context.Context, now time.Time, limit int) ([]model.Order, error)
	// ListExpiredSeckillPending 超时扫描：待支付、秒杀且已过 expire_at 的订单
	// （T13 秒杀超时取消：回补活动库存 + Redis 库存 + 用户计数，允许再次抢购）。
	ListExpiredSeckillPending(ctx context.Context, now time.Time, limit int) ([]model.Order, error)
	// CountValidByActivity 活动的秒杀有效订单数（非取消：待支付/已支付/已发货/
	// 已完成），对账用（Redis 活动库存 vs flashsale.stock vs 秒杀有效订单数）。
	CountValidByActivity(ctx context.Context, activityID int64) (int, error)
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
	// HasPurchased 用户是否已购某 SKU：存在 已支付/已发货/已完成 订单含该 SKU
	// （好友圈分享校验；待支付/已取消不算已购）。
	HasPurchased(ctx context.Context, userID, skuID int64) (bool, error)
}

// Store 聚合仓储，作为 service 的构造入参。
type Store struct {
	Orders OrderRepository
	Items  OrderItemRepository
	Tx     TxRunner
}
