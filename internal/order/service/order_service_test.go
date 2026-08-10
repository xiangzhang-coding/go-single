// service 层单元测试（中间 seam）：fake 仓储/缓存/跨模块服务，
// 覆盖下单（直购/购物车结算）、幂等、金额与券门槛、库存不足、取消回补、
// 状态机非法跃迁拒绝与 owner 校验（防 IDOR）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	cartmodel "github.com/xiangzhang-coding/go-single/internal/cart/model"
	couponmodel "github.com/xiangzhang-coding/go-single/internal/coupon/model"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	"github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/order/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

// ---- fake 订单仓储 ----

type fakeOrders struct {
	byID   map[string]*model.Order
	txLog  []string
	getErr error
}

func newFakeOrders() *fakeOrders { return &fakeOrders{byID: map[string]*model.Order{}} }

func (f *fakeOrders) Create(_ context.Context, _ *gorm.DB, o *model.Order) error {
	f.byID[o.OrderNo] = o
	return nil
}

func (f *fakeOrders) GetByNo(_ context.Context, orderNo string) (*model.Order, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byID[orderNo], nil
}

func (f *fakeOrders) List(_ context.Context, userID int64, status string, offset, limit int) ([]model.Order, int64, error) {
	var out []model.Order
	for _, o := range f.byID {
		if o.UserID != userID || (status != "" && o.Status != status) {
			continue
		}
		out = append(out, *o)
	}
	return slicePage(out, offset, limit), int64(len(out)), nil
}

func (f *fakeOrders) Cancel(_ context.Context, tx *gorm.DB, orderNo string) (bool, error) {
	o, ok := f.byID[orderNo]
	if !ok || o.Status != model.OrderStatusPendingPayment {
		return false, nil
	}
	o.Status = model.OrderStatusCancelled
	return true, nil
}

func (f *fakeOrders) Ship(_ context.Context, _ *gorm.DB, orderNo string) (bool, error) {
	o, ok := f.byID[orderNo]
	if !ok || o.Status != model.OrderStatusPaid {
		return false, nil
	}
	o.Status = model.OrderStatusShipped
	return true, nil
}

func (f *fakeOrders) ConfirmReceipt(_ context.Context, _ *gorm.DB, orderNo string) (bool, error) {
	o, ok := f.byID[orderNo]
	if !ok || o.Status != model.OrderStatusShipped {
		return false, nil
	}
	o.Status = model.OrderStatusCompleted
	return true, nil
}

// fakeTx 单测事务运行器：直接执行回调（跨模块 fake 均忽略 tx 参数）。
type fakeTx struct{}

func (fakeTx) WithinTx(_ context.Context, fn func(tx *gorm.DB) error) error { return fn(nil) }

type fakeItems struct {
	byOrder map[string][]model.OrderItem
}

func newFakeItems() *fakeItems { return &fakeItems{byOrder: map[string][]model.OrderItem{}} }

func (f *fakeItems) Create(_ context.Context, _ *gorm.DB, item *model.OrderItem) error {
	f.byOrder[item.OrderNo] = append(f.byOrder[item.OrderNo], *item)
	return nil
}

func (f *fakeItems) ListByOrder(_ context.Context, orderNo string) ([]model.OrderItem, error) {
	return f.byOrder[orderNo], nil
}

func (f *fakeItems) ListByOrders(_ context.Context, orderNos []string) (map[string][]model.OrderItem, error) {
	out := make(map[string][]model.OrderItem)
	for _, no := range orderNos {
		out[no] = f.byOrder[no]
	}
	return out, nil
}

// ---- fake 幂等缓存（镜像 SETNX+EX 语义）----

type idemEntry struct {
	value string
	exp   time.Time
}

type fakeIdemCache struct {
	mu   sync.Mutex
	keys map[string]idemEntry
	err  error
	dels int
}

func newFakeIdemCache() *fakeIdemCache { return &fakeIdemCache{keys: map[string]idemEntry{}} }

func (f *fakeIdemCache) Ping(context.Context) error { return nil }
func (f *fakeIdemCache) Close() error               { return nil }

func (f *fakeIdemCache) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.keys[key]
	if !ok || time.Now().After(e.exp) {
		return "", cache.ErrMiss
	}
	return e.value, nil
}

