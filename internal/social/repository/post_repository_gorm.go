package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
)

// GORMPostRepository GORM 实现的动态仓储。
type GORMPostRepository struct {
	db *gorm.DB
}

// NewGORMPost 基于已连接的 *gorm.DB 构造动态仓储。
func NewGORMPost(db *gorm.DB) *GORMPostRepository {
	return &GORMPostRepository{db: db}
}

func (r *GORMPostRepository) Create(ctx context.Context, post *model.Post) error {
	return r.db.WithContext(ctx).Create(post).Error
}

func (r *GORMPostRepository) GetByID(ctx context.Context, id int64) (*model.Post, error) {
	var post model.Post
	if err := r.db.WithContext(ctx).First(&post, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &post, nil
}

func (r *GORMPostRepository) GetByImageURL(ctx context.Context, reference string) (*model.Post, error) {
	var post model.Post
	if err := r.db.WithContext(ctx).First(&post, "image_url = ?", reference).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &post, nil
}

// ListByUsers 时间线拉取：好友列表 join 动态表（user_id IN 好友集合），
// 时间倒序分页（created_at DESC, id DESC——id 为单调自增，作同秒并列时的稳定次序）。
func (r *GORMPostRepository) ListByUsers(ctx context.Context, userIDs []int64, offset, limit int) ([]model.Post, int64, error) {
	if len(userIDs) == 0 {
		return []model.Post{}, 0, nil
	}
	var total int64
	q := r.db.WithContext(ctx).Model(&model.Post{}).Where("user_id IN ?", userIDs)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []model.Post
	if err := q.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListByUser 我的动态：与时间线同序（created_at DESC, id DESC），个人页展示用。
func (r *GORMPostRepository) ListByUser(ctx context.Context, userID int64, offset, limit int) ([]model.Post, int64, error) {
	var total int64
	q := r.db.WithContext(ctx).Model(&model.Post{}).Where("user_id = ?", userID)
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []model.Post
	if err := q.Order("created_at DESC, id DESC").Offset(offset).Limit(limit).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (r *GORMPostRepository) Delete(ctx context.Context, id int64) (bool, error) {
	res := r.db.WithContext(ctx).Delete(&model.Post{}, "id = ?", id)
	return res.RowsAffected == 1, res.Error
}

var _ PostRepository = (*GORMPostRepository)(nil)
