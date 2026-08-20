// service 层单元测试（中间 seam）：fake 活动仓储 + fake product 服务 + fake 缓存
// （类型化原子能力在 Go 内以互斥锁模拟），覆盖活动校验、SKU 校验、
// 上架预热（未开始覆盖/进行中只减不增）、下架清除与预扣拒绝、预扣各失败分支。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
)

// ---- fake 活动仓储 ----

type fakeActivities struct {
	byID                 map[int64]*model.Activity
	order                int64
	respectContextCancel bool
	updateStatusErr      error
}

func newFakeActivities() *fakeActivities {
	return &fakeActivities{byID: map[int64]*model.Activity{}}
}

func (f *fakeActivities) Create(_ context.Context, a *model.Activity) error {
	f.order++
	a.ID = f.order
	a.CreatedAt = time.Now()
	f.byID[a.ID] = a
	return nil
}

func (f *fakeActivities) Update(_ context.Context, a *model.Activity) error {
	if v, ok := f.byID[a.ID]; ok {
		v.SKUID, v.Title, v.Price, v.Stock = a.SKUID, a.Title, a.Price, a.Stock
		v.PerUserLimit, v.StartAt, v.EndAt = a.PerUserLimit, a.StartAt, a.EndAt
	}
	return nil
}

func (f *fakeActivities) UpdateInTx(ctx context.Context, _ *transaction.Handle, a *model.Activity) error {
	if err := f.Update(ctx, a); err != nil {
		return err
	}
	if v, ok := f.byID[a.ID]; ok {
		v.Status = a.Status
	}
	return nil
}

func (f *fakeActivities) GetByID(_ context.Context, id int64) (*model.Activity, error) {
	return f.byID[id], nil
}

func (f *fakeActivities) GetByIDForUpdate(ctx context.Context, _ *transaction.Handle, id int64) (*model.Activity, error) {
	return f.GetByID(ctx, id)
}

func (f *fakeActivities) WithinTx(_ context.Context, fn func(tx *transaction.Handle) error) error {
	return fn(nil)
}

func (f *fakeActivities) List(context.Context) ([]model.Activity, error) {
	out := make([]model.Activity, 0, len(f.byID))
	for _, v := range f.byID {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (f *fakeActivities) UpdateStatus(ctx context.Context, id int64, status string) error {
	if f.respectContextCancel && ctx.Err() != nil {
		return ctx.Err()
	}
	if f.updateStatusErr != nil {
		return f.updateStatusErr
	}
	if v, ok := f.byID[id]; ok {
		v.Status = status
	}
	return nil
}

// DeductStock 模拟 GORM 条件扣减：库存不足返回 (false, nil)。
func (f *fakeActivities) DeductStock(_ context.Context, _ *transaction.Handle, id int64, quantity int) (bool, error) {
	v, ok := f.byID[id]
	if !ok || v.Stock < quantity {
		return false, nil
	}
	v.Stock -= quantity
	return true, nil
}

// RestoreStock 模拟事务内回补活动库存。
func (f *fakeActivities) RestoreStock(_ context.Context, _ *transaction.Handle, id int64, quantity int) error {
	v, ok := f.byID[id]
	if !ok {
		return nil
	}
	v.Stock += quantity
	return nil
}

// ---- fake product 服务 ----

type fakeProducts struct {
	skus     map[int64]*productmodel.SKU
	products map[int64]*productmodel.Product
}

func newFakeProducts() *fakeProducts {
	return &fakeProducts{skus: map[int64]*productmodel.SKU{}, products: map[int64]*productmodel.Product{}}
}

func (f *fakeProducts) seed(skuID int64) {
	f.skus[skuID] = &productmodel.SKU{ID: skuID, ProductID: 1, Price: 100, Stock: 5, Specs: json.RawMessage(`{"color":"红"}`)}
	f.products[1] = &productmodel.Product{ID: 1, Title: "秒杀商品", Status: "on_sale"}
}

func (f *fakeProducts) GetSKU(_ context.Context, id int64) (*productmodel.SKU, error) {
	if s, ok := f.skus[id]; ok {
		return s, nil
	}
	return nil, productsvc.ErrSKUNotFound
}

func (f *fakeProducts) GetProduct(_ context.Context, id int64) (*productmodel.Product, error) {
	if p, ok := f.products[id]; ok {
		return p, nil
	}
	return nil, productsvc.ErrProductNotFound
}

// ---- fake 缓存（互斥锁模拟类型化原子缓存能力）----

type fakeCache struct {
	mu                   sync.Mutex
	stock                map[string]int  // flashsale:stock:{id} → 余量
	count                map[string]int  // flashsale:count:{id}:{user} → 已购数
	idem                 map[string]bool // flashsale:idem:{id}:{user}:{slot} → 槽位所有权键
	idemToken            map[string]string
	reservations         map[string]string
	rl                   map[string]int // flashsale:rl:{user} → 限流计数
	err                  error
	aofErr               error
	decreaseErr          error
	paused               map[string]string
	respectContextCancel bool
	delErrOnce           error
	delErr               error
	onDelError           func()
	onPause              func()
	releasePauseCalls    int
	// deductErr 仅作用于预扣能力（模拟预扣时基础设施故障）。
	deductErr error
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		stock:        map[string]int{},
		count:        map[string]int{},
		idem:         map[string]bool{},
		idemToken:    map[string]string{},
		reservations: map[string]string{},
		rl:           map[string]int{},
		paused:       map[string]string{},
	}
}

func (f *fakeCache) Ping(context.Context) error { return nil }
func (f *fakeCache) Close() error               { return nil }

func (f *fakeCache) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.stock[key]
	if ok {
		return intToStr(v), nil
	}
	if v, ok := f.reservations[key]; ok {
		return v, nil
	}
	if v, ok := f.idemToken[key]; ok {
		return v, nil
	}
	return "", cache.ErrMiss
}

func (f *fakeCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.stock[key] = strToInt(value)
	return nil
}

func (f *fakeCache) Del(ctx context.Context, key string) error {
	if f.respectContextCancel && ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErrOnce != nil {
		err := f.delErrOnce
		f.delErrOnce = nil
		if f.onDelError != nil {
			f.onDelError()
			f.onDelError = nil
		}
		return err
	}
	if f.delErr != nil {
		return f.delErr
	}
	if f.err != nil {
		return f.err
	}
	delete(f.stock, key)
	delete(f.idem, key)
	delete(f.idemToken, key)
	delete(f.reservations, key)
	delete(f.rl, key)
	delete(f.paused, key)
	return nil
}

func (f *fakeCache) AcquireIdempotency(_ context.Context, key, value string, _ time.Duration) (cache.IdempotencyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.idem[key] {
		return cache.IdempotencyExists, nil
	}
	f.idem[key] = true
	f.idemToken[key] = value
	return cache.IdempotencyAcquired, nil
}