func (f *fakeIdemCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = idemEntry{value: value, exp: time.Now().Add(ttl)}
	return nil
}

func (f *fakeIdemCache) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dels++
	delete(f.keys, key)
	return nil
}

// Eval 镜像 idemScript：SETNX（键不存在才写入）+ EX。
func (f *fakeIdemCache) Eval(_ context.Context, _ string, keys []string, args ...any) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if _, exists := f.keys[keys[0]]; exists {
		return 0, nil
	}
	value := args[0].(string)
	ttl := time.Duration(args[1].(int64)) * time.Second
	f.keys[keys[0]] = idemEntry{value: value, exp: time.Now().Add(ttl)}
	return 1, nil
}

// ---- fake 跨模块服务 ----

// fakeProducts 以 SKU 表模拟 product 模块：offSale 标记商品下架。
type fakeProducts struct {
	skus      map[int64]*productmodel.SKU
	offSale   map[int64]bool
	details   map[int64]*productmodel.ProductDetail
	deductErr error // 模拟事务内基础设施故障（如 DB 超时）
}

func newFakeProducts() *fakeProducts {
	return &fakeProducts{skus: map[int64]*productmodel.SKU{}, offSale: map[int64]bool{}, details: map[int64]*productmodel.ProductDetail{}}
}

func (f *fakeProducts) seed(skuID, productID int64, price int64, stock int) {
	f.skus[skuID] = &productmodel.SKU{ID: skuID, ProductID: productID, Specs: json.RawMessage(`{"color":"红"}`), Price: price, Stock: stock}
	f.details[productID] = &productmodel.ProductDetail{
		Product: productmodel.Product{ID: productID, Title: "商品", Status: productmodel.ProductStatusOnSale},
	}
}

func (f *fakeProducts) GetSKU(_ context.Context, id int64) (*productmodel.SKU, error) {
	s, ok := f.skus[id]
	if !ok {
		return nil, productsvc.ErrSKUNotFound
	}
	return s, nil
}

