package repository

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/social/model"
)

// PostRepository 好友圈动态数据访问接口。
type PostRepository interface {
	// Create 落库一条动态。
	Create(ctx context.Context, post *model.Post) error
	// GetByID 按 ID 读取；不存在返回 (nil, nil)。
	GetByID(ctx context.Context, id int64) (*model.Post, error)
	// ListByUsers 好友圈时间线：仅指定用户（好友）的动态，
	// 时间倒序（created_at DESC, id DESC 稳定分页），返回条目与总数。
	// userIDs 为空返回空列表（总数为 0），不触达数据库。
	ListByUsers(ctx context.Context, userIDs []int64, offset, limit int) ([]model.Post, int64, error)
	// ListByUser 我的动态：时间倒序分页，返回条目与总数。
	ListByUser(ctx context.Context, userID int64, offset, limit int) ([]model.Post, int64, error)
	// Delete 删除一条动态，返回是否实际删除（RowsAffected；
	// 归属校验在 service 层，false = 读取后已被并发删除）。
	Delete(ctx context.Context, id int64) (bool, error)
}
