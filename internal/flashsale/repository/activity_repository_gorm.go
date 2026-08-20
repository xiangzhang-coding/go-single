package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// GORMActivityRepository 秒杀活动仓储（GORM 实现）。
type GORMActivityRepository struct {
	db *gorm.DB
}

// NewGORMActivity 构造秒杀活动仓储。
func NewGORMActivity(db *gorm.DB) *GORMActivityRepository {
	return &GORMActivityRepository{db: db}
}

func (r *GORMActivityRepository) WithinTx(ctx context.Context, fn func(tx *transaction.Handle) error) error {
	return transaction.WithinGORM(ctx, r.db, fn)
}

func (r *GORMActivityRepository) Create(ctx context.Context, a *model.Activity) error {
	return r.db.WithContext(ctx).Create(a).Error
}

func (r *GORMActivityRepository) Update(ctx context.Context, a *model.Activity) error {
	return r.update(r.db.WithContext(ctx), a)
}

func (r *GORMActivityRepository) UpdateInTx(ctx context.Context, handle *transaction.Handle, a *model.Activity) error {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return err
	}
	return tx.WithContext(ctx).Model(a).
		Select("sku_id", "title", "price", "stock", "per_user_limit", "status", "start_at", "end_at").
		Updates(a).Error
}

func (r *GORMActivityRepository) update(db *gorm.DB, a *model.Activity) error {
	return db.Model(a).Select("sku_id", "title", "price", "stock", "per_user_limit", "start_at", "end_at").Updates(a).Error
}

func (r *GORMActivityRepository) GetByID(ctx context.Context, id int64) (*model.Activity, error) {
	return r.getByID(r.db.WithContext(ctx), id)
}

func (r *GORMActivityRepository) GetByIDForUpdate(ctx context.Context, handle *transaction.Handle, id int64) (*model.Activity, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return nil, err
	}
	return r.getByID(tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}), id)
}

func (r *GORMActivityRepository) getByID(db *gorm.DB, id int64) (*model.Activity, error) {
	var a model.Activity
	if err := db.First(&a, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (r *GORMActivityRepository) List(ctx context.Context) ([]model.Activity, error) {
	var list []model.Activity
	if err := r.db.WithContext(ctx).Order("id DESC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (r *GORMActivityRepository) UpdateStatus(ctx context.Context, id int64, status string) error {
	return r.db.WithContext(ctx).Model(&model.Activity{}).Where("id = ?", id).Update("status", status).Error
}

// DeductStock 条件扣减：UPDATE ... SET stock = stock - ? WHERE id = ? AND stock >= ?。
// RowsAffected=0（活动不存在或库存不足）返回 (false, nil)，由调用方区分语义。
func (r *GORMActivityRepository) DeductStock(ctx context.Context, handle *transaction.Handle, id int64, quantity int) (bool, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return false, err
	}
	res := tx.WithContext(ctx).Model(&model.Activity{}).
		Where("id = ? AND stock >= ?", id, quantity).
		Update("stock", gorm.Expr("stock - ?", quantity))
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RestoreStock 回补库存：UPDATE ... SET stock = stock + ? WHERE id = ?
// （秒杀订单取消/超时取消回补；活动存在由订单外键 RESTRICT 保证）。
func (r *GORMActivityRepository) RestoreStock(ctx context.Context, handle *transaction.Handle, id int64, quantity int) error {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return err
	}
	return tx.WithContext(ctx).Model(&model.Activity{}).
		Where("id = ?", id).
		Update("stock", gorm.Expr("stock + ?", quantity)).Error
}

// 编译期断言：GORM 实现满足仓储接口。
var _ ActivityRepository = (*GORMActivityRepository)(nil)
var _ TxRunner = (*GORMActivityRepository)(nil)