func (f *fakeProducts) GetSKUForUpdate(ctx context.Context, _ *gorm.DB, id int64) (*productmodel.SKU, error) {
	s, err := f.GetSKU(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.offSale[s.ProductID] {
		return nil, productsvc.ErrProductNotFound
	}
	return s, nil
}

func (f *fakeProducts) GetDetail(_ context.Context, productID int64) (*productmodel.ProductDetail, error) {
	if f.offSale[productID] {
		return nil, productsvc.ErrProductNotFound
	}
	d, ok := f.details[productID]
	if !ok {
		return nil, productsvc.ErrProductNotFound
	}
	return d, nil
}

func (f *fakeProducts) DeductStock(_ context.Context, _ *gorm.DB, skuID int64, quantity int) (bool, error) {
	if f.deductErr != nil {
		return false, f.deductErr
	}
	s, ok := f.skus[skuID]
	if !ok || s.Stock < quantity {
		return false, nil
	}
	s.Stock -= quantity
	return true, nil
}

func (f *fakeProducts) RestoreStock(_ context.Context, _ *gorm.DB, skuID int64, quantity int) error {
	if s, ok := f.skus[skuID]; ok {
		s.Stock += quantity
	}
	return nil
}

// fakeCoupons 以券表模拟 coupon 模块。
type fakeCoupons struct {
	coupons map[int64]*couponmodel.UserCouponView
	owners  map[int64]int64
}

func newFakeCoupons() *fakeCoupons {
	return &fakeCoupons{coupons: map[int64]*couponmodel.UserCouponView{}, owners: map[int64]int64{}}
}

// seed 一张属于 userID 的可用券；minAmount=0 表示直减券，否则为满减券。
func (f *fakeCoupons) seed(id, userID, value, minAmount int64) {
	now := time.Now()
	f.owners[id] = userID
	f.coupons[id] = &couponmodel.UserCouponView{
		ID: id, TemplateID: 1, Name: "券", Value: value, MinAmount: minAmount,
		Status:    couponmodel.CouponStatusUnused,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}
}

func (f *fakeCoupons) GetUsable(_ context.Context, userID, couponID int64) (*couponmodel.UserCouponView, error) {
	c, ok := f.coupons[couponID]
	if !ok {
		return nil, couponsvc.ErrCouponNotFound
	}
	if f.owners[couponID] != userID {
		return nil, couponsvc.ErrCouponNotFound
	}
	if c.Status == couponmodel.CouponStatusUsed {
		return nil, couponsvc.ErrCouponUsed
	}
	now := time.Now()
	if now.Before(c.ValidFrom) || now.After(c.ValidUntil) {
		return nil, couponsvc.ErrCouponExpired
	}
	return c, nil
}

func (f *fakeCoupons) UseCoupon(_ context.Context, _ *gorm.DB, userID, couponID int64) error {
	c, ok := f.coupons[couponID]
	if !ok || f.owners[couponID] != userID {
		return couponsvc.ErrCouponNotFound
	}
	if c.Status != couponmodel.CouponStatusUnused {
		return couponsvc.ErrCouponUsed
	}
	now := time.Now()
	if now.Before(c.ValidFrom) || now.After(c.ValidUntil) {
		return couponsvc.ErrCouponExpired
	}
	c.Status = couponmodel.CouponStatusUsed
	return nil
}

func (f *fakeCoupons) RollbackCoupon(_ context.Context, _ *gorm.DB, userID, couponID int64) error {
	c, ok := f.coupons[couponID]
	if !ok || f.owners[couponID] != userID {
		return couponsvc.ErrCouponNotFound
	}
	if c.Status != couponmodel.CouponStatusUsed {
		return fmt.Errorf("%w: coupon %d", couponsvc.ErrCouponRollbackFailed, couponID)
	}
	c.Status = couponmodel.CouponStatusUnused
	return nil
}

// fakeCart 模拟 cart 模块。
type fakeCart struct {
	items   map[int64][]cartmodel.CartItemView // userID → 条目
	itemIDs map[int64]int64
}

func newFakeCart() *fakeCart {
	return &fakeCart{items: map[int64][]cartmodel.CartItemView{}, itemIDs: map[int64]int64{}}
}

func (f *fakeCart) seed(userID, skuID int64, quantity int) {
	view := cartmodel.CartItemView{CartItem: cartmodel.CartItem{UserID: userID, SKUID: skuID, Quantity: quantity}}
	view.ID = int64(len(f.itemIDs) + 1)
	f.itemIDs[view.ID] = view.ID
	f.items[userID] = append(f.items[userID], view)
}

func (f *fakeCart) LockItems(_ context.Context, _ *gorm.DB, userID int64) ([]cartmodel.CartItem, error) {
	items := make([]cartmodel.CartItem, 0, len(f.items[userID]))
	for _, view := range f.items[userID] {
		items = append(items, view.CartItem)
	}
	return items, nil
}

func (f *fakeCart) DeletePurchased(_ context.Context, _ *gorm.DB, userID int64, itemIDs []int64) error {
	ids := make(map[int64]bool, len(itemIDs))
	for _, id := range itemIDs {
		ids[id] = true
	}
	var kept []cartmodel.CartItemView
	for _, v := range f.items[userID] {
		if !ids[v.ID] {
			kept = append(kept, v)
		}
	}
	f.items[userID] = kept
	return nil
}

// fakeUsers 模拟 user 模块。
type fakeUsers struct {
	addresses map[int64]*usermodel.Address
}

func newFakeUsers() *fakeUsers { return &fakeUsers{addresses: map[int64]*usermodel.Address{}} }

func (f *fakeUsers) seed(id, userID int64) {
	f.addresses[id] = &usermodel.Address{
		ID: id, UserID: userID, Receiver: "张三", Phone: "13800138000",
		Province: "广东省", City: "深圳市", District: "南山区", Detail: "科技园 1 号",
	}
}

func (f *fakeUsers) GetAddress(_ context.Context, userID, id int64) (*usermodel.Address, error) {
	a, ok := f.addresses[id]
	if !ok {
		return nil, usersvc.ErrAddressNotFound
	}
	if a.UserID != userID {
		return nil, usersvc.ErrAddressForbidden
	}
	return a, nil
}

// fakeNos 序列号生成器：1, 2, 3, ...
type fakeNos struct{ next int64 }

func (f *fakeNos) Next() (int64, error) {
	f.next++
	return f.next, nil
}

// ---- 测试夹具 ----

type fixture struct {
	svc     Service
	orders  *fakeOrders
	items   *fakeItems
	cache   *fakeIdemCache
	prods   *fakeProducts
	coupons *fakeCoupons
	cart    *fakeCart
	users   *fakeUsers
}

func newFixture() *fixture {
	orders, items, cache := newFakeOrders(), newFakeItems(), newFakeIdemCache()
	prods, coupons, cart, users := newFakeProducts(), newFakeCoupons(), newFakeCart(), newFakeUsers()
	svc := New(
		repository.Store{Orders: orders, Items: items, Tx: fakeTx{}},
		cache, &fakeNos{}, prods, coupons, cart, users,
	)
	return &fixture{svc: svc, orders: orders, items: items, cache: cache, prods: prods, coupons: coupons, cart: cart, users: users}
}

// seed 标准商品 + 地址 + 券：skuID=1 售价 100 分 库存 10。
func (fx *fixture) seed(t *testing.T) {
	t.Helper()
	fx.prods.seed(1, 1, 100, 10)
	fx.prods.seed(2, 1, 200, 5)
	fx.users.seed(1, 42)
}

func slicePage[T any](in []T, offset, limit int) []T {
	if offset > len(in) {
		return nil
	}
	end := offset + limit
	if end > len(in) {
		end = len(in)
	}
	return in[offset:end]
}

func (fx *fixture) directParams(rid string, skuID int64, quantity int) CreateParams {
	return CreateParams{ClientRequestID: rid, AddressID: 1, Items: []ItemParams{{SKUID: skuID, Quantity: quantity}}}
}

// ---- 测试 ----

// 直购下单：金额/快照/状态/订单项/雪花订单号全链路正确。
func TestCreateDirectHappyPath(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("r1", 1, 2))
	require.NoError(t, err)
	require.False(t, res.Idempotent)
	o := res.Order
	require.Equal(t, "1", o.OrderNo, "订单号应为雪花序列")
	require.Equal(t, model.OrderStatusPendingPayment, o.Status)
	require.Equal(t, model.OrderTypeNormal, o.OrderType)
	require.Equal(t, int64(200), o.TotalAmount)
	require.Equal(t, int64(0), o.DiscountAmount)
	require.Equal(t, int64(200), o.PayAmount)
	require.Equal(t, "张三", o.Receiver)
	require.Equal(t, "13800138000", o.Phone)
	require.Equal(t, "广东省", o.Province)
	require.WithinDuration(t, time.Now().Add(normalExpire), o.ExpireAt, time.Minute)

	require.Len(t, o.Items, 1)
	it := o.Items[0]
	require.Equal(t, int64(1), it.SKUID)
	require.Equal(t, "商品", it.Title)
	require.Equal(t, int64(100), it.Price)
	require.Equal(t, 2, it.Quantity)
	require.Equal(t, int64(200), it.Subtotal)

	require.Equal(t, 8, fx.prods.skus[1].Stock, "库存应扣减 2")
}

