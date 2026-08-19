// service 层单元测试（中间 seam）：fake 仓储 + fake 缓存，
// 覆盖 CRUD 校验、状态流转、详情缓存命中/回填/降级。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/product/model"
	"github.com/xiangzhang-coding/go-single/internal/product/repository"
)

// ---- fake 仓储 ----

type fakeCategories struct {
	byID  map[int64]*model.Category
	order int64
}

func newFakeCategories() *fakeCategories { return &fakeCategories{byID: map[int64]*model.Category{}} }

func (f *fakeCategories) Create(_ context.Context, c *model.Category) error {
	for _, v := range f.byID {
		if v.Name == c.Name {
			return repository.ErrCategoryNameExists
		}
	}
	f.order++
	c.ID = f.order
	f.byID[c.ID] = c
	return nil
}

func (f *fakeCategories) Update(_ context.Context, c *model.Category) error {
	if _, ok := f.byID[c.ID]; !ok {
		return nil
	}
	for _, v := range f.byID {
		if v.Name == c.Name && v.ID != c.ID {
			return repository.ErrCategoryNameExists
		}
	}
	f.byID[c.ID].Name = c.Name
	return nil
}

func (f *fakeCategories) Delete(_ context.Context, id int64) error {
	delete(f.byID, id)
	return nil
}

func (f *fakeCategories) GetByID(_ context.Context, id int64) (*model.Category, error) {
	return f.byID[id], nil
}

func (f *fakeCategories) List(context.Context) ([]model.Category, error) {
	out := make([]model.Category, 0, len(f.byID))
	for _, v := range f.byID {
		out = append(out, *v)
	}
	return out, nil
}

type fakeProducts struct {
	byID  map[int64]*model.Product
	order int64
}

func newFakeProducts() *fakeProducts { return &fakeProducts{byID: map[int64]*model.Product{}} }

func (f *fakeProducts) Create(_ context.Context, p *model.Product) error {
	f.order++
	p.ID = f.order
	f.byID[p.ID] = p
	return nil
}

func (f *fakeProducts) Update(_ context.Context, p *model.Product) error {
	if v, ok := f.byID[p.ID]; ok {
		v.CategoryID, v.Title, v.Description = p.CategoryID, p.Title, p.Description
	}
	return nil
}

func (f *fakeProducts) SetStatus(_ context.Context, id int64, status string) error {
	if v, ok := f.byID[id]; ok {
		v.Status = status
	}
	return nil
}

func (f *fakeProducts) GetByID(_ context.Context, id int64) (*model.Product, error) {
	return f.byID[id], nil
}

func (f *fakeProducts) GetByIDForUpdate(ctx context.Context, _ *gorm.DB, id int64) (*model.Product, error) {
	return f.GetByID(ctx, id)
}

func (f *fakeProducts) List(_ context.Context, categoryID *int64, status string, offset, limit int) ([]model.Product, int64, error) {
	var total int64
	var matched []model.Product
	for _, v := range f.byID {
		if status != "" && v.Status != status {
			continue
		}
		if categoryID != nil && v.CategoryID != *categoryID {
			continue
		}
		total++
		matched = append(matched, *v)
	}
	// 与 GORM 实现一致：id 倒序。
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })

	end := offset + limit
	if offset > len(matched) {
		return nil, total, nil
	}
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], total, nil
}

func (f *fakeProducts) CountByCategory(_ context.Context, categoryID int64) (int64, error) {
	var n int64
	for _, v := range f.byID {
		if v.CategoryID == categoryID {
			n++
		}
	}
	return n, nil
}

type fakeSKUs struct {
	byID  map[int64]*model.SKU
	order int64
}

func newFakeSKUs() *fakeSKUs { return &fakeSKUs{byID: map[int64]*model.SKU{}} }

func (f *fakeSKUs) Create(_ context.Context, s *model.SKU) error {
	f.order++
	s.ID = f.order
	f.byID[s.ID] = s
	return nil
}

func (f *fakeSKUs) Update(_ context.Context, s *model.SKU) error {
	if v, ok := f.byID[s.ID]; ok {
		v.Specs, v.Price, v.Stock = s.Specs, s.Price, s.Stock
	}
	return nil
}

func (f *fakeSKUs) Delete(_ context.Context, id int64) error {
	delete(f.byID, id)
	return nil
}

func (f *fakeSKUs) GetByID(_ context.Context, id int64) (*model.SKU, error) {
	return f.byID[id], nil
}