func (f *fakeCache) ReleaseIdempotencyDurably(_ context.Context, key, value string, _ time.Duration) error {
	f.mu.Lock()
	if f.idemToken[key] != value {
		f.mu.Unlock()
		return errors.New("idempotency token mismatch")
	}
	delete(f.idem, key)
	delete(f.idemToken, key)
	f.mu.Unlock()
	return f.afterAOF(nil)
}

func (f *fakeCache) IncrementFixedWindow(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.rl[key]++
	return int64(f.rl[key]), nil
}

func (f *fakeCache) WarmFlashSaleStock(_ context.Context, p cache.FlashSaleWarmParams) (cache.FlashSaleWarmResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if cur, ok := f.stock[p.StockKey]; !ok || p.Stock < cur {
		f.stock[p.StockKey] = p.Stock
		return cache.FlashSaleStockUpdated, nil
	}
	return cache.FlashSaleStockRetained, nil
}

func (f *fakeCache) DecreaseFlashSaleStockDurably(_ context.Context, p cache.FlashSaleDecreaseParams, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decreaseErr != nil {
		return f.decreaseErr
	}
	stock, ok := f.stock[p.StockKey]
	if !ok || stock < p.Delta {
		return errors.New("flash-sale stock decrease exceeds sellable stock")
	}
	f.stock[p.StockKey] = stock - p.Delta
	return nil
}

func (f *fakeCache) PauseFlashSaleStockDurably(_ context.Context, p cache.FlashSalePauseParams, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onPause != nil {
		f.onPause()
		f.onPause = nil
	}
	if f.err != nil {
		return 0, f.err
	}
	if f.paused[p.PauseKey] != "" {
		return 0, errors.New("flash-sale stock is already paused")
	}
	stock, ok := f.stock[p.StockKey]
	if !ok {
		return 0, errors.New("flash-sale stock is missing during pause")
	}
	f.paused[p.PauseKey] = p.Token
	return stock, nil
}

func (f *fakeCache) ReleaseFlashSalePauseDurably(_ context.Context, pauseKey, token string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releasePauseCalls++
	if token == "" || f.paused[pauseKey] == token {
		delete(f.paused, pauseKey)
	}
	return nil
}

func (f *fakeCache) HoldFlashSalePauseDurably(_ context.Context, pauseKey string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused[pauseKey] = "fail-closed"
	return nil
}

func (f *fakeCache) PreDeductFlashSale(_ context.Context, p cache.FlashSalePreDeductParams) (cache.FlashSalePreDeductResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.deductErr != nil {
		return 0, f.deductErr
	}
	if token, ok := f.reservations[p.ReservationKey]; p.ReservationKey != "" && ok {
		if token != p.ReservationToken {
			return 0, errors.New("reservation token mismatch")
		}
		if p.IdempotencyKey != "" {
			f.idem[p.IdempotencyKey] = true
			f.idemToken[p.IdempotencyKey] = p.ReservationToken
		}
		return cache.FlashSaleAlreadyPreDeducted, nil
	}
	if f.paused[p.PauseKey] != "" {
		return cache.FlashSalePaused, nil
	}
	if !p.OnSale {
		return cache.FlashSaleOffline, nil
	}
	if p.Now.Before(p.StartAt) || p.Now.After(p.EndAt) {
		return cache.FlashSaleNotInWindow, nil
	}
	if f.stock[p.StockKey] <= 0 {
		return cache.FlashSaleSoldOut, nil
	}
	if f.count[p.CountKey] >= p.PerUserLimit {
		return cache.FlashSaleLimitReached, nil
	}
	f.stock[p.StockKey]--
	f.count[p.CountKey]++
	if p.ReservationKey != "" {
		f.reservations[p.ReservationKey] = p.ReservationToken
	}
	if p.IdempotencyKey != "" {
		f.idem[p.IdempotencyKey] = true
		f.idemToken[p.IdempotencyKey] = p.ReservationToken
	}
	return cache.FlashSalePreDeducted, nil
}

func (f *fakeCache) PreDeductFlashSaleDurably(ctx context.Context, p cache.FlashSalePreDeductParams, _ time.Duration) (cache.FlashSalePreDeductResult, error) {
	result, err := f.PreDeductFlashSale(ctx, p)
	return result, f.afterAOF(err)
}

func (f *fakeCache) EnsureFlashSaleReservation(_ context.Context, p cache.FlashSaleEnsureReservationParams) (cache.FlashSaleEnsureReservationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.reservations[p.ReservationKey] == p.ReservationToken {
		return cache.FlashSaleReservationPresent, nil
	}
	if token := f.reservations[p.ReservationKey]; token != "" {
		return 0, errors.New("reservation token mismatch")
	}
	if token := f.idemToken[p.IdempotencyKey]; token != "" && token != p.ReservationToken {
		return 0, errors.New("idempotency token mismatch")
	}
	stock, exists := f.stock[p.StockKey]
	if !exists {
		stock = p.FallbackStock
		f.stock[p.StockKey] = stock
	}
	if stock < p.Quantity {
		return 0, errors.New("insufficient stock during reservation recovery")
	}
	f.stock[p.StockKey] -= p.Quantity
	f.count[p.CountKey]++
	f.reservations[p.ReservationKey] = p.ReservationToken
	f.idem[p.IdempotencyKey] = true
	f.idemToken[p.IdempotencyKey] = p.ReservationToken
	return cache.FlashSaleReservationReinstated, nil
}

func (f *fakeCache) EnsureFlashSaleReservationDurably(ctx context.Context, p cache.FlashSaleEnsureReservationParams, _ time.Duration) (cache.FlashSaleEnsureReservationResult, error) {
	result, err := f.EnsureFlashSaleReservation(ctx, p)
	return result, f.afterAOF(err)
}

func (f *fakeCache) EnsureOrderedFlashSaleReservation(_ context.Context, p cache.FlashSaleEnsureOrderedReservationParams) (cache.FlashSaleEnsureReservationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if stock, exists := f.stock[p.StockKey]; !exists || p.FallbackStock < stock {
		f.stock[p.StockKey] = p.FallbackStock
	}
	if f.reservations[p.ReservationKey] == p.ReservationToken {
		return cache.FlashSaleReservationPresent, nil
	}
	if token := f.reservations[p.ReservationKey]; token != "" {
		return 0, errors.New("ordered reservation token mismatch")
	}
	if token := f.idemToken[p.IdempotencyKey]; token != "" && token != p.ReservationToken {
		return 0, errors.New("ordered idempotency token mismatch")
	}
	f.count[p.CountKey]++
	f.reservations[p.ReservationKey] = p.ReservationToken
	f.idem[p.IdempotencyKey] = true
	f.idemToken[p.IdempotencyKey] = p.ReservationToken
	return cache.FlashSaleReservationReinstated, nil
}

