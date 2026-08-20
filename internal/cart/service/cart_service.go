// Package service 承载 cart 模块业务：登录用户加购/改量/删条目/查列表。
// 加购校验 SKU 存在与所属商品上架（跨模块经 product 服务接口进程内调用）；
// 列表由仓储跨表拼装展示快照（读模型）；条目修改强制 owner 校验防 IDOR。
package service

import (
	"context"
	"errors"
	"fmt"

	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"

	"github.com/xiangzhang-coding/go-single/internal/cart/model"
	"github.com/xiangzhang-coding/go-single/internal/cart/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrSKUNotFound       = errors.New("sku not found")
	ErrSKUUnavailable    = errors.New("sku product is not on sale")
	ErrCartItemNotFound  = errors.New("cart item not found")
	ErrCartItemForbidden = errors.New("cart item does not belong to user")
)

// maxQuantity 单条目数量上限（加购合并与改量共用）。
const maxQuantity = 99

// ProductService product 模块暴露的最小查询接口（跨模块进程内调用，面向接口非 HTTP；
// productSvc 天然满足，未来拆模块时换实现即可）。
type ProductService interface {
	// GetSKU 校验 SKU 存在。
	GetSKU(ctx context.Context, id int64) (*productmodel.SKU, error)
	// GetProduct 直读商品事实；加购可售性不能依赖详情缓存。
	GetProduct(ctx context.Context, id int64) (*productmodel.Product, error)
}

// Service cart 模块的业务接口。
type Service interface {
	// AddItem 加购：校验 SKU 存在/上架；重复加购同一 SKU 合并数量（上限 99）。
	AddItem(ctx context.Context, userID, skuID int64, quantity int) (*model.CartItem, error)
	// UpdateQuantity 修改数量：仅条目归属人可操作（owner 校验）。
	UpdateQuantity(ctx context.Context, userID, itemID int64, quantity int) error
	// DeleteItem 删除条目：仅条目归属人可操作（owner 校验）。
	DeleteItem(ctx context.Context, userID, itemID int64) error
	// ListItems 我的购物车列表（含 SKU/商品展示快照，新加购的排最前）。
	ListItems(ctx context.Context, userID int64) ([]model.CartItemView, error)
	// LockItems 结算事务内锁定并读取当前购物车条目。
	LockItems(ctx context.Context, tx *transaction.Handle, userID int64) ([]model.CartItem, error)
	// DeletePurchased 事务内删除已锁定、已结算的条目。
	DeletePurchased(ctx context.Context, tx *transaction.Handle, userID int64, itemIDs []int64) error
}

type cartService struct {
	store    repository.Store
	products ProductService
}

// New 构造购物车服务。
func New(store repository.Store, products ProductService) Service {
	return &cartService{store: store, products: products}
}

// AddItem 加购流程：数量校验 → SKU 存在校验 → 直读商品状态校验上架
// → 数据库原子创建或累加数量并封顶。
func (s *cartService) AddItem(ctx context.Context, userID, skuID int64, quantity int) (*model.CartItem, error) {
	if quantity < 1 || quantity > maxQuantity {
		return nil, fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
	}
	if userID <= 0 || skuID <= 0 {
		return nil, fmt.Errorf("%w: invalid id", ErrInvalidInput)
	}

	sku, err := s.products.GetSKU(ctx, skuID)
	if err != nil {
		if errors.Is(err, productsvc.ErrSKUNotFound) {
			return nil, ErrSKUNotFound
		}
		return nil, err
	}
	if sku == nil {
		return nil, ErrSKUNotFound
	}
	product, err := s.products.GetProduct(ctx, sku.ProductID)
	if err != nil {
		if errors.Is(err, productsvc.ErrProductNotFound) {
			return nil, ErrSKUUnavailable
		}
		return nil, err
	}
	if product == nil || !product.IsOnSale() {
		return nil, ErrSKUUnavailable
	}

	return s.store.Items.AddQuantity(ctx, userID, skuID, quantity, maxQuantity)
}

// UpdateQuantity 改量：数量校验 + 条目归属校验后更新。
func (s *cartService) UpdateQuantity(ctx context.Context, userID, itemID int64, quantity int) error {
	if quantity < 1 || quantity > maxQuantity {
		return fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
	}
	if itemID <= 0 {
		return fmt.Errorf("%w: invalid id", ErrInvalidInput)
	}
	if err := s.ensureOwned(ctx, userID, itemID); err != nil {
		return err
	}
	if err := s.store.Items.UpdateQuantity(ctx, itemID, quantity); err != nil {
		if errors.Is(err, repository.ErrCartItemNotFound) {
			return ErrCartItemNotFound
		}
		return err
	}
	return nil
}

// DeleteItem 删条目：条目归属校验后删除。
func (s *cartService) DeleteItem(ctx context.Context, userID, itemID int64) error {
	if itemID <= 0 {
		return fmt.Errorf("%w: invalid id", ErrInvalidInput)
	}
	if err := s.ensureOwned(ctx, userID, itemID); err != nil {
		return err
	}
	return s.store.Items.Delete(ctx, itemID)
}

func (s *cartService) ListItems(ctx context.Context, userID int64) ([]model.CartItemView, error) {
	return s.store.Items.ListByUser(ctx, userID)
}

// LockItems 结算事务内锁定当前购物车条目，调用方（order 模块）负责开启事务。
func (s *cartService) LockItems(ctx context.Context, tx *transaction.Handle, userID int64) ([]model.CartItem, error) {
	return s.store.Items.LockByUser(ctx, tx, userID)
}

// DeletePurchased 按锁定的条目 ID 清理，避免按 SKU 误删并发变更。
func (s *cartService) DeletePurchased(ctx context.Context, tx *transaction.Handle, userID int64, itemIDs []int64) error {
	return s.store.Items.DeleteByIDs(ctx, tx, userID, itemIDs)
}

// ensureOwned 对象级授权（防 IDOR）：条目不存在 404；归属他人 403。
func (s *cartService) ensureOwned(ctx context.Context, userID, itemID int64) error {
	item, err := s.store.Items.GetByID(ctx, itemID)
	if err != nil {
		return err
	}
	if item == nil {
		return ErrCartItemNotFound
	}
	if item.UserID != userID {
		return ErrCartItemForbidden
	}
	return nil
}
