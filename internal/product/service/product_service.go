// Package service 承载 product 模块业务：admin 维护类目/商品/SKU，
// 游客按类目浏览与查看详情（详情走缓存，缺失降级直查 DB）。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
	"github.com/xiangzhang-coding/go-single/internal/product/model"
	"github.com/xiangzhang-coding/go-single/internal/product/repository"
	"go.uber.org/zap"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrCategoryNotFound = errors.New("category not found")
	ErrCategoryInUse    = errors.New("category has products")
	ErrProductNotFound  = errors.New("product not found")
	ErrSKUNotFound      = errors.New("sku not found")
	ErrSKUInUse         = errors.New("sku in use")
	ErrSKUStockChanged  = errors.New("sku stock changed")
	ErrInvalidInput     = errors.New("invalid input")
)

// 商品详情缓存约定：key product:detail:{id}，TTL 5min（见规格容错节）。
const (
	detailCacheTTL       = 5 * time.Minute
	detailMutationTTL    = 30 * time.Minute
	detailAOFTimeout     = 2 * time.Second
	detailCleanupTimeout = detailAOFTimeout + time.Second
	// 分页上限与默认页大小。
	defaultPageSize = 20
	maxPageSize     = 50
)

func detailCacheKeys(id int64) cache.ProductDetailKeys {
	return cache.ProductDetailKeys{
		Detail:   fmt.Sprintf("product:detail:%d", id),
		Version:  fmt.Sprintf("product:detail-version:%d", id),
		Mutation: fmt.Sprintf("product:detail-mutation:%d", id),
	}
}

type detailCache interface {
	Get(ctx context.Context, key string) (string, error)
	ProductDetailVersion(ctx context.Context, keys cache.ProductDetailKeys) (int64, error)
	SetProductDetailIfVersion(ctx context.Context, keys cache.ProductDetailKeys, version int64, value string, ttl time.Duration) (bool, error)
	BeginProductDetailMutation(ctx context.Context, keys cache.ProductDetailKeys, token string, ttl, aofTimeout time.Duration) error
	FinishProductDetailMutation(ctx context.Context, keys cache.ProductDetailKeys, token string, aofTimeout time.Duration) error
}

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
	UpdateSKU(ctx context.Context, id int64, specs json.RawMessage, price int64, stock, expectedStock int) error
	DeleteSKU(ctx context.Context, id int64) error
	// ListAllProducts 后台商品列表（T25）：可选类目/状态筛选 + 分页，
	// 与 ListProducts 的区别是不过滤上架状态（admin 需管理草稿/下架商品）。
	ListAllProducts(ctx context.Context, categoryID *int64, status string, page, pageSize int) ([]model.Product, int64, error)
	// GetAdminDetail 后台商品详情，包含上架及下架商品的全部 SKU，不经过公开详情缓存。
	GetAdminDetail(ctx context.Context, id int64) (*model.ProductDetail, error)

	// ---- 游客浏览 ----
	ListCategories(ctx context.Context) ([]model.Category, error)
	ListProducts(ctx context.Context, categoryID *int64, page, pageSize int) ([]model.Product, int64, error)
	// GetDetail 详情（仅上架商品），优先缓存、缺失回填。
	GetDetail(ctx context.Context, id int64) (*model.ProductDetail, error)
	// GetSKU 供后续模块校验 SKU 存在。
	GetSKU(ctx context.Context, id int64) (*model.SKU, error)
	// GetProduct 供购物车直读状态、秒杀页读取 SPU 标题等摘要信息。
	GetProduct(ctx context.Context, id int64) (*model.Product, error)
	// GetSKUSummaries 批量返回 SKU 与所属商品标题，供其他模块构建读模型。
	GetSKUSummaries(ctx context.Context, ids []int64) (map[int64]model.SKUSummary, error)
	// GetSKUForUpdate 在订单事务内锁定 SKU 并校验商品仍上架，供订单固化成交价。
	GetSKUForUpdate(ctx context.Context, tx *transaction.Handle, id int64) (*model.SKU, error)
	// DeductStock 事务内条件扣减库存（stock>=N 防超卖），供 order 模块下单调用。
	// 返回是否扣减成功；调用方在事务前后维护详情写入围栏。
	DeductStock(ctx context.Context, tx *transaction.Handle, skuID int64, quantity int) (bool, error)
	// RestoreStock 事务内回补库存；调用方在事务前后维护详情写入围栏。
	RestoreStock(ctx context.Context, tx *transaction.Handle, skuID int64, quantity int) error
	// BeginDetailMutation 在调用方事务写入前建立缓存回填围栏。
	BeginDetailMutation(ctx context.Context, productID int64) (string, error)
	// FinishDetailMutation 在调用方事务结束后推进代次、删除详情并解除围栏。
	FinishDetailMutation(ctx context.Context, productID int64, token string)
}

