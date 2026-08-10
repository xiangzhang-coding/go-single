// Package repository 定义 cart 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/cart/model"
)

var (
	// ErrCartItemExists 同 (user_id, sku_id) 条目已存在（唯一键冲突），
	// 服务层据此重查后走合并路径（并发加购兜底）。
	ErrCartItemExists = errors.New("cart item already exists")
)

// CartItemRepository 购物车条目数据访问接口。
type CartItemRepository interface {
	Create(ctx context.Context, item *model.CartItem) error
	GetByID(ctx context.Context, id int64) (*model.CartItem, error)
	GetByUserAndSKU(ctx context.Context, userID, skuID int64) (*model.CartItem, error)
	// UpdateQuantity 改量（修改数量时校验归属在 service 层完成）。
	UpdateQuantity(ctx context.Context, id int64, quantity int) error
	Delete(ctx context.Context, id int64) error
	// ListByUser 我的购物车列表：条目 + SKU/商品只读快照（跨表读模型，一次查询）。
	ListByUser(ctx context.Context, userID int64) ([]model.CartItemView, error)
	// DeleteBySKUs 事务内批量删除指定 SKU 的条目（结算后清理已购），
	// tx 由调用方（order 模块）开启，与订单创建同事务原子提交。
	DeleteBySKUs(ctx context.Context, tx *gorm.DB, userID int64, skuIDs []int64) error
}

// Store 聚合仓储实现，作为 service 的构造入参。
type Store struct {
	Items CartItemRepository
}