// 购物车结算：读取购物车条目、金额累计、结算后清空购物车。
func TestCreateFromCart(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.cart.seed(42, 1, 2)
	fx.cart.seed(42, 2, 1)

	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r-cart", AddressID: 1, FromCart: true,
	})
	require.NoError(t, err)
	require.Len(t, res.Order.Items, 2)
	require.Equal(t, int64(400), res.Order.TotalAmount, "100*2 + 200*1")
	require.Empty(t, fx.cart.items[42], "结算后购物车应清空")
	require.Equal(t, 8, fx.prods.skus[1].Stock)
	require.Equal(t, 4, fx.prods.skus[2].Stock)
}

// 幂等：同一 client_request_id 重复提交只生成一单并返回同一订单号。
func TestCreateIdempotent(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	first, err := fx.svc.Create(context.Background(), 42, fx.directParams("same-req", 1, 1))
	require.NoError(t, err)
	require.False(t, first.Idempotent)

	second, err := fx.svc.Create(context.Background(), 42, fx.directParams("same-req", 1, 1))
	require.NoError(t, err)
	require.True(t, second.Idempotent)
	require.Equal(t, first.Order.OrderNo, second.Order.OrderNo)
	require.Len(t, fx.orders.byID, 1, "只应生成一单")
	require.Equal(t, 9, fx.prods.skus[1].Stock, "库存只扣一次")
}