type productService struct {
	store repository.Store
	cache detailCache
	log   *zap.Logger
}

// New 构造商品服务。
func New(store repository.Store, c detailCache, log *zap.Logger) Service {
	return &productService{store: store, cache: c, log: log}
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
	return s.withDetailMutation(ctx, id, func() error {
		return s.store.Product.Update(ctx, &model.Product{ID: id, CategoryID: categoryID, Title: title, Description: description})
	})
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
	return s.withDetailMutation(ctx, id, func() error {
		return s.store.Product.SetStatus(ctx, id, status)
	})
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
	if err := s.withDetailMutation(ctx, productID, func() error {
		return s.store.SKU.Create(ctx, sku)
	}); err != nil {
		return nil, err
	}
	return sku, nil
}

func (s *productService) UpdateSKU(ctx context.Context, id int64, specs json.RawMessage, price int64, stock, expectedStock int) error {
	if err := validateSKU(specs, price, stock); err != nil {
		return err
	}
	if expectedStock < 0 {
		return fmt.Errorf("%w: invalid expected_stock", ErrInvalidInput)
	}
	sku, err := s.store.SKU.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if sku == nil {
		return ErrSKUNotFound
	}
	return s.withDetailMutation(ctx, sku.ProductID, func() error {
		updated, err := s.store.SKU.Update(ctx, &model.SKU{ID: id, Specs: specs, Price: price, Stock: stock}, expectedStock)
		if err != nil {
			return err
		}
		if !updated {
			return ErrSKUStockChanged
		}
		return nil
	})
}

func (s *productService) DeleteSKU(ctx context.Context, id int64) error {
	sku, err := s.store.SKU.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if sku == nil {
		return ErrSKUNotFound
	}
	return s.withDetailMutation(ctx, sku.ProductID, func() error {
		if err := s.store.SKU.Delete(ctx, id); err != nil {
			if errors.Is(err, repository.ErrSKUInUse) {
				return ErrSKUInUse
			}
			return err
		}
		return nil
	})
}

func (s *productService) ListCategories(ctx context.Context) ([]model.Category, error) {
	return s.store.Category.List(ctx)
}

func (s *productService) GetSKUSummaries(ctx context.Context, ids []int64) (map[int64]model.SKUSummary, error) {
	rows, err := s.store.SKU.ListSummariesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]model.SKUSummary, len(rows))
	for i := range rows {
		byID[rows[i].ID] = rows[i]
	}
	return byID, nil
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

// ListAllProducts 后台商品列表：status 空 = 全部状态（含草稿/下架）。
func (s *productService) ListAllProducts(ctx context.Context, categoryID *int64, status string, page, pageSize int) ([]model.Product, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if status != "" && status != model.ProductStatusOnSale && status != model.ProductStatusOffSale {
		return nil, 0, fmt.Errorf("%w: invalid status", ErrInvalidInput)
	}
	return s.store.Product.List(ctx, categoryID, status, (page-1)*pageSize, pageSize)
}

func (s *productService) GetAdminDetail(ctx context.Context, id int64) (*model.ProductDetail, error) {
	p, err := s.store.Product.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProductNotFound
	}
	skus, err := s.store.SKU.ListByProduct(ctx, id)
	if err != nil {
		return nil, err
	}
	return &model.ProductDetail{Product: *p, Skus: skus}, nil
}