func (f *fakeCache) EnsureOrderedFlashSaleReservationDurably(ctx context.Context, p cache.FlashSaleEnsureOrderedReservationParams, _ time.Duration) (cache.FlashSaleEnsureReservationResult, error) {
	result, err := f.EnsureOrderedFlashSaleReservation(ctx, p)
	return result, f.afterAOF(err)
}

func (f *fakeCache) AdoptLegacyFlashSaleReservationDurably(_ context.Context, p cache.FlashSaleAdoptLegacyReservationParams, _ time.Duration) (cache.FlashSaleEnsureReservationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if stock, ok := f.stock[p.StockKey]; !ok || p.TargetStock < stock {
		f.stock[p.StockKey] = p.TargetStock
	}
	if count, ok := f.count[p.CountKey]; !ok || count < p.TargetUserCount {
		f.count[p.CountKey] = p.TargetUserCount
	}
	if token := f.reservations[p.ReservationKey]; token != "" && token != p.ReservationToken {
		return 0, errors.New("legacy reservation token mismatch")
	}
	already := f.reservations[p.ReservationKey] == p.ReservationToken
	f.reservations[p.ReservationKey] = p.ReservationToken
	f.idem[p.IdempotencyKey] = true
	f.idemToken[p.IdempotencyKey] = p.ReservationToken
	if already {
		return cache.FlashSaleReservationPresent, nil
	}
	return cache.FlashSaleReservationReinstated, nil
}

func (f *fakeCache) RestoreFlashSale(_ context.Context, p cache.FlashSaleRestoreParams) (cache.FlashSaleRestoreResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if f.reservations[p.ReservationKey] == "rolled_back:"+p.ReservationToken {
		return cache.FlashSaleAlreadyRestored, nil
	}
	shouldRestore := p.ReservationKey == ""
	if p.ReservationKey != "" {
		reservation := f.reservations[p.ReservationKey]
		if reservation != "" && reservation != p.ReservationToken {
			return 0, errors.New("reservation token mismatch")
		}
		shouldRestore = reservation == p.ReservationToken
		if !shouldRestore && p.AllowIdempotencyFallback {
			shouldRestore = f.idemToken[p.IdempotencyKey] == p.ReservationToken
		}
		if !shouldRestore && reservation == "" && p.AllowMissingReservation {
			if _, exists := f.stock[p.StockKey]; !exists {
				f.stock[p.StockKey] = p.FallbackStock
			}
			f.reservations[p.ReservationKey] = "rolled_back:" + p.ReservationToken
			delete(f.idem, p.IdempotencyKey)
			delete(f.idemToken, p.IdempotencyKey)
			return cache.FlashSaleRestored, nil
		}
	}
	if shouldRestore {
		if _, ok := f.stock[p.StockKey]; ok {
			f.stock[p.StockKey] += p.Quantity
		}
		if _, ok := f.count[p.CountKey]; ok {
			if p.ReservationKey == "" {
				f.count[p.CountKey] -= p.Quantity
			} else {
				f.count[p.CountKey]--
			}
		}
		if p.ReservationKey != "" {
			f.reservations[p.ReservationKey] = "rolled_back:" + p.ReservationToken
		}
	}
	if p.ReservationKey == "" || f.idemToken[p.IdempotencyKey] == p.ReservationToken {
		delete(f.idem, p.IdempotencyKey)
		delete(f.idemToken, p.IdempotencyKey)
	}
	if shouldRestore {
		return cache.FlashSaleRestored, nil
	}
	return cache.FlashSaleReservationMissing, nil
}

func (f *fakeCache) RestoreFlashSaleDurably(ctx context.Context, p cache.FlashSaleRestoreParams, _ time.Duration) (cache.FlashSaleRestoreResult, error) {
	result, err := f.RestoreFlashSale(ctx, p)
	return result, f.afterAOF(err)
}

func (f *fakeCache) afterAOF(err error) error {
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aofErr
}

// ---- 辅助 ----

func intToStr(v int) string { return strconv.Itoa(v) }
func strToInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// discardSeckill 只断言错误的抢购调用（成功时返回的订单号在发布测试中断言）。
func discardSeckill(svc Service, ctx context.Context, userID, activityID int64) error {
	_, err := svc.Seckill(ctx, userID, activityID, "test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	return err
}

// ---- 测试夹具 ----

// fakePublisher 记录发布的消息；err 模拟 MQ 发布失败（恒定失败，legacy 语义）；
// fails > 0 时前 fails 次发布以瞬时错误失败、之后成功（供有限重试测试）。
// 并发安全：并发抢购测试多 goroutine 共用同一实例。
type fakePublisher struct {
	mu       sync.Mutex
	queue    string
	body     []byte
	err      error
	fails    int
	attempts int
}

func (f *fakePublisher) Publish(_ context.Context, queue string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = queue
	f.body = body
	f.attempts++
	if f.fails > 0 {
		f.fails--
		return errors.New("mq transient failure")
	}
	return f.err
}

// attempts 读取发布次数（测试断言用）。
func (f *fakePublisher) attemptsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// fakeNos 雪花订单号替身：1, 2, 3, ...（并发安全：并发抢购测试多 goroutine 共用）。
type fakeNos struct {
	mu   sync.Mutex
	next int64
}

func (f *fakeNos) Next() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return f.next, nil
}

type fixture struct {
	svc           testFlashSaleService
	acts          *fakeActivities
	preDeductions *fakePreDeductions
	products      *fakeProducts
	cache         *fakeCache
	pub           *fakePublisher
}

type testFlashSaleService interface {
	Service
	PreDeduct(ctx context.Context, userID, activityID int64) error
}

func asTestFlashSaleService(svc Service) testFlashSaleService {
	return svc.(testFlashSaleService)
}

func newFixture() *fixture {
	acts := newFakeActivities()
	products := newFakeProducts()
	fc := newFakeCache()
	pub := &fakePublisher{}
	pd := newFakePreDeductions()
	svc := asTestFlashSaleService(New(repository.Store{Activities: acts, PreDeductions: pd, Tx: acts}, products, fc, limiter.RedisCounterConfig{}, pub, &fakeNos{}, metrics.New().Business()))
	return &fixture{svc: svc, acts: acts, preDeductions: pd, products: products, cache: fc, pub: pub}
}