// 幂等键在途：键已存在但订单未落库（并发）→ 返回订单号供轮询。
func TestCreateIdempotentInFlight(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	require.NoError(t, fx.cache.Set(context.Background(), idemKey(42, "inflight"), "12345", idemTTL))
	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("inflight", 1, 1))
	require.NoError(t, err)
	require.True(t, res.Idempotent)
	require.True(t, res.Processing)
	require.Empty(t, res.Order.Status, "订单尚未落库时不能伪装为待支付成功")
	require.Equal(t, "12345", res.Order.OrderNo)
}

// 幂等键随下单失败释放：库存不足修正后可重试。
func TestCreateFailureReleasesIdempotencyKey(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, fx.directParams("retry", 1, 99))
	require.ErrorIs(t, err, ErrInsufficientStock)

	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("retry", 1, 1))
	require.NoError(t, err)
	require.False(t, res.Idempotent, "失败后幂等键应释放，可重新下单")
}

// 基础设施失败（事务内 DB 超时，且确认订单未落库）：幂等键释放，
// 恢复后重试可以重新创建，而不是永久返回不存在的订单号。
func TestCreateInfraFailureReleasesMissingOrderReservation(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	fx.prods.deductErr = errors.New("db timeout")
	_, err := fx.svc.Create(context.Background(), 42, fx.directParams("infra", 1, 1))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidInput, "基础设施失败不应包装为输入错误")
	require.Equal(t, 1, fx.cache.dels, "确认订单未落库后应释放幂等键")

	// 恢复后重试：重新获得幂等键并成功创建。
	fx.prods.deductErr = nil
	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("infra", 1, 1))
	require.NoError(t, err)
	require.False(t, res.Idempotent)
	require.Len(t, fx.orders.byID, 1)
}

// 基础设施失败后无法确认订单是否提交：保留幂等键，重试返回明确的在途结果，
// 不冒险创建第二单。
func TestCreateInfraFailureKeepsUnknownReservation(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.prods.deductErr = errors.New("db timeout")
	fx.orders.getErr = errors.New("order lookup timeout")

	_, err := fx.svc.Create(context.Background(), 42, fx.directParams("unknown", 1, 1))
	require.Error(t, err)
	require.Zero(t, fx.cache.dels)

	fx.prods.deductErr = nil
	fx.orders.getErr = nil
	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("unknown", 1, 1))
	require.NoError(t, err)
	require.True(t, res.Idempotent)
	require.True(t, res.Processing)
	require.Empty(t, fx.orders.byID, "未知状态下不得生成第二单")
}

// 库存不足：条件扣减失败 → 下单拒绝，库存不变。
func TestCreateInsufficientStock(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, fx.directParams("r2", 2, 10))
	require.ErrorIs(t, err, ErrInsufficientStock)
	require.Equal(t, 5, fx.prods.skus[2].Stock, "库存不足时不得扣减")
	require.Empty(t, fx.orders.byID, "不得生成订单")
}

// 事务内商品服务错误应翻译为订单模块错误，而不是泄漏为 HTTP 500。
func TestCreateTranslatesTransactionalProductError(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.prods.deductErr = productsvc.ErrSKUNotFound

	_, err := fx.svc.Create(context.Background(), 42, fx.directParams("product-error", 1, 1))
	require.ErrorIs(t, err, ErrSKUNotFound)
	require.NotErrorIs(t, err, productsvc.ErrSKUNotFound)
}

// SKU 不存在 / 商品下架：下单拒绝。
func TestCreateSKURejects(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, fx.directParams("r3", 999, 1))
	require.ErrorIs(t, err, ErrSKUNotFound)

	fx.prods.offSale[1] = true
	_, err = fx.svc.Create(context.Background(), 42, fx.directParams("r4", 1, 1))
	require.ErrorIs(t, err, ErrSKUUnavailable)
}

