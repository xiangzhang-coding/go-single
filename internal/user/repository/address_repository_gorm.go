package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/user/model"
)

// GORMAddressRepository 地址簿仓储（GORM 实现）。
type GORMAddressRepository struct {
	db *gorm.DB
}

// NewGORMAddress 基于已连接的 *gorm.DB 构造地址簿仓储。
func NewGORMAddress(db *gorm.DB) *GORMAddressRepository {
	return &GORMAddressRepository{db: db}
}

func (r *GORMAddressRepository) Create(ctx context.Context, a *model.Address) error {
	return r.db.WithContext(ctx).Create(a).Error
}

func (r *GORMAddressRepository) Update(ctx context.Context, a *model.Address) error {
	return r.db.WithContext(ctx).Model(a).Select("receiver", "phone", "province", "city", "district", "detail").Updates(a).Error
}

func (r *GORMAddressRepository) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.Address{}, id).Error
}

func (r *GORMAddressRepository) GetByID(ctx context.Context, id int64) (*model.Address, error) {
	var a model.Address
	if err := r.db.WithContext(ctx).First(&a, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// ListByUser JOIN users 取默认指针派生 is_default 标记：表内不存默认状态，
// 避免与 users.default_address_id 双份状态漂移；默认地址排最前。
func (r *GORMAddressRepository) ListByUser(ctx context.Context, userID int64) ([]model.Address, error) {
	var list []model.Address
	err := r.db.WithContext(ctx).
		Table("user_addresses AS a").
		Joins("JOIN users AS u ON u.id = a.user_id").
		Select("a.*, (u.default_address_id = a.id) AS is_default").
		Where("a.user_id = ?", userID).
		Order("is_default DESC, a.id DESC").
		Scan(&list).Error
	return list, err
}

func (r *GORMAddressRepository) CountByUser(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.Address{}).
		Where("user_id = ?", userID).Count(&n).Error
	return n, err
}

// GetDefaultAddress 按 users.default_address_id 指针读取默认地址；
// 无默认地址（指针为空或地址已删）返回 (nil, nil)。
func (r *GORMAddressRepository) GetDefaultAddress(ctx context.Context, userID int64) (*model.Address, error) {
	var a model.Address
	err := r.db.WithContext(ctx).
		Table("user_addresses AS a").
		Joins("JOIN users AS u ON u.id = a.user_id AND u.default_address_id = a.id").
		Select("a.*").
		Where("a.user_id = ?", userID).
		Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// SetDefault 单条 UPDATE 原子切换默认指向；地址归属由 service 先校验。
func (r *GORMAddressRepository) SetDefault(ctx context.Context, userID, addressID int64) error {
	return r.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", userID).Update("default_address_id", addressID).Error
}

// EnsureDefaultExists 删除默认地址后自愈：默认指针为空（删除默认地址时 FK 置空）
// 且仍有余下地址时，把最新一条设为默认。单条 UPDATE 幂等，非默认删除场景为空操作。
func (r *GORMAddressRepository) EnsureDefaultExists(ctx context.Context, userID int64) error {
	return r.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ? AND default_address_id IS NULL AND EXISTS ("+
			"SELECT 1 FROM user_addresses WHERE user_id = ?)", userID, userID).
		Update("default_address_id", gorm.Expr("(SELECT id FROM user_addresses WHERE user_id = ? ORDER BY id DESC LIMIT 1)", userID)).Error
}

var _ AddressRepository = (*GORMAddressRepository)(nil)
