// Package service 承载 product 模块业务：admin 维护类目/商品/SKU，
// 游客按类目浏览与查看详情（详情走缓存，缺失降级直查 DB）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/product/model"
	"github.com/xiangzhang-coding/go-single/internal/product/repository"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrCategoryNotFound = errors.New("category not found")
	ErrCategoryInUse    = errors.New("category has products")
	ErrProductNotFound  = errors.New("product not found")
	ErrSKUNotFound      = errors.New("sku not found")
	ErrInvalidInput     = errors.New("invalid input")
)

// 商品详情缓存约定：key product:detail:{id}，TTL 5min（见规格容错节）。
const (
	detailCacheTTL = 5 * time.Minute
	// 分页上限与默认页大小。
	defaultPageSize = 20
	maxPageSize     = 50
)

func detailCacheKey(id int64) string { return fmt.Sprintf("product:detail:%d", id) }

// Service product 模块的业务接口。
type Service interface {
	// ---- admin 管理 ----
	CreateCategory(ctx context.Context, name string) (*model.Category, error)
	UpdateCategory(ctx context.Context, id int64, name string) error
	DeleteCategory(ctx context.Context, id int64) error

	CreateProduct(ctx context.Context, categoryID int64, title, description string) (*model.Product, error)
	UpdateProduct(ctx context.Context, id int64, categoryID int64, title, description string) error
	PublishProduct(ctx context.Context, id int64) error
	UnpublishProduct(ctx context.Context, id int64) error

	CreateSKU(ctx context.Context, productID int64, specs json.RawMessage, price int64, stock int) (*model.SKU, error)
	UpdateSKU(ctx context.Context, id int64, specs json.RawMessage, price int64, stock int) error
	DeleteSKU(ctx context.Context, id int64) error

	// ---- 游客浏览 ----
	ListCategories(ctx context.Context) ([]model.Category, error)
	ListProducts(ctx context.Context, categoryID *int64, page, pageSize int) ([]model.Product, int64, error)
	// GetDetail 详情（仅上架商品），优先缓存、缺失回填。
	GetDetail(ctx context.Context, id int64) (*model.ProductDetail, error)
	// GetSKU 供后续模块（购物车）校验 SKU 存在/上架。
	GetSKU(ctx context.Context, id int64) (*model.SKU, error)
	// DeductStock 事务内条件扣减库存（stock>=N 防超卖），供 order 模块下单调用；
	// 扣减成功后失效详情缓存。返回是否扣减成功。
	DeductStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) (bool, error)
	// RestoreStock 事务内回补库存，供 order 模块取消订单调用；随后失效详情缓存。
	RestoreStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) error
}

type productService struct {
	store repository.Store
	cache cache.Cache
}

// New 构造商品服务。
func New(store repository.Store, c cache.Cache) Service {
	return &productService{store: store, cache: c}
}

func (s *productService) CreateCategory(ctx context.Context, name string) (*model.Category, error) {
	name, err := validateName("category", name)
	if err != nil {
		return nil, err
	}
	c := &model.Category{Name: name}
	if err := s.store.Category.Create(ctx, c); err != nil {
		if errors.Is(err, repository.ErrCategoryNameExists) {
			return nil, ErrInvalidInput
		}
		return nil, err
	}
	return c, nil
}

func (s *productService) UpdateCategory(ctx context.Context, id int64, name string) error {
	name, err := validateName("category", name)
	if err != nil {
		return err
	}
	c := &model.Category{ID: id, Name: name}
	if err := s.store.Category.Update(ctx, c); err != nil {
		if errors.Is(err, repository.ErrCategoryNameExists) {
			return ErrInvalidInput
		}
		return err
	}
	return nil
}

func (s *productService) DeleteCategory(ctx context.Context, id int64) error {
	n, err := s.store.Product.CountByCategory(ctx, id)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrCategoryInUse
	}
	if err := s.store.Category.Delete(ctx, id); err != nil {
		if errors.Is(err, repository.ErrCategoryInUse) {
			return ErrCategoryInUse
		}
		return err
	}
	return nil
}