// GetDetail 缓存优先（product:detail:{id}，TTL 5min）；未命中读取缓存代次后
// 直查 DB，仅在代次未变化时回填；缓存故障降级直查且跳过回填。
func (s *productService) GetDetail(ctx context.Context, id int64) (*model.ProductDetail, error) {
	keys := detailCacheKeys(id)

	raw, err := s.cache.Get(ctx, keys.Detail)
	if err == nil {
		var d model.ProductDetail
		jsonErr := json.Unmarshal([]byte(raw), &d)
		if jsonErr == nil {
			return &d, nil
		}
		s.log.Warn("商品详情缓存反序列化失败，降级直查", zap.String("key", keys.Detail), zap.Error(jsonErr))
	} else if !errors.Is(err, cache.ErrMiss) {
		s.log.Warn("商品详情缓存读取失败，降级直查", zap.String("key", keys.Detail), zap.Error(err))
	}
	version, versionErr := s.cache.ProductDetailVersion(ctx, keys)

	d, err := s.loadDetail(ctx, id)
	if err != nil {
		return nil, err
	}
	if data, jsonErr := json.Marshal(d); jsonErr == nil && versionErr == nil {
		if _, setErr := s.cache.SetProductDetailIfVersion(ctx, keys, version, string(data), detailCacheTTL); setErr != nil {
			s.log.Warn("商品详情回填缓存失败", zap.String("key", keys.Detail), zap.Error(setErr))
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

// GetProduct 读取 SPU；不存在返回 ErrProductNotFound。
func (s *productService) GetProduct(ctx context.Context, id int64) (*model.Product, error) {
	p, err := s.store.Product.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProductNotFound
	}
	return p, nil
}

// GetSKUForUpdate 锁定 SKU 行，并读取商品状态；库存扣减随后仍会在 SQL
// 条件中再次校验 on_sale，避免下架与下单并发时售出已下架商品。
func (s *productService) GetSKUForUpdate(ctx context.Context, tx *transaction.Handle, id int64) (*model.SKU, error) {
	// 先用非锁定读取得 product_id，再按 product → SKU 的固定顺序加锁；
	// 所有订单都遵守同一顺序，避免多 SKU/多商品订单形成锁环。
	sku, err := s.store.SKU.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if sku == nil {
		return nil, ErrSKUNotFound
	}
	p, err := s.store.Product.GetByIDForUpdate(ctx, tx, sku.ProductID)
	if err != nil {
		return nil, err
	}
	if p == nil || !p.IsOnSale() {
		return nil, ErrProductNotFound
	}
	lockedSKU, err := s.store.SKU.GetByIDForUpdate(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if lockedSKU == nil {
		return nil, ErrSKUNotFound
	}
	return lockedSKU, nil
}

// DeductStock 在调用方事务内条件扣减；事务由 order 模块开启并提交，
// 调用方必须在事务前后调用 BeginDetailMutation / FinishDetailMutation。
func (s *productService) DeductStock(ctx context.Context, tx *transaction.Handle, skuID int64, quantity int) (bool, error) {
	if quantity < 1 {
		return false, fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
	}
	ok, err := s.store.SKU.DeductStock(ctx, tx, skuID, quantity)
	if err != nil {
		return false, err
	}
	if !ok {
		// 条件更新失败不一定是库存不足：SKU 可能被删除，或商品在并发期间下架。
		latest, getErr := s.store.SKU.GetByID(ctx, skuID)
		if getErr != nil {
			return false, getErr
		}
		if latest == nil {
			return false, ErrSKUNotFound
		}
		product, getErr := s.store.Product.GetByID(ctx, latest.ProductID)
		if getErr != nil {
			return false, getErr
		}
		if product == nil || !product.IsOnSale() {
			return false, ErrProductNotFound
		}
	}
	return ok, nil
}

// RestoreStock 在调用方事务内回补库存；调用方必须在事务前后维护详情围栏。
func (s *productService) RestoreStock(ctx context.Context, tx *transaction.Handle, skuID int64, quantity int) error {
	if quantity < 1 {
		return fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
	}
	if _, err := s.GetSKU(ctx, skuID); err != nil {
		return err
	}
	if err := s.store.SKU.RestoreStock(ctx, tx, skuID, quantity); err != nil {
		return err
	}
	return nil
}

func (s *productService) BeginDetailMutation(ctx context.Context, productID int64) (string, error) {
	token, err := newDetailMutationToken()
	if err != nil {
		return "", err
	}
	if err := s.cache.BeginProductDetailMutation(ctx, detailCacheKeys(productID), token, detailMutationTTL, detailAOFTimeout); err != nil {
		return "", err
	}
	return token, nil
}

func (s *productService) FinishDetailMutation(ctx context.Context, productID int64, token string) {
	if err := s.cache.FinishProductDetailMutation(ctx, detailCacheKeys(productID), token, detailAOFTimeout); err != nil {
		s.log.Warn("商品详情缓存写入围栏解除失败，保持降级直查", zap.Int64("product_id", productID), zap.Error(err))
	}
}

func (s *productService) withDetailMutation(ctx context.Context, productID int64, mutate func() error) error {
	token, err := s.BeginDetailMutation(ctx, productID)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detailCleanupTimeout)
		defer cancel()
		s.FinishDetailMutation(cleanupCtx, productID, token)
	}()
	return mutate()
}

func newDetailMutationToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate product detail mutation token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
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
	if price < 0 || price > model.MaxPriceCents {
		return fmt.Errorf("%w: invalid price", ErrInvalidInput)
	}
	if stock < 0 {
		return fmt.Errorf("%w: invalid stock", ErrInvalidInput)
	}
	return nil
}
