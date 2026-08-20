// Package repository 定义 cart 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/cart/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

var ErrCartItemNotFound = errors.New("cart item not found")

// CartItemRepository 购物车条目数据访问接口。
type CartItemRepository interface {
	// AddQuantity 原子创建或累加同一用户与 SKU 的条目，并将结果限制在 maxQuantity。
	AddQuantity(ctx context.Context, userID, skuID int64, quantity, maxQuantity int) (*model.CartItem, error)
	GetByID(ctx context.Context, id int64) (*model.CartItem, error)
	// UpdateQuantity 改量（修改数量时校验归属在 service 层完成）。
	UpdateQuantity(ctx context.Context, id int64, quantity int) error
	Delete(ctx context.Context, id int64) error
	// ListByUser 我的购物车列表：条目 + SKU/商品只读快照（跨表读模型，一次查询）。
	ListByUser(ctx context.Context, userID int64) ([]model.CartItemView, error)
	// LockByUser 在结算事务内读取并锁定当前条目，避免读取数量后被并发改量。
	LockByUser(ctx context.Context, tx *transaction.Handle, userID int64) ([]model.CartItem, error)
	// DeleteByIDs 事务内按条目 ID 删除已结算的行，避免按 SKU 删除并发新增/修改的条目。
	DeleteByIDs(ctx context.Context, tx *transaction.Handle, userID int64, itemIDs []int64) error
}

// Store 聚合仓储实现，作为 service 的构造入参。
type Store struct {
	Items CartItemRepository
}