// newFixtureLimited 同 newFixture，但启用按用户限流（Max 次 / Window）。
func newFixtureLimited(max int, window time.Duration) *fixture {
	acts := newFakeActivities()
	products := newFakeProducts()
	fc := newFakeCache()
	pub := &fakePublisher{}
	pd := newFakePreDeductions()
	svc := asTestFlashSaleService(New(
		repository.Store{Activities: acts, PreDeductions: pd, Tx: acts}, products, fc,
		limiter.RedisCounterConfig{Max: max, Window: window}, pub, &fakeNos{}, metrics.New().Business(),
	))
	return &fixture{svc: svc, acts: acts, preDeductions: pd, products: products, cache: fc, pub: pub}
}

// createActivity 有效参数活动：进行中窗口（1 分钟前开始，1 小时后结束）。
func (fx *fixture) createActivity(t *testing.T, mutate func(*ActivityParams)) *model.Activity {
	t.Helper()
	fx.products.seed(1)
	p := ActivityParams{
		SKUID:        1,
		Title:        "限时秒杀",
		Price:        9900,
		Stock:        100,
		PerUserLimit: 1,
		StartAt:      time.Now().Add(-time.Minute),
		EndAt:        time.Now().Add(time.Hour),
	}
	if mutate != nil {
		mutate(&p)
	}
	a, err := fx.svc.CreateActivity(context.Background(), p)
	require.NoError(t, err)
	return a
}

// ---- 活动校验（admin）----

func TestCreateActivityValidation(t *testing.T) {
	fx := newFixture()
	fx.products.seed(1)
	base := ActivityParams{
		SKUID:        1,
		Title:        "限时秒杀",
		Price:        9900,
		Stock:        100,
		PerUserLimit: 1,
		StartAt:      time.Now().Add(-time.Minute),
		EndAt:        time.Now().Add(time.Hour),
	}

	cases := []struct {
		name   string
		mutate func(*ActivityParams)
	}{
		{"空白标题", func(p *ActivityParams) { p.Title = "  " }},
		{"SKU 为 0", func(p *ActivityParams) { p.SKUID = 0 }},
		{"秒杀价为 0", func(p *ActivityParams) { p.Price = 0 }},
		{"秒杀价超过 100 万元", func(p *ActivityParams) { p.Price = 100_000_001 }},
		{"库存为 0", func(p *ActivityParams) { p.Stock = 0 }},
		{"限购为 0", func(p *ActivityParams) { p.PerUserLimit = 0 }},
		{"时间窗口倒置", func(p *ActivityParams) { p.EndAt = p.StartAt.Add(-time.Hour) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			_, err := fx.svc.CreateActivity(context.Background(), p)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}

	// SKU 不存在 → ErrInvalidInput（跨模块校验）。
	fx2 := newFixture()
	_, err := fx2.svc.CreateActivity(context.Background(), base)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 默认下架状态。
	a, err := fx.svc.CreateActivity(context.Background(), base)
	require.NoError(t, err)
	require.Equal(t, model.ActivityStatusOffSale, a.Status)

	base.Price = 100_000_000
	a, err = fx.svc.CreateActivity(context.Background(), base)
	require.NoError(t, err)
	require.Equal(t, int64(100_000_000), a.Price)
}

// ---- 上架预热 ----

// 上架：预热库存进 Redis，且与配置一致。
func TestPublishPrewarmsStock(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)

	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)])
	updated, err := fx.acts.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, model.ActivityStatusOnSale, updated.Status)
}

// 未开始的活动上架：覆盖已有存量（DEL+SET）。
func TestPublishNotStartedOverwrites(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	fx.cache.stock[stockKey(a.ID)] = 999 // 遗留旧值

	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)])
}

// 进行中的活动上架：SETNX 不覆盖存量。
func TestPublishInProgressKeepsExisting(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	fx.cache.stock[stockKey(a.ID)] = 42 // 已预热且部分预扣

	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	require.Equal(t, 42, fx.cache.stock[stockKey(a.ID)])
}

func TestPublishPropagatesWarmCacheError(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	fx.cache.err = context.DeadlineExceeded

	err := fx.svc.PublishActivity(context.Background(), a.ID)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	updated, getErr := fx.acts.GetByID(context.Background(), a.ID)
	require.NoError(t, getErr)
	require.Equal(t, model.ActivityStatusOffSale, updated.Status, "预热失败时活动不得上架")
}

// 已结束的活动不可上架。
func TestPublishEndedRejected(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) {
		p.EndAt = time.Now().Add(-time.Minute)
	})
	require.ErrorIs(t, fx.svc.PublishActivity(context.Background(), a.ID), ErrInvalidInput)
}

// 不存在的活动 → 404 语义。
func TestPublishNotFound(t *testing.T) {
	fx := newFixture()
	require.ErrorIs(t, fx.svc.PublishActivity(context.Background(), 999), ErrActivityNotFound)
}

// ---- 进行中编辑库存只减不增 ----

// 进行中（上架 + 窗口内）编辑：库存调高被拒，Redis 存量不变。
func TestUpdateInProgressRejectsIncrease(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.stock[stockKey(a.ID)] = 50 // 部分预扣后存量

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 200,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	err := fx.svc.UpdateActivity(context.Background(), a.ID, p)
	require.ErrorIs(t, err, ErrStockIncreaseInProgress)
	require.Equal(t, 50, fx.cache.stock[stockKey(a.ID)])

	// DB 也未被调高。
	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, 100, updated.Stock)
}

// 进行中编辑库存调低：Redis 按 MySQL 配置差额降低，保留未落单预扣量。
func TestUpdateInProgressDecreases(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.stock[stockKey(a.ID)] = 50

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 80,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.NoError(t, fx.svc.UpdateActivity(context.Background(), a.ID, p))

	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, 80, updated.Stock)
	require.Equal(t, 30, fx.cache.stock[stockKey(a.ID)])
}

// 进行中活动把窗口改到未来（未开始）+ 调高库存：仍应被拒（按当前进行中判定，
// 防止同一次编辑改窗口绕过"进行中只减不增"）。
func TestUpdateWindowShiftCannotBypass(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil) // 进行中窗口
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 200,
		PerUserLimit: a.PerUserLimit,
		StartAt:      time.Now().Add(time.Hour),
		EndAt:        time.Now().Add(2 * time.Hour),
	}
	require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), a.ID, p), ErrActivityFieldsLocked)

	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, 100, updated.Stock, "DB 不应被调高")
	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)])
}

func TestUpdateOnSaleFutureActivityCannotMoveIntoProgress(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: a.Stock,
		PerUserLimit: a.PerUserLimit, StartAt: time.Now().Add(-time.Minute), EndAt: time.Now().Add(time.Hour),
	}
	require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), a.ID, p), ErrActivityFieldsLocked)
	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)])
}

