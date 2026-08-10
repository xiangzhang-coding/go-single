// service 层单元测试（中间 seam）：fake 购物车仓储 + fake product 服务，
// 覆盖加购校验（数量/SKU 存在/上架）、重复加购合并与上限、改量/删除 owner 校验。
package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/cart/model"
	"github.com/xiangzhang-coding/go-single/internal/cart/repository"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
)

// ---- fake 购物车仓储 ----

type fakeItems struct {
	byID      map[int64]*model.CartItem
	order     int64
	createErr error
}

func newFakeItems() *fakeItems { return &fakeItems{byID: map[int64]*model.CartItem{}} }

func (f *fakeItems) Create(_ context.Context, item *model.CartItem) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.order++
	item.ID = f.order
	f.byID[item.ID] = item
	return nil
}

func (f *fakeItems) GetByID(_ context.Context, id int64) (*model.CartItem, error) {
	return f.byID[id], nil
}

func (f *fakeItems) GetByUserAndSKU(_ context.Context, userID, skuID int64) (*model.CartItem, error) {
	for _, v := range f.byID {
		if v.UserID == userID && v.SKUID == skuID {
			return v, nil
		}
	}
	return nil, nil
}

func (f *fakeItems) UpdateQuantity(_ context.Context, id int64, quantity int) error {
	if v, ok := f.byID[id]; ok {
		v.Quantity = quantity
	}
	return nil
}

func (f *fakeItems) Delete(_ context.Context, id int64) error {
	delete(f.byID, id)
	return nil
}

func (f *fakeItems) ListByUser(_ context.Context, userID int64) ([]model.CartItemView, error) {
	var out []model.CartItemView
	for _, v := range f.byID {
		if v.UserID == userID {
			out = append(out, model.CartItemView{CartItem: *v})
		}
	}
	return out, nil
}

// ---- fake product 服务 ----

// fakeProducts 以 SKU 表模拟 product 模块：sku.OffSale 标记商品下架。
type fakeProducts struct {
	skus map[int64]*productmodel.SKU
	// offSale 商品集合（ProductID → 下架）。
	offSale map[int64]bool
}

func newFakeProducts() *fakeProducts {
	return &fakeProducts{skus: map[int64]*productmodel.SKU{}, offSale: map[int64]bool{}}
}

func (f *fakeProducts) seed(skuID, productID int64) {
	f.skus[skuID] = &productmodel.SKU{ID: skuID, ProductID: productID, Specs: json.RawMessage(`{}`), Price: 100, Stock: 5}
}

func (f *fakeProducts) GetSKU(_ context.Context, id int64) (*productmodel.SKU, error) {
	if s, ok := f.skus[id]; ok {
		return s, nil
	}
	return nil, productsvc.ErrSKUNotFound
}

func (f *fakeProducts) GetDetail(_ context.Context, productID int64) (*productmodel.ProductDetail, error) {
	if f.offSale[productID] {
		return nil, productsvc.ErrProductNotFound
	}
	return &productmodel.ProductDetail{Product: productmodel.Product{ID: productID}}, nil
}

// ---- 测试夹具 ----

type fixture struct {
	svc      Service
	items    *fakeItems
	products *fakeProducts
}

func newFixture() *fixture {
	items, products := newFakeItems(), newFakeProducts()
	svc := New(repository.Store{Items: items}, products)
	return &fixture{svc: svc, items: items, products: products}
}

// ---- 加购 ----

