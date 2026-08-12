package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

// GORM 实现的 user 仓储。
type GORMUserRepository struct {
	db *gorm.DB
}

// NewGORM 基于已连接的 *gorm.DB 构造用户仓储。
func NewGORM(db *gorm.DB) *GORMUserRepository {
	return &GORMUserRepository{db: db}
}

func (r *GORMUserRepository) Create(ctx context.Context, u *model.User) error {
	if err := r.db.WithContext(ctx).Create(u).Error; err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return ErrUsernameExists
		}
		return err
	}
	return nil
}

func (r *GORMUserRepository) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	return r.findOne(ctx, "username = ?", username)
}

func (r *GORMUserRepository) GetByID(ctx context.Context, id int64) (*model.User, error) {
	return r.findOne(ctx, "id = ?", id)
}

// SearchByUsername 前缀搜索：username LIKE 'prefix%'，id 升序限量返回。
func (r *GORMUserRepository) SearchByUsername(ctx context.Context, prefix string, limit int) ([]model.User, error) {
	if prefix == "" || limit <= 0 {
		return []model.User{}, nil
	}
	var users []model.User
	if err := r.db.WithContext(ctx).Where("username LIKE ?", prefix+"%").Order("id ASC").Limit(limit).Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

func (r *GORMUserRepository) findOne(ctx context.Context, query string, args ...any) (*model.User, error) {
	var u model.User
	if err := r.db.WithContext(ctx).Where(query, args...).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

var _ UserRepository = (*GORMUserRepository)(nil)