func TestUpdateInProgressImmutableFieldsRejected(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.products.seed(2)

	tests := []struct {
		name string
		mut  func(*ActivityParams)
	}{
		{"sku", func(p *ActivityParams) { p.SKUID = 2 }},
		{"price", func(p *ActivityParams) { p.Price-- }},
		{"limit", func(p *ActivityParams) { p.PerUserLimit++ }},
		{"start", func(p *ActivityParams) { p.StartAt = p.StartAt.Add(time.Minute) }},
		{"end", func(p *ActivityParams) { p.EndAt = p.EndAt.Add(time.Minute) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ActivityParams{
				SKUID: a.SKUID, Title: "允许改标题", Price: a.Price, Stock: a.Stock,
				PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
			}
			tc.mut(&p)
			require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), a.ID, p), ErrActivityFieldsLocked)
		})
	}
}

func TestUpdateSyncFailureFailsClosed(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.decreaseErr = errors.New("redis sync failed")

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 90,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.Error(t, fx.svc.UpdateActivity(context.Background(), a.ID, p))
	updated, err := fx.acts.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, 90, updated.Stock, "MySQL stock commits offline before Redis synchronization")
	require.Equal(t, model.ActivityStatusOffSale, updated.Status)
	_, exists := fx.cache.stock[stockKey(a.ID)]
	require.False(t, exists, "fail-closed edit must remove stale sellable stock")
}

func TestUpdateSyncFailureFailsClosedAfterRequestCancellation(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	ctx, cancel := context.WithCancel(context.Background())
	fx.acts.respectContextCancel = true
	fx.cache.respectContextCancel = true
	fx.cache.delErrOnce = errors.New("redis sync failed")
	fx.cache.onDelError = cancel

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 90,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.Error(t, fx.svc.UpdateActivity(ctx, a.ID, p))

	updated, err := fx.acts.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, model.ActivityStatusOffSale, updated.Status)
	_, exists := fx.cache.stock[stockKey(a.ID)]
	require.False(t, exists, "fail-closed cleanup must outlive the cancelled request")
}

func TestUpdateCannotReduceStockBelowAcceptedReservations(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.stock[stockKey(a.ID)] = 5

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 1,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), a.ID, p), ErrStockBelowAcceptedReservations)
	updated, err := fx.acts.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, 100, updated.Stock)
	require.Equal(t, model.ActivityStatusOnSale, updated.Status)
	require.Equal(t, 5, fx.cache.stock[stockKey(a.ID)])
}

// 未开始的已上架活动编辑：可覆盖 Redis 存量（DEL+SET）。
func TestUpdateNotStartedOverwrites(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.stock[stockKey(a.ID)] = 999

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 60,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.NoError(t, fx.svc.UpdateActivity(context.Background(), a.ID, p))
	require.Equal(t, 60, fx.cache.stock[stockKey(a.ID)])
}

// 未上架的活动编辑：不触碰 Redis。
func TestUpdateOfflineSkipsRedis(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	fx.cache.stock[stockKey(a.ID)] = 999 // 残留

	p := ActivityParams{
		SKUID: a.SKUID, Title: "改标题", Price: a.Price, Stock: 50,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.NoError(t, fx.svc.UpdateActivity(context.Background(), a.ID, p))
	require.Equal(t, 999, fx.cache.stock[stockKey(a.ID)])

	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, "改标题", updated.Title)
	require.Equal(t, 50, updated.Stock)
}

// 编辑边界：不存在 404；SKU 换绑需校验存在；非法参数 400 语义。
func TestUpdateEdgeCases(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)

	// 不存在。
	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 10,
		PerUserLimit: 1, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), 999, p), ErrActivityNotFound)

	// 换绑到不存在的 SKU。
	p.SKUID = 2
	require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), a.ID, p), ErrInvalidInput)
}

// ---- 下架 ----

// 下架：状态置 off_sale，清除预热库存，预扣被拒。
func TestUnpublishRejectsPreDeduct(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	require.NoError(t, fx.svc.UnpublishActivity(context.Background(), a.ID))

	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, model.ActivityStatusOffSale, updated.Status)
	_, ok := fx.cache.stock[stockKey(a.ID)]
	require.False(t, ok, "下架应清除预热库存")

	require.ErrorIs(t, fx.svc.PreDeduct(context.Background(), 1, a.ID), ErrOffline)
}

func TestUnpublishFinishesAfterRequestCancellation(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	ctx, cancel := context.WithCancel(context.Background())
	fx.acts.respectContextCancel = true
	fx.cache.respectContextCancel = true
	fx.cache.onPause = cancel
	require.NoError(t, fx.svc.UnpublishActivity(ctx, a.ID))

	updated, err := fx.acts.GetByID(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, model.ActivityStatusOffSale, updated.Status)
	_, exists := fx.cache.stock[stockKey(a.ID)]
	require.False(t, exists)
}

func TestUnpublishKeepsPauseWhenNeitherSafeStateCanBePersisted(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.releasePauseCalls = 0
	fx.cache.delErr = errors.New("redis delete failed")
	fx.acts.updateStatusErr = errors.New("mysql update failed")

	require.Error(t, fx.svc.UnpublishActivity(context.Background(), a.ID))
	require.Equal(t, "fail-closed", fx.cache.paused[pauseKey(a.ID)])
	require.Zero(t, fx.cache.releasePauseCalls)
}

func TestUnpublishKeepsPauseWhenDatabaseStillReportsOnSale(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.releasePauseCalls = 0
	fx.acts.updateStatusErr = errors.New("mysql update failed")

	require.Error(t, fx.svc.UnpublishActivity(context.Background(), a.ID))
	require.Equal(t, "fail-closed", fx.cache.paused[pauseKey(a.ID)])
	require.Zero(t, fx.cache.releasePauseCalls)
	_, stockExists := fx.cache.stock[stockKey(a.ID)]
	require.False(t, stockExists, "stock deletion alone is not enough to release the pause")
}

// ---- 预扣 ----

// 预扣成功：库存递减、用户计数递增。
func TestPreDeductOK(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	require.NoError(t, fx.svc.PreDeduct(context.Background(), 7, a.ID))
	require.Equal(t, 99, fx.cache.stock[stockKey(a.ID)])
	require.Equal(t, 1, fx.cache.count[countKey(a.ID, 7)])
}