func (f *fakeSKUs) GetByIDForUpdate(ctx context.Context, _ *gorm.DB, id int64) (*model.SKU, error) {
	return f.GetByID(ctx, id)
}

func (f *fakeSKUs) ListByProduct(_ context.Context, productID int64) ([]model.SKU, error) {
	var out []model.SKU
	for _, v := range f.byID {
		if v.ProductID == productID {
			out = append(out, *v)
		}
	}
	return out, nil
}

// DeductStock 条件扣减：库存不足返回 (false, nil)；tx 参数忽略（单测无真实事务）。
func (f *fakeSKUs) DeductStock(_ context.Context, _ *gorm.DB, skuID int64, quantity int) (bool, error) {
	v, ok := f.byID[skuID]
	if !ok || v.Stock < quantity {
		return false, nil
	}
	v.Stock -= quantity
	return true, nil
}

func (f *fakeSKUs) RestoreStock(_ context.Context, _ *gorm.DB, skuID int64, quantity int) error {
	if v, ok := f.byID[skuID]; ok {
		v.Stock += quantity
	}
	return nil
}

// ---- fake 缓存 ----

type fakeCache struct {
	data      map[string]string
	ttl       map[string]time.Duration
	versions  map[string]int64
	mutations map[string]map[string]struct{}
	err       error
	beginErr  error
	finishErr error
	getCalls  int
	setCalls  int
	delCalls  int
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: map[string]string{}, ttl: map[string]time.Duration{}, versions: map[string]int64{}, mutations: map[string]map[string]struct{}{}}
}

func (f *fakeCache) Ping(context.Context) error { return nil }
func (f *fakeCache) Close() error               { return nil }

func (f *fakeCache) Get(_ context.Context, key string) (string, error) {
	f.getCalls++
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.data[key]
	if !ok {
		return "", cache.ErrMiss
	}
	return v, nil
}

func (f *fakeCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.data[key] = value
	f.ttl[key] = ttl
	f.setCalls++
	return nil
}

func (f *fakeCache) Del(_ context.Context, key string) error {
	f.delCalls++
	if f.err != nil {
		return f.err
	}
	delete(f.data, key)
	return nil
}

func (f *fakeCache) ProductDetailVersion(_ context.Context, keys cache.ProductDetailKeys) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.versions[keys.Version], nil
}

func (f *fakeCache) SetProductDetailIfVersion(_ context.Context, keys cache.ProductDetailKeys, version int64, value string, ttl time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.versions[keys.Version] != version {
		return false, nil
	}
	if len(f.mutations[keys.Mutation]) > 0 {
		return false, nil
	}
	f.data[keys.Detail] = value
	f.ttl[keys.Detail] = ttl
	f.setCalls++
	return true, nil
}

func (f *fakeCache) BeginProductDetailMutation(_ context.Context, keys cache.ProductDetailKeys, token string, _, _ time.Duration) error {
	if f.beginErr != nil {
		return f.beginErr
	}
	if f.err != nil {
		return f.err
	}
	f.versions[keys.Version]++
	if f.mutations[keys.Mutation] == nil {
		f.mutations[keys.Mutation] = map[string]struct{}{}
	}
	f.mutations[keys.Mutation][token] = struct{}{}
	delete(f.data, keys.Detail)
	f.delCalls++
	return nil
}

func (f *fakeCache) FinishProductDetailMutation(_ context.Context, keys cache.ProductDetailKeys, token string, _ time.Duration) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	if f.err != nil {
		return f.err
	}
	f.versions[keys.Version]++
	delete(f.mutations[keys.Mutation], token)
	if len(f.mutations[keys.Mutation]) == 0 {
		delete(f.mutations, keys.Mutation)
	}
	delete(f.data, keys.Detail)
	f.delCalls++
	return nil
}

// ---- 测试夹具 ----

type fixture struct {
	svc   Service
	cats  *fakeCategories
	prods *fakeProducts
	skus  *fakeSKUs
	cache *fakeCache
}

func newFixture() *fixture {
	cats, prods, skus := newFakeCategories(), newFakeProducts(), newFakeSKUs()
	fc := newFakeCache()
	svc := New(repository.Store{Category: cats, Product: prods, SKU: skus}, fc, zap.NewNop())
	return &fixture{svc: svc, cats: cats, prods: prods, skus: skus, cache: fc}
}

