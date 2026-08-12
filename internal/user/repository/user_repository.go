// Package repository 定义 user 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

// ErrUsernameExists 用户名已存在（唯一约束冲突）。
var ErrUsernameExists = errors.New("username already exists")

// UserRepository 用户数据访问接口。
type UserRepository interface {
	Create(ctx context.Context, u *model.User) error
	GetByUsername(ctx context.Context, username string) (*model.User, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	// SearchByUsername 按用户名前缀搜索（"加好友"发现入口），id 升序
	// 返回至多 limit 条；prefix 为空或 limit <= 0 返回空列表，不触达数据库。
	SearchByUsername(ctx context.Context, prefix string, limit int) ([]model.User, error)
}

// Store 聚合 user 模块各仓储，作为 service 的构造入参。
type Store struct {
	Users     UserRepository
	Addresses AddressRepository
}
