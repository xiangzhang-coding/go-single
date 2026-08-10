package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

// GetByIDForUpdate 在订单事务内读取并锁定商品状态。
func (r *GORMProductRepository) GetByIDForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*model.Product, error) {
	var p model.Product
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, id).Error; err != nil {
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

// GetByIDForUpdate 在订单事务内读取并锁定 SKU 行。
func (r *GORMSKURepository) GetByIDForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*model.SKU, error) {
	var s model.SKU
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&s, id).Error; err != nil {
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

// DeductStock 条件更新：stock=stock-N WHERE stock>=N 且商品仍上架；
// RowsAffected=0 表示库存不足、SKU 不存在或商品已下架。tx 由调用方
// （order 下单事务）提供，与订单创建同事务原子提交。
func (r *GORMSKURepository) DeductStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) (bool, error) {
	res := tx.WithContext(ctx).Exec(`
		UPDATE skus s
		JOIN products p ON p.id = s.product_id
		SET s.stock = s.stock - ?
		WHERE s.id = ? AND s.stock >= ? AND p.status = ?`,
		quantity, skuID, quantity, model.ProductStatusOnSale)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RestoreStock 回补库存（取消订单）；SKU 已删除时影响 0 行，视为空操作。
func (r *GORMSKURepository) RestoreStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) error {
	return tx.WithContext(ctx).Model(&model.SKU{}).
		Where("id = ?", skuID).
		Update("stock", gorm.Expr("stock + ?", quantity)).Error
}

var _ CategoryRepository = (*GORMCategoryRepository)(nil)
var _ ProductRepository = (*GORMProductRepository)(nil)
var _ SKURepository = (*GORMSKURepository)(nil)