// 预扣各失败分支：未开始/已结束（窗口外）、抢光、超限购、下架、活动不存在。
func TestPreDeductFailures(t *testing.T) {
	fx := newFixture()

	// 未开始。
	notStarted := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	require.NoError(t, fx.svc.PublishActivity(context.Background(), notStarted.ID))
	require.ErrorIs(t, fx.svc.PreDeduct(context.Background(), 1, notStarted.ID), ErrNotInWindow)

	// 已结束（已上架但窗口已过：直接改状态模拟活动结束后未下架）。
	ended := fx.createActivity(t, func(p *ActivityParams) {
		p.EndAt = time.Now().Add(-time.Minute)
	})
	require.NoError(t, fx.acts.UpdateStatus(context.Background(), ended.ID, model.ActivityStatusOnSale))
	require.ErrorIs(t, fx.svc.PreDeduct(context.Background(), 1, ended.ID), ErrNotInWindow)

	// 抢光：库存 1，两个用户各抢一次，第二次被拒。
	soldOut := fx.createActivity(t, func(p *ActivityParams) { p.Stock = 1 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), soldOut.ID))
	require.NoError(t, fx.svc.PreDeduct(context.Background(), 1, soldOut.ID))
	require.ErrorIs(t, fx.svc.PreDeduct(context.Background(), 2, soldOut.ID), ErrSoldOut)

	// 超限购：限购 2，同一用户第三次被拒。
	limited := fx.createActivity(t, func(p *ActivityParams) { p.PerUserLimit = 2 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), limited.ID))
	require.NoError(t, fx.svc.PreDeduct(context.Background(), 3, limited.ID))
	require.NoError(t, fx.svc.PreDeduct(context.Background(), 3, limited.ID))
	require.ErrorIs(t, fx.svc.PreDeduct(context.Background(), 3, limited.ID), ErrLimitReached)

	// 活动不存在。
	require.ErrorIs(t, fx.svc.PreDeduct(context.Background(), 1, 999), ErrActivityNotFound)
}

// 并发预扣不超卖：互斥锁模拟缓存原子性，20 用户抢 5 库存恰好 5 成功。
func TestPreDeductConcurrentNoOversell(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) { p.Stock = 5 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fx.svc.PreDeduct(context.Background(), int64(i+1), a.ID)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, ErrSoldOut)
		}
	}
	require.Equal(t, 5, ok, "并发预扣不得超卖")
	require.Equal(t, 0, fx.cache.stock[stockKey(a.ID)])
}

// ---- 列表 ----

func TestListActivities(t *testing.T) {
	fx := newFixture()
	a1 := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a1.ID))
	a2 := fx.createActivity(t, func(p *ActivityParams) { p.Title = "第二场" })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a2.ID))
	// 一场下架（手动）与一场已结束（先以有效窗口上架，再把仓储时间改到过去）。
	offSale := fx.createActivity(t, func(p *ActivityParams) { p.Title = "已下架" })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), offSale.ID))
	require.NoError(t, fx.svc.UnpublishActivity(context.Background(), offSale.ID))
	ended := fx.createActivity(t, func(p *ActivityParams) { p.Title = "已结束" })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), ended.ID))
	stored, err := fx.acts.GetByID(context.Background(), ended.ID)
	require.NoError(t, err)
	stored.StartAt, stored.EndAt = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)

	list, err := fx.svc.ListActivities(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 4, "后台列表应含全状态活动（含下架与已结束）")

	stateByID := map[int64]string{}
	for _, v := range list {
		stateByID[v.ID] = v.State
		require.Equal(t, "秒杀商品", v.ProductTitle, "后台列表应携带商品标题摘要")
		require.Equal(t, int64(1), v.SKU.ID, "后台列表应携带 SKU 摘要")
	}
	require.Equal(t, model.ActivityStateInProgress, stateByID[a1.ID])
	require.Equal(t, model.ActivityStateInProgress, stateByID[a2.ID])
	require.Equal(t, model.ActivityStateOffSale, stateByID[offSale.ID])
	require.Equal(t, model.ActivityStateEnded, stateByID[ended.ID])
}

// ---- 抢购（T11：限流 → 幂等键 → 原子预扣）----

// 抢购成功（T12）：返回订单号，且"抢购成功"消息已发布到异步落单队列
// （消息体 = order_no/user_id/activity_id，供消费者落单、前端轮询）。
func TestSeckillOK(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "ok")
	require.NoError(t, err)
	require.NotEmpty(t, result.OrderNo, "抢购成功应返回订单号供前端轮询")
	require.Equal(t, 99, fx.cache.stock[stockKey(a.ID)])
	require.Equal(t, 1, fx.cache.count[countKey(a.ID, 7)])
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, result.PreDeductionID)], "预扣成功应保留槽位幂等键")

	require.Equal(t, SeckillOrderQueue, fx.pub.queue, "消息应发布到异步落单队列")
	var msg SeckillSuccessMessage
	require.NoError(t, json.Unmarshal(fx.pub.body, &msg))
	require.Equal(t, result.OrderNo, msg.OrderNo)
	require.Equal(t, result.PreDeductionID, msg.PreDeductionID)
	require.Equal(t, int64(7), msg.UserID)
	require.Equal(t, a.ID, msg.ActivityID)
	require.Equal(t, a.SKUID, msg.SKUID)
	require.Equal(t, a.Price, msg.Price)
	require.Equal(t, 1, msg.Quantity)
	require.Equal(t, result.PreDeductionID, msg.PurchaseSlot)
}

// MQ 发布失败：保留幂等键与 pending_publish 事实，不重复预扣；后台恢复接管。
func TestSeckillPublishFailureKeepsIdemKey(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.pub.err = errors.New("rabbitmq down")

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "publish-failure")
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingPublish, result.Status)
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, result.PreDeductionID)], "发布失败应保留槽位幂等键")
	require.Equal(t, 99, fx.cache.stock[stockKey(a.ID)], "预扣已生效，库存不再变动")
	retryResult, err := fx.svc.Seckill(context.Background(), 7, a.ID, "publish-failure")
	require.NoError(t, err)
	require.Equal(t, result.PreDeductionID, retryResult.PreDeductionID,
		"幂等重试返回原生命周期，不会二次预扣")
}

// T20 幂等操作有限重试：MQ 发布瞬时失败自动重试 + 退避（Attempts 次），
// 重试成功后返回订单号；消息体全程复用同一 order_no（重复投递由消费侧去重）。
func TestSeckillPublishRetriesOnTransientFailure(t *testing.T) {
	fx := newFixtureWithRetry(t, 3)
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.pub.fails = 2 // 前两次失败，第三次成功

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "publish-retry")
	require.NoError(t, err, "瞬时失败应重试成功")
	require.Equal(t, 3, fx.pub.attemptsCount(), "重试次数受限：恰好 Attempts 次")
	var msg SeckillSuccessMessage
	require.NoError(t, json.Unmarshal(fx.pub.body, &msg))
	require.Equal(t, result.OrderNo, msg.OrderNo, "重试复用同一订单号（消息幂等）")
}

