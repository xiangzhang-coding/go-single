package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
)

// GORMRequestRepository GORM 实现的好友申请仓储。
type GORMRequestRepository struct {
	db *gorm.DB
}

// NewGORMRequest 基于已连接的 *gorm.DB 构造好友申请仓储。
func NewGORMRequest(db *gorm.DB) *GORMRequestRepository {
	return &GORMRequestRepository{db: db}
}

func (r *GORMRequestRepository) Create(ctx context.Context, req *model.FriendRequest) error {
	if err := r.db.WithContext(ctx).Create(req).Error; err != nil {
		if isDuplicateKey(err) {
			return ErrRequestPairExists
		}
		return err
	}
	return nil
}

func (r *GORMRequestRepository) GetByID(ctx context.Context, id int64) (*model.FriendRequest, error) {
	return r.findOne(ctx, "id = ?", id)
}

func (r *GORMRequestRepository) GetByPair(ctx context.Context, fromUserID, toUserID int64) (*model.FriendRequest, error) {
	return r.findOne(ctx, "from_user_id = ? AND to_user_id = ?", fromUserID, toUserID)
}

func (r *GORMRequestRepository) UpdateStatus(ctx context.Context, id int64, status string) error {
	return r.db.WithContext(ctx).Model(&model.FriendRequest{}).
		Where("id = ?", id).Update("status", status).Error
}

func (r *GORMRequestRepository) ListByUser(ctx context.Context, userID int64, scope, status string) ([]model.FriendRequest, error) {
	q := r.db.WithContext(ctx)
	switch scope {
	case "incoming":
		q = q.Where("to_user_id = ?", userID)
	default:
		q = q.Where("from_user_id = ?", userID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var list []model.FriendRequest
	if err := q.Order("id DESC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (r *GORMRequestRepository) findOne(ctx context.Context, query string, args ...any) (*model.FriendRequest, error) {
	var req model.FriendRequest
	if err := r.db.WithContext(ctx).Where(query, args...).First(&req).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &req, nil
}

var _ FriendRequestRepository = (*GORMRequestRepository)(nil)

// GORMFriendshipRepository GORM 实现的好友关系仓储。
type GORMFriendshipRepository struct {
	db *gorm.DB
}

// NewGORMFriendship 基于已连接的 *gorm.DB 构造好友关系仓储。
func NewGORMFriendship(db *gorm.DB) *GORMFriendshipRepository {
	return &GORMFriendshipRepository{db: db}
}

// CreatePair 一次写入双向两行；任一行撞唯一键（并发重复通过）即回滚返回 ErrFriendshipExists。
func (r *GORMFriendshipRepository) CreatePair(ctx context.Context, userID, friendID int64) error {
	pair := []model.Friendship{
		{UserID: userID, FriendID: friendID},
		{UserID: friendID, FriendID: userID},
	}
	if err := r.db.WithContext(ctx).Create(&pair).Error; err != nil {
		if isDuplicateKey(err) {
			return ErrFriendshipExists
		}
		return err
	}
	return nil
}

func (r *GORMFriendshipRepository) Exists(ctx context.Context, userID, friendID int64) (bool, error) {
	var cnt int64
	err := r.db.WithContext(ctx).Model(&model.Friendship{}).
		Where("user_id = ? AND friend_id = ?", userID, friendID).Count(&cnt).Error
	return cnt > 0, err
}

func (r *GORMFriendshipRepository) ListByUser(ctx context.Context, userID int64) ([]model.Friendship, error) {
	var list []model.Friendship
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).
		Order("id DESC").Find(&list).Error
	return list, err
}

var _ FriendshipRepository = (*GORMFriendshipRepository)(nil)

// isDuplicateKey MySQL 唯一键冲突（1062）。
func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
