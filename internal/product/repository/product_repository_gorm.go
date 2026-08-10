package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/product/model"
)

func isDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

func isFKRestrict(err error) bool {
	var mysqlErr *mysql.MySQLError
	// 1451: 外键约束阻止删除（类目下仍有商品）。
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1451
}

// GORMCategoryRepository 类目仓储（GORM 实现）。
type GORMCategoryRepository struct {
	db *gorm.DB
}

// NewGORMCategory 构造类目仓储。
func NewGORMCategory(db *gorm.DB) *GORMCategoryRepository {
	return &GORMCategoryRepository{db: db}
}

func (r *GORMCategoryRepository) Create(ctx context.Context, c *model.Category) error {
	if err := r.db.WithContext(ctx).Create(c).Error; err != nil {
		if isDuplicate(err) {
			return ErrCategoryNameExists
		}
		return err
	}
	return nil
}

func (r *GORMCategoryRepository) Update(ctx context.Context, c *model.Category) error {
	if err := r.db.WithContext(ctx).Model(c).Select("name").Updates(c).Error; err != nil {
		if isDuplicate(err) {
			return ErrCategoryNameExists
		}
		return err
	}
	return nil
}

func (r *GORMCategoryRepository) Delete(ctx context.Context, id int64) error {
	if err := r.db.WithContext(ctx).Delete(&model.Category{}, id).Error; err != nil {
		if isFKRestrict(err) {
			return ErrCategoryInUse
		}
		return err
	}
	return nil
}

func (r *GORMCategoryRepository) GetByID(ctx context.Context, id int64) (*model.Category, error) {
	var c model.Category
	if err := r.db.WithContext(ctx).First(&c, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

func (r *GORMCategoryRepository) List(ctx context.Context) ([]model.Category, error) {
	var list []model.Category
	if err := r.db.WithContext(ctx).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// GORMProductRepository 商品(SPU)仓储（GORM 实现）。
type GORMProductRepository struct {
	db *gorm.DB
}

// NewGORMProduct 构造商品仓储。
func NewGORMProduct(db *gorm.DB) *GORMProductRepository {
	return &GORMProductRepository{db: db}
}

func (r *GORMProductRepository) Create(ctx context.Context, p *model.Product) error {
	return r.db.WithContext(ctx).Create(p).Error
}

func (r *GORMProductRepository) Update(ctx context.Context, p *model.Product) error {
	return r.db.WithContext(ctx).Model(p).Select("category_id", "title", "description").Updates(p).Error
}

func (r *GORMProductRepository) SetStatus(ctx context.Context, id int64, status string) error {
	return r.db.WithContext(ctx).Model(&model.Product{}).Where("id = ?", id).
		Update("status", status).Error
}

func (r *GORMProductRepository) GetByID(ctx context.Context, id int64) (*model.Product, error) {
	var p model.Product
	if err := r.db.WithContext(ctx).First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (r *GORMProductRepository) List(ctx context.Context, categoryID *int64, status string, offset, limit int) ([]model.Product, int64, error) {
	q := r.db.WithContext(ctx).Model(&model.Product{})
	if categoryID != nil {
		q = q.Where("category_id = ?", *categoryID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var list []model.Product
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

func (r *GORMProductRepository) CountByCategory(ctx context.Context, categoryID int64) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.Product{}).
		Where("category_id = ?", categoryID).Count(&n).Error
	return n, err
}

// GORMSKURepository SKU 仓储（GORM 实现）。
type GORMSKURepository struct {
	db *gorm.DB
}

// NewGORMSKU 构造 SKU 仓储。
func NewGORMSKU(db *gorm.DB) *GORMSKURepository {
	return &GORMSKURepository{db: db}
}

func (r *GORMSKURepository) Create(ctx context.Context, s *model.SKU) error {
	return r.db.WithContext(ctx).Create(s).Error
}

func (r *GORMSKURepository) Update(ctx context.Context, s *model.SKU) error {
	return r.db.WithContext(ctx).Model(s).Select("specs", "price", "stock").Updates(s).Error
}

func (r *GORMSKURepository) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.SKU{}, id).Error
}

func (r *GORMSKURepository) GetByID(ctx context.Context, id int64) (*model.SKU, error) {
	var s model.SKU
	if err := r.db.WithContext(ctx).First(&s, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *GORMSKURepository) ListByProduct(ctx context.Context, productID int64) ([]model.SKU, error) {
	var list []model.SKU
	if err := r.db.WithContext(ctx).Where("product_id = ?", productID).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

var _ CategoryRepository = (*GORMCategoryRepository)(nil)
var _ ProductRepository = (*GORMProductRepository)(nil)
var _ SKURepository = (*GORMSKURepository)(nil)