// 当前请求内重试耗尽：仍返回已持久接管的预扣事实，后台恢复继续处理。
func TestSeckillPublishRetryExhaustedKeepsIdemKey(t *testing.T) {
	fx := newFixtureWithRetry(t, 3)
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.pub.fails = 99 // 持续失败，重试耗尽

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "publish-exhausted")
	require.NoError(t, err)
	require.Equal(t, 3, fx.pub.attemptsCount(), "重试耗尽即停止，次数受限")
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, result.PreDeductionID)], "重试耗尽仍失败 → 保留槽位幂等键")
	require.Equal(t, model.PreDeductionStatusPendingPublish, result.Status)
}

// newFixtureWithRetry 同 newFixture，但服务启用发布重试（退避极小，测试快速）。
func newFixtureWithRetry(t *testing.T, attempts int) *fixture {
	t.Helper()
	acts := newFakeActivities()
	products := newFakeProducts()
	fc := newFakeCache()
	pub := &fakePublisher{}
	pd := newFakePreDeductions()
	svc := asTestFlashSaleService(New(repository.Store{Activities: acts, PreDeductions: pd, Tx: acts}, products, fc, limiter.RedisCounterConfig{}, pub, &fakeNos{},
		metrics.New().Business(), retry.Config{
			Attempts:       attempts,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     2 * time.Millisecond,
		}))
	return &fixture{svc: svc, acts: acts, preDeductions: pd, products: products, cache: fc, pub: pub}
}

// 订单号生成失败（时钟回拨等）：保留幂等键，同发布失败语义。
func TestSeckillOrderNoFailureKeepsIdemKey(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.svc = asTestFlashSaleService(New(
		repository.Store{Activities: fx.acts, PreDeductions: fx.preDeductions, Tx: fx.acts}, fx.products, fx.cache,
		limiter.RedisCounterConfig{}, fx.pub, &failingNos{}, metrics.New().Business(),
	))

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "order-no-failure")
	require.NoError(t, err)
	require.Empty(t, result.OrderNo)
	require.Equal(t, model.PreDeductionStatusPendingPublish, result.Status)
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, result.PreDeductionID)], "订单号生成失败应保留槽位幂等键")
}

// failingNos 恒失败订单号生成器。
type failingNos struct{}

func (failingNos) Next() (int64, error) { return 0, errors.New("clock rollback") }

// 同一 client_request_id 重试：返回原生命周期，库存不再扣。
func TestSeckillDuplicateReturnsExistingLifecycle(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	first, err := fx.svc.Seckill(context.Background(), 7, a.ID, "request-1")
	require.NoError(t, err)
	second, err := fx.svc.Seckill(context.Background(), 7, a.ID, "request-1")
	require.NoError(t, err)
	require.Equal(t, first.PreDeductionID, second.PreDeductionID)
	require.Equal(t, 99, fx.cache.stock[stockKey(a.ID)], "重复请求不得再次预扣")
	require.Equal(t, 1, fx.cache.count[countKey(a.ID, 7)])

	// 不同活动不互相拦截。
	a2 := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a2.ID))
	require.NoError(t, discardSeckill(fx.svc, context.Background(), 7, a2.ID))
}

func TestSeckillPerUserLimitAllocatesIndependentPurchaseSlots(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) { p.PerUserLimit = 2 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	first, err := fx.svc.Seckill(context.Background(), 7, a.ID, "request-1")
	require.NoError(t, err)
	second, err := fx.svc.Seckill(context.Background(), 7, a.ID, "request-2")
	require.NoError(t, err)
	require.NotEqual(t, first.PreDeductionID, second.PreDeductionID)
	require.Equal(t, 2, fx.cache.count[countKey(a.ID, 7)])
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, first.PreDeductionID)])
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, second.PreDeductionID)])

	_, err = fx.svc.Seckill(context.Background(), 7, a.ID, "request-3")
	require.ErrorIs(t, err, ErrLimitReached)
	require.Equal(t, 98, fx.cache.stock[stockKey(a.ID)])
}

func TestSeckillIdempotencyCacheErrorReturnsLifecycle(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.err = context.DeadlineExceeded

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "cache-error")
	require.NoError(t, err)
	require.NotZero(t, result.PreDeductionID)
	require.Equal(t, model.PreDeductionStatusPreparing, result.Status)
	require.False(t, fx.cache.idem[slotIdemKey(a.ID, 7, result.PreDeductionID)])
	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)], "幂等抢占失败不得预扣")
}

// 按用户限流：窗口内超过 Max 次请求被拒（429 语义），且不触碰幂等键与库存。
func TestSeckillPerUserRateLimited(t *testing.T) {
	fx := newFixtureLimited(1, time.Minute)
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	require.NoError(t, discardSeckill(fx.svc, context.Background(), 7, a.ID))
	require.ErrorIs(t, discardSeckill(fx.svc, context.Background(), 7, a.ID), ErrRateLimited)
	require.Equal(t, 99, fx.cache.stock[stockKey(a.ID)], "限流拒绝不得预扣")
	require.Len(t, fx.cache.idem, 1, "第二次被限流拒绝不得改变首次请求的槽位键")

	// 不同用户互不影响。
	require.NoError(t, discardSeckill(fx.svc, context.Background(), 8, a.ID))
}

func TestSeckillIdempotentRetryBypassesRateLimit(t *testing.T) {
	fx := newFixtureLimited(1, time.Minute)
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	first, err := fx.svc.Seckill(context.Background(), 7, a.ID, "same-request")
	require.NoError(t, err)
	replay, err := fx.svc.Seckill(context.Background(), 7, a.ID, "same-request")
	require.NoError(t, err)
	require.Equal(t, first.PreDeductionID, replay.PreDeductionID)
	_, err = fx.svc.Seckill(context.Background(), 7, a.ID, "new-request")
	require.ErrorIs(t, err, ErrRateLimited)
}

func TestSeckillConcurrentIdempotentRetryConvergesBeforeRateLimit(t *testing.T) {
	fx := newFixtureLimited(1, time.Minute)
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	results := make([]*PurchaseResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = fx.svc.Seckill(context.Background(), 7, a.ID, "concurrent-same-request")
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, results[0].PreDeductionID, results[1].PreDeductionID)
	require.Equal(t, 99, fx.cache.stock[stockKey(a.ID)])
}

// 限流配置关闭（Max<=0）：不限流——同一用户跨活动请求不被限流拦截
// （同活动重复提交仍由幂等键拦截）。
func TestSeckillRateLimitDisabled(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	a2 := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a2.ID))

	first, err := fx.svc.Seckill(context.Background(), 7, a.ID, "rate-disabled")
	require.NoError(t, err)
	require.NoError(t, discardSeckill(fx.svc, context.Background(), 7, a2.ID), "关闭限流后同一用户可继续抢购其他活动")
	retryResult, err := fx.svc.Seckill(context.Background(), 7, a.ID, "rate-disabled")
	require.NoError(t, err)
	require.Equal(t, first.PreDeductionID, retryResult.PreDeductionID)
}

