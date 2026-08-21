package repository

import (
	"context"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

type DeleteAddressResult uint8

const (
	DeleteAddressDeleted DeleteAddressResult = iota + 1
	DeleteAddressNotFound
	DeleteAddressForbidden
)

type SetDefaultAddressResult uint8

const (
	SetDefaultAddressSet SetDefaultAddressResult = iota + 1
	SetDefaultAddressNotFound
	SetDefaultAddressForbidden
)

// AddressRepository 地址簿数据访问接口。
type AddressRepository interface {
	// CreateWithDefault serializes a user's address mutations, creates the row,
	// and atomically assigns the first or explicitly requested default.
	CreateWithDefault(ctx context.Context, a *model.Address, requestedDefault bool) (isDefault bool, err error)
	Update(ctx context.Context, a *model.Address) error
	// DeleteAndEnsureDefault validates ownership, deletes the row, and promotes
	// the newest remaining address in one transaction.
	DeleteAndEnsureDefault(ctx context.Context, userID, id int64) (DeleteAddressResult, error)
	// GetByID 读取单条地址。
	GetByID(ctx context.Context, id int64) (*model.Address, error)
	// GetDefaultAddress 读取用户默认地址（JOIN users.default_address_id 指针）；
	// 无默认地址返回 (nil, nil)。
	GetDefaultAddress(ctx context.Context, userID int64) (*model.Address, error)
	// ListByUser 我的地址列表（JOIN users 派生 is_default 标记，默认地址排最前）。
	ListByUser(ctx context.Context, userID int64) ([]model.Address, error)
	// SetDefaultOwned serializes with create/delete, validates ownership, and
	// atomically switches the user's default pointer.
	SetDefaultOwned(ctx context.Context, userID, addressID int64) (SetDefaultAddressResult, error)
}
