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

// UserRepository 用户数据访问接口。
type UserRepository interface {
	Create(ctx context.Context, u *model.User) error
	GetByUsername(ctx context.Context, username string) (*model.User, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	// UpdateProfile 按主键更新个人资料字段（nickname/avatar_url，零值生效允许清空）。
	UpdateProfile(ctx context.Context, u *model.User) error
	// HasAvatarURL 判断托管引用是否仍绑定为用户头像。
	HasAvatarURL(ctx context.Context, reference string) (bool, error)
	// SearchByUsername 按用户名前缀搜索（"加好友"发现入口），id 升序
	// 返回至多 limit 条；prefix 为空或 limit <= 0 返回空列表，不触达数据库。
	SearchByUsername(ctx context.Context, prefix string, limit int) ([]model.User, error)
}

// Store 聚合 user 模块各仓储，作为 service 的构造入参。
type Store struct {
	Users     UserRepository
	Addresses AddressRepository
}