func (fx *fixture) category(t *testing.T, name string) *model.Category {
	t.Helper()
	c, err := fx.svc.CreateCategory(context.Background(), name)
	require.NoError(t, err)
	return c
}

func (fx *fixture) publishedProduct(t *testing.T, categoryID int64, title string) *model.Product {
	t.Helper()
	p, err := fx.svc.CreateProduct(context.Background(), categoryID, title, "desc")
	require.NoError(t, err)
	require.NoError(t, fx.svc.PublishProduct(context.Background(), p.ID))
	return p
}

// ---- 类目 ----

func TestCreateCategory(t *testing.T) {
	fx := newFixture()
	c, err := fx.svc.CreateCategory(context.Background(), "数码")
	require.NoError(t, err)
	assert.Equal(t, "数码", c.Name)
	assert.Equal(t, int64(1), c.ID)

	// 重复名 → 400 语义错误。
	_, err = fx.svc.CreateCategory(context.Background(), "数码")
	require.ErrorIs(t, err, ErrInvalidInput)

	// 空白名 → 400。
	_, err = fx.svc.CreateCategory(context.Background(), "   ")
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestUpdateCategoryTrimsName(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")

	require.NoError(t, fx.svc.UpdateCategory(context.Background(), c.ID, "  家电  "))
	got, err := fx.cats.GetByID(context.Background(), c.ID)
	require.NoError(t, err)
	assert.Equal(t, "家电", got.Name)
}

func TestDeleteCategoryInUse(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	fx.publishedProduct(t, c.ID, "手机")

	require.ErrorIs(t, fx.svc.DeleteCategory(context.Background(), c.ID), ErrCategoryInUse)

	// 无商品后可删除。
	empty := fx.category(t, "空类目")
	require.NoError(t, fx.svc.DeleteCategory(context.Background(), empty.ID))
}

// ---- 商品 ----

func TestCreateProductValidation(t *testing.T) {
	fx := newFixture()

	_, err := fx.svc.CreateProduct(context.Background(), 99, "手机", "")
	require.ErrorIs(t, err, ErrCategoryNotFound)

	c := fx.category(t, "数码")
	_, err = fx.svc.CreateProduct(context.Background(), c.ID, "  ", "")
	require.ErrorIs(t, err, ErrInvalidInput)

	p, err := fx.svc.CreateProduct(context.Background(), c.ID, "旗舰手机", "详细描述")
	require.NoError(t, err)
	assert.Equal(t, model.ProductStatusOffSale, p.Status, "新建商品默认下架")
}

func TestPublishUnpublish(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p, err := fx.svc.CreateProduct(context.Background(), c.ID, "手机", "")
	require.NoError(t, err)

	require.NoError(t, fx.svc.PublishProduct(context.Background(), p.ID))
	got, err := fx.prods.GetByID(context.Background(), p.ID)
	require.NoError(t, err)
	assert.True(t, got.IsOnSale())

	require.NoError(t, fx.svc.UnpublishProduct(context.Background(), p.ID))
	got, err = fx.prods.GetByID(context.Background(), p.ID)
	require.NoError(t, err)
	assert.False(t, got.IsOnSale())

	// 不存在的商品 → 404。
	require.ErrorIs(t, fx.svc.PublishProduct(context.Background(), 999), ErrProductNotFound)
}

func TestUpdateProduct(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p, err := fx.svc.CreateProduct(context.Background(), c.ID, "手机", "")
	require.NoError(t, err)
	fx.svc.PublishProduct(context.Background(), p.ID)

	// 预热缓存。
	_, err = fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Greater(t, fx.cache.setCalls, 0)

	require.NoError(t, fx.svc.UpdateProduct(context.Background(), p.ID, c.ID, "手机Pro", "新描述"))
	got, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, "手机Pro", got.Title, "编辑商品后缓存被清除，详情应读到新值")

	// 不存在的类目 → 404。
	require.ErrorIs(t, fx.svc.UpdateProduct(context.Background(), p.ID, 999, "x", ""), ErrCategoryNotFound)
}

// ---- SKU ----