func TestAddItemValidation(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1, 10)

	// 数量越界 / 非法 id → 400。
	_, err := fx.svc.AddItem(context.Background(), 100, 1, 0)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.AddItem(context.Background(), 100, 1, -1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.AddItem(context.Background(), 100, 1, maxQuantity+1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.AddItem(context.Background(), 0, 1, 1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = fx.svc.AddItem(context.Background(), 100, 0, 1)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestAddItemSKUChecks(t *testing.T) {
	fx := newFixture()

	// 不存在的 SKU → 404。
	_, err := fx.svc.AddItem(context.Background(), 100, 999, 1)
	require.ErrorIs(t, err, ErrSKUNotFound)

	// 商品下架 → 409。
	fx.products.seed(1, 10)
	fx.products.offSale[10] = true
	_, err = fx.svc.AddItem(context.Background(), 100, 1, 1)
	require.ErrorIs(t, err, ErrSKUUnavailable)

	// 上架商品可加购。
	fx.products.offSale[10] = false
	item, err := fx.svc.AddItem(context.Background(), 100, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(100), item.UserID)
	assert.Equal(t, int64(1), item.SKUID)
	assert.Equal(t, 2, item.Quantity)
}

func TestAddItemMergesExisting(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1, 10)

	first, err := fx.svc.AddItem(context.Background(), 100, 1, 2)
	require.NoError(t, err)

	// 重复加购同一 SKU：数量合并。
	merged, err := fx.svc.AddItem(context.Background(), 100, 1, 3)
	require.NoError(t, err)
	assert.Equal(t, first.ID, merged.ID, "应复用原条目")
	assert.Equal(t, 5, merged.Quantity)

	// 合并超出上限：封顶 99。
	got, err := fx.svc.AddItem(context.Background(), 100, 1, maxQuantity)
	require.NoError(t, err)
	assert.Equal(t, maxQuantity, got.Quantity)
	assert.Equal(t, 1, len(fx.items.byID), "合并不应新增条目")
}

// 并发同对加购：Create 撞唯一键冲突，服务应重查后合并（与正常合并同一逻辑）。
func TestAddItemConcurrentDuplicateMerges(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1, 10)

	_, err := fx.svc.AddItem(context.Background(), 100, 1, 2)
	require.NoError(t, err)

	fx.items.createErr = repository.ErrCartItemExists
	merged, err := fx.svc.AddItem(context.Background(), 100, 1, 3)
	require.NoError(t, err)
	assert.Equal(t, 5, merged.Quantity)
	assert.Equal(t, 1, len(fx.items.byID), "冲突合并不应新增条目")
}

// ---- 改量 / 删除 ----

func TestUpdateQuantityOwnerCheck(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1, 10)
	item, err := fx.svc.AddItem(context.Background(), 100, 1, 1)
	require.NoError(t, err)

	// 不存在的条目 → 404。
	err = fx.svc.UpdateQuantity(context.Background(), 100, 999, 2)
	require.ErrorIs(t, err, ErrCartItemNotFound)

	// 归属他人 → 403。
	err = fx.svc.UpdateQuantity(context.Background(), 200, item.ID, 2)
	require.ErrorIs(t, err, ErrCartItemForbidden)

	// 数量越界 → 400。
	err = fx.svc.UpdateQuantity(context.Background(), 100, item.ID, 0)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 归属人改量生效。
	require.NoError(t, fx.svc.UpdateQuantity(context.Background(), 100, item.ID, 4))
	got, err := fx.items.GetByID(context.Background(), item.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, got.Quantity)
}

func TestDeleteItemOwnerCheck(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1, 10)
	item, err := fx.svc.AddItem(context.Background(), 100, 1, 1)
	require.NoError(t, err)

	err = fx.svc.DeleteItem(context.Background(), 200, item.ID)
	require.ErrorIs(t, err, ErrCartItemForbidden)

	err = fx.svc.DeleteItem(context.Background(), 100, 999)
	require.ErrorIs(t, err, ErrCartItemNotFound)

	require.NoError(t, fx.svc.DeleteItem(context.Background(), 100, item.ID))
	_, err = fx.items.GetByID(context.Background(), item.ID)
	assert.Nil(t, err, "条目已删除")
	assert.NotContains(t, fx.items.byID, item.ID)
}

// ---- 列表 ----

func TestListItems(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1, 10)

	_, err := fx.svc.AddItem(context.Background(), 100, 1, 2)
	require.NoError(t, err)
	// 只影响归属人自己的列表：另一用户的列表为空。
	list, err := fx.svc.ListItems(context.Background(), 200)
	require.NoError(t, err)
	assert.Empty(t, list)

	list, err = fx.svc.ListItems(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, int64(1), list[0].SKUID)
	assert.Equal(t, 2, list[0].Quantity)
}


