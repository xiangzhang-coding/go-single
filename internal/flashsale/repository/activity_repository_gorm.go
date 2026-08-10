package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
)

// GORMActivityRepository 秒杀活动仓储（GORM 实现）。
type GORMActivityRepository struct {
	db *gorm.DB
}

// NewGORMActivity 构造秒杀活动仓储。
func NewGORMActivity(db *gorm.DB) *GORMActivityRepository {
	return &GORMActivityRepository{db: db}
}

func (r *GORMActivityRepository) Create(ctx context.Context, a *model.Activity) error {
	return r.db.WithContext(ctx).Create(a).Error
}

func (r *GORMActivityRepository) Update(ctx context.Context, a *model.Activity) error {
	return r.db.WithContext(ctx).Model(a).Select("sku_id", "title", "price", "stock", "per_user_limit", "start_at", "end_at").Updates(a).Error
}

func (r *GORMActivityRepository) GetByID(ctx context.Context, id int64) (*model.Activity, error) {
	var a model.Activity
	if err := r.db.WithContext(ctx).First(&a, id).Error; err != nil {
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

// 编译期断言：GORM 实现满足仓储接口。
var _ ActivityRepository = (*GORMActivityRepository)(nil)