// 预扣业务拒绝：释放幂等键（允许重试）；库存与计数不受影响。
func TestSeckillBusinessRejectReleasesIdemKey(t *testing.T) {
	fx := newFixture()

	// 抢光：第一个用户成功，第二个用户被拒（不同用户，幂等键不同）。
	soldOut := fx.createActivity(t, func(p *ActivityParams) { p.Stock = 1 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), soldOut.ID))
	require.NoError(t, discardSeckill(fx.svc, context.Background(), 1, soldOut.ID))
	require.ErrorIs(t, discardSeckill(fx.svc, context.Background(), 2, soldOut.ID), ErrSoldOut)
	require.Len(t, fx.cache.idem, 1, "抢光拒绝应释放自身槽位键，仅保留首个成功槽位")
	require.Equal(t, 0, fx.cache.stock[stockKey(soldOut.ID)])

	// 未开始：拒绝并释放幂等键；窗口开始后可重抢成功。
	future := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	require.NoError(t, fx.svc.PublishActivity(context.Background(), future.ID))
	require.ErrorIs(t, discardSeckill(fx.svc, context.Background(), 3, future.ID), ErrNotInWindow)
	require.Len(t, fx.cache.idem, 1, "窗口拒绝应释放自身槽位键")
}

// 活动不存在：视为业务拒绝，释放幂等键。
func TestSeckillActivityNotFound(t *testing.T) {
	fx := newFixture()
	require.ErrorIs(t, discardSeckill(fx.svc, context.Background(), 1, 999), ErrActivityNotFound)
	require.Empty(t, fx.cache.idem, "活动不存在不应创建槽位键")
}

// 预扣基础设施结果不确定：返回持久生命周期供查询，避免客户端丢失关联。
func TestSeckillInfraFailureReturnsRecoverableLifecycle(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.deductErr = errors.New("redis down")

	result, err := fx.svc.Seckill(context.Background(), 7, a.ID, "infra-failure")
	require.NoError(t, err)
	require.NotZero(t, result.PreDeductionID)
	require.Equal(t, model.PreDeductionStatusPreparing, result.Status)
	require.True(t, fx.cache.idem[slotIdemKey(a.ID, 7, result.PreDeductionID)], "基础设施失败应保留槽位幂等键")
	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)], "失败不得预扣")
}

// 并发抢购不超卖（经 Seckill 全流程）：40 用户抢 10 库存恰好 10 成功；
// 同一用户重复提交只成功一次（幂等键）。
func TestSeckillConcurrentNoOversell(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, func(p *ActivityParams) { p.Stock = 10 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	var wg sync.WaitGroup
	errs := make([]error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = discardSeckill(fx.svc, context.Background(), int64(i+1), a.ID)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, ErrSoldOut)
		}
	}
	require.Equal(t, 10, ok, "并发抢购不得超卖")
	require.Equal(t, 0, fx.cache.stock[stockKey(a.ID)])

	// 同一用户再次提交（不同活动）：独立幂等键，仍可抢购。
	dup := fx.createActivity(t, func(p *ActivityParams) { p.Stock = 1 })
	require.NoError(t, fx.svc.PublishActivity(context.Background(), dup.ID))
	first, err := fx.svc.Seckill(context.Background(), 1, dup.ID, "concurrent-duplicate")
	require.NoError(t, err)
	retryResult, err := fx.svc.Seckill(context.Background(), 1, dup.ID, "concurrent-duplicate")
	require.NoError(t, err)
	require.Equal(t, first.PreDeductionID, retryResult.PreDeductionID)
}

// ---- 秒杀页活动列表（T23）----

func TestListUserActivitiesFiltersAndState(t *testing.T) {
	fx := newFixture()
	// 进行中（1 分钟前开始）。
	inProgress := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), inProgress.ID))
	// 即将开始（1 小时后开始）。
	notStarted := fx.createActivity(t, func(p *ActivityParams) {
		p.StartAt = time.Now().Add(time.Hour)
		p.EndAt = time.Now().Add(2 * time.Hour)
	})
	require.NoError(t, fx.svc.PublishActivity(context.Background(), notStarted.ID))
	// 已结束：先以有效窗口上架，再把仓储时间改到过去模拟窗口流逝。
	ended := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), ended.ID))
	stored, err := fx.acts.GetByID(context.Background(), ended.ID)
	require.NoError(t, err)
	stored.StartAt, stored.EndAt = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	// 已下架：发布后下架。
	offSale := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), offSale.ID))
	require.NoError(t, fx.svc.UnpublishActivity(context.Background(), offSale.ID))

	views, err := fx.svc.ListUserActivities(context.Background())
	require.NoError(t, err)
	require.Len(t, views, 2, "仅返回已上架且未结束的活动")

	byID := map[int64]model.ActivityView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	require.Equal(t, model.ActivityStateInProgress, byID[inProgress.ID].State)
	require.Equal(t, model.ActivityStateNotStarted, byID[notStarted.ID].State)
	_, hasEnded := byID[ended.ID]
	require.False(t, hasEnded, "已结束活动不返回")
	_, hasOffSale := byID[offSale.ID]
	require.False(t, hasOffSale, "下架活动不返回")
}

func TestListUserActivitiesRemainingStockAndSummary(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	// 模拟预扣：Redis 余量 37（配置库存 100）。
	fx.cache.Set(context.Background(), stockKey(a.ID), "37", 0)
	// 预扣部分用户计数，验证摘要拼接不依赖计数。

	views, err := fx.svc.ListUserActivities(context.Background())
	require.NoError(t, err)
	require.Len(t, views, 1)
	v := views[0]
	require.Equal(t, 37, v.Stock, "剩余库存以 Redis 预扣余量为准")
	require.Equal(t, "秒杀商品", v.ProductTitle)
	require.Equal(t, int64(1), v.SKU.ProductID)
	require.Equal(t, int64(100), v.SKU.Price, "SKU 原价")
	require.NotEmpty(t, v.SKU.Specs)

	// Redis 缺失（如预热 TTL 过期）时降级配置库存。
	fx.cache.Del(context.Background(), stockKey(a.ID))
	views, err = fx.svc.ListUserActivities(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, views[0].Stock, "缓存缺失降级 DB 配置库存")
}

func TestListUserActivitiesSkipsBrokenSummary(t *testing.T) {
	fx := newFixture()
	// SKU 不存在的活动（如 SKU 被删除）：活动仍返回，摘要为空。
	a := fx.createActivity(t, nil)
	delete(fx.products.skus, 1)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))

	views, err := fx.svc.ListUserActivities(context.Background())
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, "", views[0].ProductTitle)
	require.Zero(t, views[0].SKU.ID)
	require.Equal(t, 100, views[0].Stock, "摘要失败不影响活动与库存展示")
}
