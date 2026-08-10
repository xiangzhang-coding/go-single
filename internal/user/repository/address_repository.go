package repository

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

// AddressRepository 地址簿数据访问接口。
type AddressRepository interface {
	Create(ctx context.Context, a *model.Address) error
	Update(ctx context.Context, a *model.Address) error
	Delete(ctx context.Context, id int64) error
	GetByID(ctx context.Context, id int64) (*model.Address, error)
	// ListByUser 我的地址列表（JOIN users 派生 is_default 标记，默认地址排最前）。
	ListByUser(ctx context.Context, userID int64) ([]model.Address, error)
	CountByUser(ctx context.Context, userID int64) (int64, error)
	// SetDefault 原子切换默认指向（单条 UPDATE users，唯一性由构造保证）。
	SetDefault(ctx context.Context, userID, addressID int64) error
	// EnsureDefaultExists 删除默认地址后的自愈：无默认指向且仍有地址时，
	// 将最新一条设为默认（幂等，非默认删除时为空操作）。
	EnsureDefaultExists(ctx context.Context, userID int64) error
}