func (s *productService) CreateProduct(ctx context.Context, categoryID int64, title, description string) (*model.Product, error) {
	title, err := validateProduct(categoryID, title)
	if err != nil {
		return nil, err
	}
	c, err := s.store.Category.GetByID(ctx, categoryID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrCategoryNotFound
	}
	p := &model.Product{CategoryID: categoryID, Title: title, Description: description, Status: model.ProductStatusOffSale}
	if err := s.store.Product.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *productService) UpdateProduct(ctx context.Context, id int64, categoryID int64, title, description string) error {
	title, err := validateProduct(categoryID, title)
	if err != nil {
		return err
	}
	cat, err := s.store.Category.GetByID(ctx, categoryID)
	if err != nil {
		return err
	}
	if cat == nil {
		return ErrCategoryNotFound
	}
	p, err := s.store.Product.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrProductNotFound
	}
	if err := s.store.Product.Update(ctx, &model.Product{ID: id, CategoryID: categoryID, Title: title, Description: description}); err != nil {
		return err
	}
	s.invalidateDetail(ctx, id)
	return nil
}

func (s *productService) PublishProduct(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, model.ProductStatusOnSale)
}

func (s *productService) UnpublishProduct(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, model.ProductStatusOffSale)
}

func (s *productService) setStatus(ctx context.Context, id int64, status string) error {
	p, err := s.store.Product.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrProductNotFound
	}
	if p.Status == status {
		return nil
	}
	if err := s.store.Product.SetStatus(ctx, id, status); err != nil {
		return err
	}
	s.invalidateDetail(ctx, id)
	return nil
}

func (s *productService) CreateSKU(ctx context.Context, productID int64, specs json.RawMessage, price int64, stock int) (*model.SKU, error) {
	if err := validateSKU(specs, price, stock); err != nil {
		return nil, err
	}
	p, err := s.store.Product.GetByID(ctx, productID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProductNotFound
	}
	sku := &model.SKU{ProductID: productID, Specs: specs, Price: price, Stock: stock}
	if err := s.store.SKU.Create(ctx, sku); err != nil {
		return nil, err
	}
	s.invalidateDetail(ctx, productID)
	return sku, nil
}

func (s *productService) UpdateSKU(ctx context.Context, id int64, specs json.RawMessage, price int64, stock int) error {
	if err := validateSKU(specs, price, stock); err != nil {
		return err
	}
	sku, err := s.store.SKU.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if sku == nil {
		return ErrSKUNotFound
	}
	if err := s.store.SKU.Update(ctx, &model.SKU{ID: id, Specs: specs, Price: price, Stock: stock}); err != nil {
		return err
	}
	s.invalidateDetail(ctx, sku.ProductID)
	return nil
}

func (s *productService) DeleteSKU(ctx context.Context, id int64) error {
	sku, err := s.store.SKU.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if sku == nil {
		return ErrSKUNotFound
	}
	if err := s.store.SKU.Delete(ctx, id); err != nil {
		return err
	}
	s.invalidateDetail(ctx, sku.ProductID)
	return nil
}

func (s *productService) ListCategories(ctx context.Context) ([]model.Category, error) {
	return s.store.Category.List(ctx)
}

func (s *productService) ListProducts(ctx context.Context, categoryID *int64, page, pageSize int) ([]model.Product, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return s.store.Product.List(ctx, categoryID, model.ProductStatusOnSale, (page-1)*pageSize, pageSize)
}

