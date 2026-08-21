package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

func (r *GORMAddressRepository) CreateWithDefault(ctx context.Context, a *model.Address, requestedDefault bool) (bool, error) {
	var isDefault bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "default_address_id").First(&user, a.UserID).Error; err != nil {
			return err
		}
		if err := tx.Create(a).Error; err != nil {
			return err
		}
		isDefault = requestedDefault || user.DefaultAddressID == nil
		if !isDefault {
			return nil
		}
		return tx.Model(&model.User{}).Where("id = ?", a.UserID).
			Update("default_address_id", a.ID).Error
	})
	return isDefault, err
}

func (r *GORMAddressRepository) Update(ctx context.Context, a *model.Address) error {
	return r.db.WithContext(ctx).Model(a).Select("receiver", "phone", "province", "city", "district", "detail").Updates(a).Error
}

func (r *GORMAddressRepository) DeleteAndEnsureDefault(ctx context.Context, userID, id int64) (DeleteAddressResult, error) {
	result := DeleteAddressNotFound
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "default_address_id").First(&user, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		var address model.Address
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&address, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if address.UserID != userID {
			result = DeleteAddressForbidden
			return nil
		}
		if err := tx.Delete(&address).Error; err != nil {
			return err
		}
		result = DeleteAddressDeleted

		if user.DefaultAddressID != nil && *user.DefaultAddressID != id {
			return nil
		}
		var replacement model.Address
		err := tx.Where("user_id = ?", userID).Order("id DESC").Take(&replacement).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Model(&model.User{}).Where("id = ?", userID).
				Update("default_address_id", nil).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&model.User{}).Where("id = ?", userID).
			Update("default_address_id", replacement.ID).Error
	})
	return result, err
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

func (r *GORMAddressRepository) SetDefaultOwned(ctx context.Context, userID, addressID int64) (SetDefaultAddressResult, error) {
	result := SetDefaultAddressNotFound
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&user, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var address model.Address
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "user_id").First(&address, addressID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if address.UserID != userID {
			result = SetDefaultAddressForbidden
			return nil
		}
		if err := tx.Model(&model.User{}).Where("id = ?", userID).
			Update("default_address_id", addressID).Error; err != nil {
			return err
		}
		result = SetDefaultAddressSet
		return nil
	})
	return result, err
}

var _ AddressRepository = (*GORMAddressRepository)(nil)