// 购物车为空：结算拒绝。
func TestCreateEmptyCart(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, CreateParams{ClientRequestID: "r5", AddressID: 1, FromCart: true})
	require.ErrorIs(t, err, ErrCartEmpty)
}

// 满减券：门槛不足拒绝；满足门槛后 discount_amount/应付正确。
func TestCreateCouponThreshold(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	// 满 300 减 50：总额 200 → 门槛不足。
	fx.coupons.seed(1, 42, 50, 300)
	_, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r6", AddressID: 1, CouponID: 1,
		Items: []ItemParams{{SKUID: 1, Quantity: 2}},
	})
	require.ErrorIs(t, err, ErrCouponThresholdNotMet)

	// 总额 400 → 门槛满足：应付 = 400 - 50。
	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r7", AddressID: 1, CouponID: 1,
		Items: []ItemParams{{SKUID: 1, Quantity: 4}},
	})
	require.NoError(t, err)
	require.Equal(t, int64(400), res.Order.TotalAmount)
	require.Equal(t, int64(50), res.Order.DiscountAmount)
	require.Equal(t, int64(350), res.Order.PayAmount)
	require.NotNil(t, res.Order.CouponID)
	require.Equal(t, couponmodel.CouponStatusUsed, fx.coupons.coupons[1].Status, "下单成功后券应核销")
}

// 直减券：券额封顶为商品总额（应付不为负）。
func TestCreateCouponCappedAtTotal(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	fx.coupons.seed(2, 42, 500, 0) // 直减 500，总额仅 100
	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r8", AddressID: 1, CouponID: 2,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.NoError(t, err)
	require.Equal(t, int64(100), res.Order.DiscountAmount, "券额不得超过商品总额")
	require.Equal(t, int64(0), res.Order.PayAmount)
}

// 券不可用：不存在 / 已用。
func TestCreateCouponUnusable(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r9", AddressID: 1, CouponID: 999,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrCouponNotFound)

	fx.coupons.seed(3, 42, 10, 0)
	fx.coupons.coupons[3].Status = couponmodel.CouponStatusUsed
	_, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r10", AddressID: 1, CouponID: 3,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrCouponUsed)
}

// 券 owner 校验：订单用户不能使用其他用户持有的券。
func TestCreateRejectsOtherUsersCoupon(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.coupons.seed(12, 7, 10, 0)

	_, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "coupon-owner", AddressID: 1, CouponID: 12,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrCouponNotFound)
}

// 地址：不存在 404 / 归属他人 403。
func TestCreateAddressRejects(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r11", AddressID: 999, Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrAddressNotFound)

	// 归属他人：种子地址 userID=42，用 userID=7 下单。
	_, err = fx.svc.Create(context.Background(), 7, CreateParams{
		ClientRequestID: "r12", AddressID: 1, Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrAddressForbidden)
}

// 取消待支付订单：回补库存 + 回退券；重复取消拒绝。
func TestCancelRestoresStockAndCoupon(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.coupons.seed(4, 42, 50, 0)

	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r13", AddressID: 1, CouponID: 4,
		Items: []ItemParams{{SKUID: 1, Quantity: 2}},
	})
	require.NoError(t, err)

	err = fx.svc.Cancel(context.Background(), 42, res.Order.OrderNo)
	require.NoError(t, err)
	require.Equal(t, model.OrderStatusCancelled, fx.orders.byID[res.Order.OrderNo].Status)
	require.Equal(t, 10, fx.prods.skus[1].Stock, "取消后库存应回补")
	require.Equal(t, couponmodel.CouponStatusUnused, fx.coupons.coupons[4].Status, "取消后券应回退")

	// 重复取消：已取消 → 已取消 非法跃迁（状态机拒绝）。
	err = fx.svc.Cancel(context.Background(), 42, res.Order.OrderNo)
	require.ErrorIs(t, err, ErrIllegalTransition)
	require.Equal(t, 10, fx.prods.skus[1].Stock, "重复取消不得重复回补")
}