func TestCreateSKUValidation(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p, err := fx.svc.CreateProduct(context.Background(), c.ID, "手机", "")
	require.NoError(t, err)

	// 不存在商品 → 404。
	_, err = fx.svc.CreateSKU(context.Background(), 999, json.RawMessage(`{}`), 100, 1)
	require.ErrorIs(t, err, ErrProductNotFound)

	// 非法 specs → 400。
	_, err = fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{bad`), 100, 1)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 负价格/负库存 → 400。
	_, err = fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{}`), -1, 1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{}`), 100, -1)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 商品价格上限为 100 万元（金额单位为分）。
	_, err = fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{"tier":"max"}`), 100_000_000, 1)
	require.NoError(t, err)
	_, err = fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{"tier":"over"}`), 100_000_001, 1)
	require.ErrorIs(t, err, ErrInvalidInput)

	sku, err := fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{"color":"红"}`), 9900, 10)
	require.NoError(t, err)
	assert.Equal(t, p.ID, sku.ProductID)
	assert.Equal(t, int64(9900), sku.Price)
	assert.Equal(t, 10, sku.Stock)
}

func TestUpdateSKUInvalidatesCache(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p := fx.publishedProduct(t, c.ID, "手机")
	sku, err := fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{"color":"红"}`), 100, 5)
	require.NoError(t, err)

	_, err = fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)

	require.NoError(t, fx.svc.UpdateSKU(context.Background(), sku.ID, json.RawMessage(`{"color":"蓝"}`), 200, 3))
	d, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	require.Len(t, d.Skus, 1)
	assert.Equal(t, int64(200), d.Skus[0].Price)
	assert.Equal(t, int64(3), int64(d.Skus[0].Stock))

	// 更新不存在的 SKU → 404。
	require.ErrorIs(t, fx.svc.UpdateSKU(context.Background(), 999, json.RawMessage(`{}`), 1, 1), ErrSKUNotFound)

	// 删除后详情不再含该 SKU。
	require.NoError(t, fx.svc.DeleteSKU(context.Background(), sku.ID))
	d, err = fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Empty(t, d.Skus)
	require.ErrorIs(t, fx.svc.DeleteSKU(context.Background(), sku.ID), ErrSKUNotFound)
}

func TestUpdateSKUKeepsCacheFencedWhenFinishFails(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p := fx.publishedProduct(t, c.ID, "手机")
	sku, err := fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{"color":"红"}`), 100, 5)
	require.NoError(t, err)
	keys := detailCacheKeys(p.ID)
	staleVersion := fx.cache.versions[keys.Version]

	fx.cache.finishErr = errors.New("redis unavailable")
	require.NoError(t, fx.svc.UpdateSKU(context.Background(), sku.ID, json.RawMessage(`{"color":"蓝"}`), 200, 3))
	fx.cache.finishErr = nil

	written, err := fx.cache.SetProductDetailIfVersion(context.Background(), keys, staleVersion, `{"old":true}`, detailCacheTTL)
	require.NoError(t, err)
	require.False(t, written, "结束步骤失败时 marker 应继续拒绝旧请求回填")
	detail, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	require.Equal(t, int64(200), detail.Skus[0].Price)
	require.NotContains(t, fx.cache.data, keys.Detail, "marker 存在时详情应降级直查且不回填")
}

