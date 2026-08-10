// Package repository 定义 product 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 拆分为类目/商品/SKU 三个小接口，便于各聚合独立测试替身。
package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/product/model"
)

var (
	// ErrCategoryNameExists 类目名已存在（唯一约束冲突）。
	ErrCategoryNameExists = errors.New("category name already exists")
	// ErrCategoryInUse 类目下仍有商品，禁止删除。
	ErrCategoryInUse = errors.New("category in use")
)

// CategoryRepository 类目数据访问接口。
type CategoryRepository interface {
	Create(ctx context.Context, c *model.Category) error
	Update(ctx context.Context, c *model.Category) error
	Delete(ctx context.Context, id int64) error
	GetByID(ctx context.Context, id int64) (*model.Category, error)
	List(ctx context.Context) ([]model.Category, error)
}

// ProductRepository 商品(SPU)数据访问接口。
type ProductRepository interface {
	Create(ctx context.Context, p *model.Product) error
	Update(ctx context.Context, p *model.Product) error
	// SetStatus 上架/下架。
	SetStatus(ctx context.Context, id int64, status string) error
	GetByID(ctx context.Context, id int64) (*model.Product, error)
	// List 按类目（nil 为全部）与状态分页查询，返回条目与总数。
	List(ctx context.Context, categoryID *int64, status string, offset, limit int) ([]model.Product, int64, error)
	// CountByCategory 统计类目下商品数（删除类目前校验）。
	CountByCategory(ctx context.Context, categoryID int64) (int64, error)
}

// SKURepository SKU 数据访问接口。
type SKURepository interface {
	Create(ctx context.Context, s *model.SKU) error
	Update(ctx context.Context, s *model.SKU) error
	Delete(ctx context.Context, id int64) error
	GetByID(ctx context.Context, id int64) (*model.SKU, error)
	ListByProduct(ctx context.Context, productID int64) ([]model.SKU, error)
	// DeductStock 事务内条件扣减库存（stock>=quantity 才更新，防超卖）：
	// 返回是否实际扣减。tx 由调用方（order 模块）开启，同一事务保证
	// 订单创建与库存扣减的原子性。
	DeductStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) (bool, error)
	// RestoreStock 事务内回补库存（取消订单回补；SKU 已删则空操作）。
	RestoreStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) error
}

// Store 聚合三个仓储的具体实现，作为 service 的构造入参。
type Store struct {
	Category CategoryRepository
	Product  ProductRepository
	SKU      SKURepository
}