// 状态机：待支付不可确认收货/发货；已支付不可取消。
func TestStateMachineRejectsIllegalTransitions(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("r14", 1, 1))
	require.NoError(t, err)
	no := res.Order.OrderNo

	// 待支付 → 已完成：非法。
	err = fx.svc.ConfirmReceipt(context.Background(), 42, no)
	require.ErrorIs(t, err, ErrIllegalTransition)
	// 待支付 → 已发货：非法。
	err = fx.svc.Ship(context.Background(), no)
	require.ErrorIs(t, err, ErrIllegalTransition)

	// 支付后（直接置状态模拟支付回调）：已支付 → 已发货 → 已完成 合法。
	fx.orders.byID[no].Status = model.OrderStatusPaid
	require.NoError(t, fx.svc.Ship(context.Background(), no))
	require.Equal(t, model.OrderStatusShipped, fx.orders.byID[no].Status)
	require.NoError(t, fx.svc.ConfirmReceipt(context.Background(), 42, no))
	require.Equal(t, model.OrderStatusCompleted, fx.orders.byID[no].Status)

	// 已完成后再取消：非法。
	err = fx.svc.Cancel(context.Background(), 42, no)
	require.ErrorIs(t, err, ErrIllegalTransition)
}

// owner 校验（防 IDOR）：他人订单的详情/取消/确认收货被拒。
func TestOrderOwnerEnforced(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	res, err := fx.svc.Create(context.Background(), 42, fx.directParams("r15", 1, 1))
	require.NoError(t, err)
	no := res.Order.OrderNo

	_, err = fx.svc.GetDetail(context.Background(), 7, no)
	require.ErrorIs(t, err, ErrOrderForbidden)
	err = fx.svc.Cancel(context.Background(), 7, no)
	require.ErrorIs(t, err, ErrOrderForbidden)
	err = fx.svc.ConfirmReceipt(context.Background(), 7, no)
	require.ErrorIs(t, err, ErrOrderForbidden)

	// 不存在的订单 404。
	_, err = fx.svc.GetDetail(context.Background(), 42, "999")
	require.ErrorIs(t, err, ErrOrderNotFound)
}

// 列表：状态筛选 + 分页 + 订单项随附。
func TestListWithStatusFilterAndPagination(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	for i := 0; i < 3; i++ {
		res, err := fx.svc.Create(context.Background(), 42, fx.directParams("r16-"+strconv.Itoa(i), 1, 1))
		require.NoError(t, err)
		if i == 0 {
			require.NoError(t, fx.svc.Cancel(context.Background(), 42, res.Order.OrderNo))
		}
	}

	// 全部：3 单。
	all, total, err := fx.svc.List(context.Background(), 42, "", 1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, all, 3)
	for _, v := range all {
		require.Len(t, v.Items, 1, "列表项应随附订单项")
	}

	// 状态筛选 cancelled：1 单。
	_, total, err = fx.svc.List(context.Background(), 42, model.OrderStatusCancelled, 1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)

	// 分页 page_size=2：第 2 页 1 单。
	page2, total, err := fx.svc.List(context.Background(), 42, "", 2, 2)
	require.NoError(t, err)
	require.Equal(t, int64(3), total)
	require.Len(t, page2, 1)

	// 非法状态 400。
	_, _, err = fx.svc.List(context.Background(), 42, "unknown", 1, 10)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// 输入校验：缺 client_request_id / 直购空 items / from_cart 与 items 互斥。
func TestCreateInputValidation(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.Create(context.Background(), 42, CreateParams{AddressID: 1, Items: []ItemParams{{SKUID: 1, Quantity: 1}}})
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r17", AddressID: 1, Items: nil,
	})
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r18", AddressID: 1, FromCart: true,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r19", AddressID: 1, Items: []ItemParams{{SKUID: 1, Quantity: 0}},
	})
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "r20", AddressID: 1, CouponID: -1,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}

