package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
	res := r.db.WithContext(ctx).Model(&model.CartItem{}).Where("id = ?", id).
		Update("quantity", quantity)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// MySQL 将"改成原值"报告为 0 行；先确认条目仍存在，避免把合法幂等改量误报 404。
		var item model.CartItem
		return r.db.WithContext(ctx).Select("id").First(&item, id).Error
	}
	return nil
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

// LockByUser 结算事务内读取当前条目并加排他锁。
// 在 InnoDB 下，用户索引范围锁住后，改量/新增会等待本次结算提交，
// 结算使用的数量与随后删除的条目 ID 保持同一事务快照。
func (r *GORMCartItemRepository) LockByUser(ctx context.Context, tx *gorm.DB, userID int64) ([]model.CartItem, error) {
	items := make([]model.CartItem, 0)
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ?", userID).Order("id ASC").Find(&items).Error
	return items, err
}

// DeleteByIDs 结算后按锁定的条目 ID 清理（与订单创建同一事务）。
func (r *GORMCartItemRepository) DeleteByIDs(ctx context.Context, tx *gorm.DB, userID int64, itemIDs []int64) error {
	if len(itemIDs) == 0 {
		return nil
	}
	return tx.WithContext(ctx).Where("user_id = ? AND id IN ?", userID, itemIDs).
		Delete(&model.CartItem{}).Error
}

var _ CartItemRepository = (*GORMCartItemRepository)(nil)