func TestUpdateSKURequiresCacheMutationFence(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p := fx.publishedProduct(t, c.ID, "手机")
	sku, err := fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{}`), 100, 5)
	require.NoError(t, err)

	fx.cache.beginErr = errors.New("redis unavailable")
	err = fx.svc.UpdateSKU(context.Background(), sku.ID, json.RawMessage(`{}`), 200, 3)
	require.Error(t, err)
	require.Equal(t, int64(100), fx.skus.byID[sku.ID].Price, "围栏未建立时不得提交商品变更")
}

// ---- 游客浏览 ----

func TestListProductsOnlyOnSaleAndFiltered(t *testing.T) {
	fx := newFixture()
	digital := fx.category(t, "数码")
	home := fx.category(t, "家电")

	phone := fx.publishedProduct(t, digital.ID, "手机")
	fx.publishedProduct(t, digital.ID, "平板")
	offline, err := fx.svc.CreateProduct(context.Background(), digital.ID, "未上架", "")
	require.NoError(t, err)
	fx.publishedProduct(t, home.ID, "冰箱")

	all, total, err := fx.svc.ListProducts(context.Background(), nil, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, 3, len(all))

	// 按类目筛选：数码类 2 件（下架的不计）。
	digitalList, total, err := fx.svc.ListProducts(context.Background(), &digital.ID, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	ids := []int64{digitalList[0].ID, digitalList[1].ID}
	assert.Contains(t, ids, phone.ID)
	assert.NotContains(t, ids, offline.ID)

	// 分页：page=2, page_size=1 取第 2 条。
	page2, total, err := fx.svc.ListProducts(context.Background(), nil, 2, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, 1, len(page2))
}

// 后台列表：status 空 = 全部状态（含草稿/下架），可按状态/类目筛选、分页。
func TestListAllProductsIncludesDrafts(t *testing.T) {
	fx := newFixture()
	digital := fx.category(t, "数码")
	home := fx.category(t, "家电")

	fx.publishedProduct(t, digital.ID, "手机")
	offline, err := fx.svc.CreateProduct(context.Background(), digital.ID, "草稿手机", "")
	require.NoError(t, err)
	fx.publishedProduct(t, home.ID, "冰箱")

	// 全部状态：3 件都可见（含草稿）。
	all, total, err := fx.svc.ListAllProducts(context.Background(), nil, "", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, 3, len(all))

	// 按状态筛选：仅下架 1 件。
	offlineList, total, err := fx.svc.ListAllProducts(context.Background(), nil, model.ProductStatusOffSale, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, offline.ID, offlineList[0].ID)

	// 按类目筛选：数码 2 件（含草稿）。
	_, total, err = fx.svc.ListAllProducts(context.Background(), &digital.ID, "", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)

	// 非法状态 → 400。
	_, _, err = fx.svc.ListAllProducts(context.Background(), nil, "bogus", 1, 10)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 分页：page=2, page_size=2 取第 2 条。
	page2, total, err := fx.svc.ListAllProducts(context.Background(), nil, "", 2, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, 1, len(page2))
}

func TestGetDetailOffSaleInvisible(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	offline, err := fx.svc.CreateProduct(context.Background(), c.ID, "草稿商品", "")
	require.NoError(t, err)

	_, err = fx.svc.GetDetail(context.Background(), offline.ID)
	require.ErrorIs(t, err, ErrProductNotFound)

	_, err = fx.svc.GetDetail(context.Background(), 999)
	require.ErrorIs(t, err, ErrProductNotFound)
}

func TestGetDetailCacheHitMissAndFill(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p := fx.publishedProduct(t, c.ID, "手机")
	_, err := fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{"color":"红"}`), 9900, 10)
	require.NoError(t, err)

	// 首次访问：miss → 直查 DB → 回填（TTL 5min）。
	before := fx.cache.getCalls
	d, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, "手机", d.Title)
	require.Len(t, d.Skus, 1)
	assert.Equal(t, int64(9900), d.Skus[0].Price)
	assert.Equal(t, detailCacheTTL, fx.cache.ttl[detailCacheKeys(p.ID).Detail])
	require.Equal(t, before+1, fx.cache.getCalls)

	// 绕过缓存改 DB：第二次访问仍返回旧值（命中缓存）。
	require.NoError(t, fx.skus.Update(context.Background(), &model.SKU{ID: d.Skus[0].ID, Specs: d.Skus[0].Specs, Price: 1, Stock: 1}))
	cached, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(9900), cached.Skus[0].Price, "命中缓存应返回旧价格")
	require.Equal(t, before+2, fx.cache.getCalls)

	// 清空缓存（模拟缓存被清除）：降级直查 DB，读到新值并重新回填。
	fx.cache.data = map[string]string{}
	d2, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), d2.Skus[0].Price, "缓存清空后直查 DB 应读到新价格")
	assert.Equal(t, before+3, fx.cache.getCalls)
}

func TestGetDetailCacheDownDegrades(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p := fx.publishedProduct(t, c.ID, "手机")

	// 缓存故障：Get/Set 均报错，仍应直查 DB 正常返回。
	fx.cache.err = errors.New("connection refused")
	d, err := fx.svc.GetDetail(context.Background(), p.ID)
	require.NoError(t, err)
	assert.Equal(t, "手机", d.Title)
}

func TestGetSKU(t *testing.T) {
	fx := newFixture()
	c := fx.category(t, "数码")
	p := fx.publishedProduct(t, c.ID, "手机")
	sku, err := fx.svc.CreateSKU(context.Background(), p.ID, json.RawMessage(`{}`), 100, 1)
	require.NoError(t, err)

	got, err := fx.svc.GetSKU(context.Background(), sku.ID)
	require.NoError(t, err)
	assert.Equal(t, sku.ID, got.ID)

	_, err = fx.svc.GetSKU(context.Background(), 999)
	require.ErrorIs(t, err, ErrSKUNotFound)
}