// 直购同 SKU 多行：合并为一行（只扣一次库存、只生成一条订单项）；
// 合并后超 99 上限明确拒绝（金额敏感路径不做静默裁剪）。
func TestCreateDirectMergesDuplicateSKUs(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m1", AddressID: 1,
		Items: []ItemParams{{SKUID: 1, Quantity: 2}, {SKUID: 1, Quantity: 3}},
	})
	require.NoError(t, err)
	require.Len(t, res.Order.Items, 1, "同 SKU 应合并为一条订单项")
	require.Equal(t, 5, res.Order.Items[0].Quantity)
	require.Equal(t, int64(500), res.Order.TotalAmount)
	require.Equal(t, 5, fx.prods.skus[1].Stock, "库存只扣一次 5")

	_, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m2", AddressID: 1,
		Items: []ItemParams{{SKUID: 1, Quantity: 60}, {SKUID: 1, Quantity: 60}},
	})
	require.ErrorIs(t, err, ErrInvalidInput, "合并后超上限应拒绝")

	// 合并不应影响其他 SKU 的独立行。
	res, err = fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m3", AddressID: 1,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}, {SKUID: 2, Quantity: 1}, {SKUID: 1, Quantity: 1}},
	})
	require.NoError(t, err)
	require.Len(t, res.Order.Items, 2)
	require.Equal(t, int64(400), res.Order.TotalAmount, "100*2 + 200*1")
}

// 取消多条目订单：全部条目库存逐一回补。
func TestCancelRestoresAllItems(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m4", AddressID: 1,
		Items: []ItemParams{{SKUID: 1, Quantity: 2}, {SKUID: 2, Quantity: 1}},
	})
	require.NoError(t, err)
	require.NoError(t, fx.svc.Cancel(context.Background(), 42, res.Order.OrderNo))
	require.Equal(t, 10, fx.prods.skus[1].Stock)
	require.Equal(t, 5, fx.prods.skus[2].Stock)
}

// 取消时券已非 used（外部扰动）：取消失败，不产生部分状态。
func TestCancelCouponRollbackFailure(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.coupons.seed(9, 42, 50, 0)

	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m5", AddressID: 1, CouponID: 9,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.NoError(t, err)

	// 模拟外部把券改回 unused：回退条件更新将失败。
	fx.coupons.coupons[9].Status = couponmodel.CouponStatusUnused
	err = fx.svc.Cancel(context.Background(), 42, res.Order.OrderNo)
	require.ErrorIs(t, err, ErrCouponUsed)
}

// 已过期券：GetUsable 拒绝下单。
func TestCreateExpiredCoupon(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.coupons.seed(10, 42, 50, 0)
	fx.coupons.coupons[10].ValidUntil = time.Now().Add(-time.Minute)

	_, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m6", AddressID: 1, CouponID: 10,
		Items: []ItemParams{{SKUID: 1, Quantity: 1}},
	})
	require.ErrorIs(t, err, ErrCouponExpired)
}

// 不存在的订单：详情/取消/发货/确认收货一律 ErrOrderNotFound。
func TestNotFoundOnMissingOrder(t *testing.T) {
	fx := newFixture()
	fx.seed(t)

	_, err := fx.svc.GetDetail(context.Background(), 42, "999")
	require.ErrorIs(t, err, ErrOrderNotFound)
	err = fx.svc.Cancel(context.Background(), 42, "999")
	require.ErrorIs(t, err, ErrOrderNotFound)
	err = fx.svc.Ship(context.Background(), "999")
	require.ErrorIs(t, err, ErrOrderNotFound)
	err = fx.svc.ConfirmReceipt(context.Background(), 42, "999")
	require.ErrorIs(t, err, ErrOrderNotFound)
}

// 购物车结算 + 券：金额含券额、购物车清空、库存扣减、券核销。
func TestCreateFromCartWithCoupon(t *testing.T) {
	fx := newFixture()
	fx.seed(t)
	fx.cart.seed(42, 1, 2)
	fx.cart.seed(42, 2, 1)
	fx.coupons.seed(11, 42, 300, 300) // 满 300 减 300

	res, err := fx.svc.Create(context.Background(), 42, CreateParams{
		ClientRequestID: "m7", AddressID: 1, FromCart: true, CouponID: 11,
	})
	require.NoError(t, err)
	require.Equal(t, int64(400), res.Order.TotalAmount)
	require.Equal(t, int64(300), res.Order.DiscountAmount)
	require.Equal(t, int64(100), res.Order.PayAmount)
	require.Empty(t, fx.cart.items[42])
	require.Equal(t, 8, fx.prods.skus[1].Stock)
	require.Equal(t, 4, fx.prods.skus[2].Stock)
	require.Equal(t, couponmodel.CouponStatusUsed, fx.coupons.coupons[11].Status)
}
