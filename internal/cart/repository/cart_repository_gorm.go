package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/cart/model"
)

// isDuplicate MySQL 1062：唯一键冲突。
func isDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// GORMCartItemRepository 购物车条目仓储（GORM 实现）。
type GORMCartItemRepository struct {
	db *gorm.DB
}

// NewGORMCartItem 构造购物车条目仓储。
func NewGORMCartItem(db *gorm.DB) *GORMCartItemRepository {
	return &GORMCartItemRepository{db: db}
}

func (r *GORMCartItemRepository) Create(ctx context.Context, item *model.CartItem) error {
	if err := r.db.WithContext(ctx).Create(item).Error; err != nil {
		if isDuplicate(err) {
			return ErrCartItemExists
		}
		return err
	}
	return nil
}

func (r *GORMCartItemRepository) GetByID(ctx context.Context, id int64) (*model.CartItem, error) {
	var item model.CartItem
	if err := r.db.WithContext(ctx).First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (r *GORMCartItemRepository) GetByUserAndSKU(ctx context.Context, userID, skuID int64) (*model.CartItem, error) {
	var item model.CartItem
	if err := r.db.WithContext(ctx).Where("user_id = ? AND sku_id = ?", userID, skuID).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &item, nil
}

func (r *GORMCartItemRepository) UpdateQuantity(ctx context.Context, id int64, quantity int) error {
	return r.db.WithContext(ctx).Model(&model.CartItem{}).Where("id = ?", id).
		Update("quantity", quantity).Error
}

func (r *GORMCartItemRepository) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.CartItem{}, id).Error
}

// ListByUser 一次查询拼装列表读模型：cart_items JOIN skus JOIN products。
// 跨表读取仅用于展示快照（不修改商品域数据），避免逐条补查的 N+1。
func (r *GORMCartItemRepository) ListByUser(ctx context.Context, userID int64) ([]model.CartItemView, error) {
	views := make([]model.CartItemView, 0)
	err := r.db.WithContext(ctx).Raw(`
		SELECT ci.id, ci.user_id, ci.sku_id, ci.quantity, ci.created_at, ci.updated_at,
		       s.product_id, s.specs, s.price, s.stock,
		       p.title
		FROM cart_items ci
		JOIN skus s ON s.id = ci.sku_id
		JOIN products p ON p.id = s.product_id
		WHERE ci.user_id = ?
		ORDER BY ci.id DESC`, userID).Scan(&views).Error
	if err != nil {
		return nil, err
	}
	return views, nil
}

var _ CartItemRepository = (*GORMCartItemRepository)(nil)
