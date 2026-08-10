// service 层单元测试（中间 seam）：fake 活动仓储 + fake product 服务 + fake 缓存
// （Lua 语义在 Go 内以互斥锁模拟原子执行），覆盖活动校验、SKU 校验、
// 上架预热（未开始覆盖/进行中只减不增）、下架清除与预扣拒绝、预扣各失败分支。
package service

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
)

// ---- fake 活动仓储 ----

type fakeActivities struct {
	byID  map[int64]*model.Activity
	order int64
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

func (f *fakeActivities) GetByID(_ context.Context, id int64) (*model.Activity, error) {
	return f.byID[id], nil
}

func (f *fakeActivities) List(context.Context) ([]model.Activity, error) {
	out := make([]model.Activity, 0, len(f.byID))
	for _, v := range f.byID {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (f *fakeActivities) UpdateStatus(_ context.Context, id int64, status string) error {
	if v, ok := f.byID[id]; ok {
		v.Status = status
	}
	return nil
}

// ---- fake product 服务 ----

type fakeProducts struct {
	skus map[int64]*productmodel.SKU
}

func newFakeProducts() *fakeProducts {
	return &fakeProducts{skus: map[int64]*productmodel.SKU{}}
}

func (f *fakeProducts) seed(skuID int64) {
	f.skus[skuID] = &productmodel.SKU{ID: skuID, ProductID: 1, Price: 100, Stock: 5}
}

func (f *fakeProducts) GetSKU(_ context.Context, id int64) (*productmodel.SKU, error) {
	if s, ok := f.skus[id]; ok {
		return s, nil
	}
	return nil, productsvc.ErrSKUNotFound
}

// ---- fake 缓存（互斥锁模拟 Lua 原子预扣）----

type fakeCache struct {
	mu    sync.Mutex
	stock map[string]int // flashsale:stock:{id} → 余量
	count map[string]int // flashsale:count:{id}:{user} → 已购数
	err   error
}

func newFakeCache() *fakeCache {
	return &fakeCache{stock: map[string]int{}, count: map[string]int{}}
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
	if !ok {
		return "", cache.ErrMiss
	}
	return intToStr(v), nil
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

func (f *fakeCache) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.stock, key)
	return nil
}

// Eval 按 ARGV 数量区分脚本并镜像其判定顺序（加锁模拟单线程原子执行）：
//   - 2 参 = prewarmScript：key 缺失写入；配置库存更低才覆盖（只减不增）；
//   - 5 参 = preDeductScript：status → 时间窗口 → 库存 → 每人限购 → DECR + INCR。
func (f *fakeCache) Eval(_ context.Context, _ string, keys []string, args ...any) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if len(args) == 2 {
		stock := args[0].(int)
		if cur, ok := f.stock[keys[0]]; !ok || stock < cur {
			f.stock[keys[0]] = stock
			return 1, nil
		}
		return 0, nil
	}
	now, startAt, endAt := args[0].(int64), args[1].(int64), args[2].(int64)
	status, perUserLimit := args[3].(string), args[4].(int)
	if status != model.ActivityStatusOnSale {
		return preDeductOffline, nil
	}
	if now < startAt || now > endAt {
		return preDeductNotInWindow, nil
	}
	if f.stock[keys[0]] <= 0 {
		return preDeductSoldOut, nil
	}
	if f.count[keys[1]] >= perUserLimit {
		return preDeductLimitReach, nil
	}
	f.stock[keys[0]]--
	f.count[keys[1]]++
	return preDeductOK, nil
}

// ---- 辅助 ----

func intToStr(v int) string { return strconv.Itoa(v) }
func strToInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ---- 测试夹具 ----

type fixture struct {
	svc      Service
	acts     *fakeActivities
	products *fakeProducts
	cache    *fakeCache
}

func newFixture() *fixture {
	acts := newFakeActivities()
	products := newFakeProducts()
	fc := newFakeCache()
	svc := New(repository.Store{Activities: acts}, products, fc)
	return &fixture{svc: svc, acts: acts, products: products, cache: fc}
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

// 进行中编辑库存调低：DB 与 Redis 同步降低。
func TestUpdateInProgressDecreases(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.stock[stockKey(a.ID)] = 50

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 30,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	require.NoError(t, fx.svc.UpdateActivity(context.Background(), a.ID, p))

	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, 30, updated.Stock)
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
	require.ErrorIs(t, fx.svc.UpdateActivity(context.Background(), a.ID, p), ErrStockIncreaseInProgress)

	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, 100, updated.Stock, "DB 不应被调高")
	require.Equal(t, 100, fx.cache.stock[stockKey(a.ID)])
}

// 进行中编辑库存不低于 Redis 存量：保持存量（只减不增）。
func TestUpdateInProgressBetweenKeepsRedis(t *testing.T) {
	fx := newFixture()
	a := fx.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	fx.cache.stock[stockKey(a.ID)] = 50

	p := ActivityParams{
		SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: 80,
		PerUserLimit: a.PerUserLimit, StartAt: a.StartAt, EndAt: a.EndAt,
	}
	// 80 < DB 100 但 > Redis 50：DB 调低、Redis 保持。
	require.NoError(t, fx.svc.UpdateActivity(context.Background(), a.ID, p))
	require.Equal(t, 50, fx.cache.stock[stockKey(a.ID)])
	updated, _ := fx.acts.GetByID(context.Background(), a.ID)
	require.Equal(t, 80, updated.Stock)
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

// 并发预扣不超卖：互斥锁模拟 Lua 原子性，20 用户抢 5 库存恰好 5 成功。
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
	a2 := fx.createActivity(t, func(p *ActivityParams) { p.Title = "第二场" })

	list, err := fx.svc.ListActivities(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, a2.ID, list[0].ID)
	require.Equal(t, a1.ID, list[1].ID)
	require.Equal(t, model.ActivityStatusOffSale, list[0].Status)
}