// GetDetail 缓存优先（product:detail:{id}，TTL 5min）；未命中直查 DB 并回填；
// 缓存故障视为未命中（降级直查），不影响可用性。
func (s *productService) GetDetail(ctx context.Context, id int64) (*model.ProductDetail, error) {
	key := detailCacheKey(id)

	raw, err := s.cache.Get(ctx, key)
	if err == nil {
		var d model.ProductDetail
		jsonErr := json.Unmarshal([]byte(raw), &d)
		if jsonErr == nil {
			return &d, nil
		}
		slog.Warn("商品详情缓存反序列化失败，降级直查", "key", key, "error", jsonErr)
	} else if !errors.Is(err, cache.ErrMiss) {
		slog.Warn("商品详情缓存读取失败，降级直查", "key", key, "error", err)
	}

	d, err := s.loadDetail(ctx, id)
	if err != nil {
		return nil, err
	}
	if data, jsonErr := json.Marshal(d); jsonErr == nil {
		if setErr := s.cache.Set(ctx, key, string(data), detailCacheTTL); setErr != nil {
			slog.Warn("商品详情回填缓存失败", "key", key, "error", setErr)
		}
	}
	return d, nil
}

// loadDetail 直查 DB：仅上架商品对游客可见，下架/不存在一律视为未找到。
func (s *productService) loadDetail(ctx context.Context, id int64) (*model.ProductDetail, error) {
	p, err := s.store.Product.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil || !p.IsOnSale() {
		return nil, ErrProductNotFound
	}
	skus, err := s.store.SKU.ListByProduct(ctx, id)
	if err != nil {
		return nil, err
	}
	return &model.ProductDetail{Product: *p, Skus: skus}, nil
}

func (s *productService) GetSKU(ctx context.Context, id int64) (*model.SKU, error) {
	sku, err := s.store.SKU.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if sku == nil {
		return nil, ErrSKUNotFound
	}
	return sku, nil
}

// DeductStock 先确认 SKU 存在（同时取得 product_id 供缓存失效），
// 再在同一事务内条件扣减；事务由 order 模块开启并提交。
func (s *productService) DeductStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) (bool, error) {
	if quantity < 1 {
		return false, fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
	}
	sku, err := s.GetSKU(ctx, skuID)
	if err != nil {
		return false, err
	}
	ok, err := s.store.SKU.DeductStock(ctx, tx, skuID, quantity)
	if err != nil {
		return false, err
	}
	if ok {
		s.invalidateDetail(ctx, sku.ProductID)
	}
	return ok, nil
}

// RestoreStock 同事务回补库存（取消订单），随后失效详情缓存。
func (s *productService) RestoreStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) error {
	if quantity < 1 {
		return fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
	}
	sku, err := s.GetSKU(ctx, skuID)
	if err != nil {
		return err
	}
	if err := s.store.SKU.RestoreStock(ctx, tx, skuID, quantity); err != nil {
		return err
	}
	s.invalidateDetail(ctx, sku.ProductID)
	return nil
}

// invalidateDetail 商品或其 SKU 变更后清缓存（缓存故障不阻断写路径）。
func (s *productService) invalidateDetail(ctx context.Context, productID int64) {
	if err := s.cache.Del(ctx, detailCacheKey(productID)); err != nil {
		slog.Warn("商品详情缓存失效失败", "product_id", productID, "error", err)
	}
}

func validateName(what, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", fmt.Errorf("%w: invalid %s name", ErrInvalidInput, what)
	}
	return name, nil
}

func validateProduct(categoryID int64, title string) (string, error) {
	if categoryID <= 0 {
		return "", fmt.Errorf("%w: invalid category id", ErrInvalidInput)
	}
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 128 {
		return "", fmt.Errorf("%w: invalid title", ErrInvalidInput)
	}
	return title, nil
}

func validateSKU(specs json.RawMessage, price int64, stock int) error {
	if len(specs) == 0 || !json.Valid(specs) || len(specs) > 255 {
		return fmt.Errorf("%w: invalid specs", ErrInvalidInput)
	}
	if price < 0 {
		return fmt.Errorf("%w: invalid price", ErrInvalidInput)
	}
	if stock < 0 {
		return fmt.Errorf("%w: invalid stock", ErrInvalidInput)
	}
	return nil
}
