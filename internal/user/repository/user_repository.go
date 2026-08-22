// Package repository 定义 user 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

// ErrUsernameExists 用户名已存在（唯一约束冲突）。
var ErrUsernameExists = errors.New("username already exists")

// ErrUserNotFound 按 ID 更新时用户不存在（并发删除的极端场景）。
var ErrUserNotFound = errors.New("user not found")

// ProfilePatch carries only fields explicitly submitted by PATCH /users/me.
// A non-nil pointer with an empty value means clear the field.
type ProfilePatch struct {
	Nickname  *string
	AvatarURL *string
}

// UserRepository 用户数据访问接口。
type UserRepository interface {
	Create(ctx context.Context, u *model.User) error
	GetByUsername(ctx context.Context, username string) (*model.User, error)
	// UsernameRateLimitKey returns MySQL's equality weight for the username,
	// exactly matching the unique index's utf8mb4_unicode_ci collation.
	UsernameRateLimitKey(ctx context.Context, username string) (string, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	// UpdateProfile 按主键只更新显式提交的资料字段，避免并发部分更新互相覆盖。
	UpdateProfile(ctx context.Context, userID int64, patch ProfilePatch) error
	// HasAvatarURL 判断托管引用是否仍绑定为用户头像。
	HasAvatarURL(ctx context.Context, reference string) (bool, error)
	// SearchByUsername 按用户名前缀搜索（"加好友"发现入口），在 LIMIT 前排除
	// excludeUserID 并按 id 升序返回；prefix 为空或 limit <= 0 不触达数据库。
	SearchByUsername(ctx context.Context, prefix string, excludeUserID int64, limit int) ([]model.PublicUser, error)
	// GetPublicByIDs 批量读取可公开资料；空 id 集合不触达数据库。
	GetPublicByIDs(ctx context.Context, ids []int64) ([]model.PublicUser, error)
}

// Store 聚合 user 模块各仓储，作为 service 的构造入参。
type Store struct {
	Users     UserRepository
	Addresses AddressRepository
}
