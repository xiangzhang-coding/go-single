// T13 秒杀 Redis 回补与对账单元测试（中间 seam）：fake 活动仓储 + fake 缓存 +
// fake 订单计数端口，覆盖——
//   - RestoreRedis：缓存原子回补 Redis 库存/用户计数/幂等键
//     （key 缺失不重建、回补失败透传）；
//   - ReconcileActive：进行中只比对告警不写回，redis < mysql 识别补单信号，
//     库存 key 缺失告警，一致无告警；
//   - ReconcileEnded：以 MySQL 为准对齐刚结束活动的 Redis 库存，跳过下架/
//     未结束/超窗/已一致。
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
)

// ---- 对账 ----

// fakeCounter 秒杀有效订单数端口替身。
type fakeCounter struct {
	counts map[int64]int
	err    error
}

func newFakeCounter() *fakeCounter { return &fakeCounter{counts: map[int64]int{}} }

func (f *fakeCounter) seed(activityID int64, n int) { f.counts[activityID] = n }

func (f *fakeCounter) CountValidSeckill(_ context.Context, activityID int64) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.counts[activityID], nil
}

// reconcileFixture 对账测试夹具：活动仓储 + 缓存 + 计数端口 + 对账服务。
type reconcileFixture struct {
	acts  *fakeActivities
	pd    *fakePreDeductions
	cache *fakeCache
	cnt   *fakeCounter
	svc   Reconciliation
}

func newReconcileFixture() *reconcileFixture {
	acts := newFakeActivities()
	fc := newFakeCache()
	cnt := newFakeCounter()
	pd := newFakePreDeductions()
	return &reconcileFixture{
		acts:  acts,
		pd:    pd,
		cache: fc,
		cnt:   cnt,
		svc:   NewReconciliation(repository.Store{Activities: acts, PreDeductions: pd}, fc, cnt),
	}
}

// seedActive 种一个进行中活动（已上架，窗口覆盖 now）并预热 Redis 库存。
func (fx *reconcileFixture) seedActive(t *testing.T, stock int) *model.Activity {
	t.Helper()
	a := &model.Activity{
		ID: 1, SKUID: 1, Title: "限时秒杀", Price: 9900,
		Stock: stock, PerUserLimit: 1, Status: model.ActivityStatusOnSale,
		StartAt: time.Now().Add(-time.Minute), EndAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, fx.acts.Create(context.Background(), a))
	fx.cache.stock[stockKey(a.ID)] = stock
	return a
}

// 进行中对账：Redis 与 MySQL 一致（落单已同步扣减）→ 无告警，且不写回。
func TestReconcileActiveInSyncNoWarning(t *testing.T) {
	fx := newReconcileFixture()
	a := fx.seedActive(t, 10)
	// 模拟 3 单落单：MySQL 同事务扣 3（落单事实源）、Redis 预扣扣 3。
	fx.acts.DeductStock(context.Background(), nil, a.ID, 3)
	fx.cache.stock[stockKey(a.ID)] = 7
	fx.cnt.seed(a.ID, 3)

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Equal(t, 7, fx.cache.stock[stockKey(a.ID)], "进行中不得写回 Redis")
}

// 进行中对账："Redis 有扣减但无对应订单"（MQ 发布失败/落单死信/取消未回补）
// → 补单信号告警；Redis 不被覆盖（预扣领先属正常，绝不写回）。
func TestReconcileActiveDeductWithoutOrderWarns(t *testing.T) {
	fx := newReconcileFixture()
	a := fx.seedActive(t, 10)
	fx.cnt.seed(a.ID, 2)
	fx.cache.stock[stockKey(a.ID)] = 6 // 预扣 4 但仅 2 单

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	w := warnings[0]
	require.Equal(t, a.ID, w.ActivityID)
	require.Equal(t, 6, w.RedisStock)
	require.Equal(t, 10, w.MySQLStock)
	require.Equal(t, 2, w.OrderCount)
	require.Contains(t, w.Detail, "补单")
	require.Equal(t, 6, fx.cache.stock[stockKey(a.ID)], "进行中只比对告警，不自动回写")
}

func TestReconcileActiveIdentifiesUnresolvedPreDeduction(t *testing.T) {
	fx := newReconcileFixture()
	a := fx.seedActive(t, 10)
	orderNo := "S100"
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: a.ID, OrderNo: &orderNo, Quantity: 1,
		Status: model.PreDeductionStatusPendingPublish,
	}
	require.NoError(t, fx.pd.Create(context.Background(), pd))
	fx.cache.stock[stockKey(a.ID)] = 9

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, warnings)

	found := false
	for _, warning := range warnings {
		if warning.PreDeductionID == pd.ID {
			found = true
			require.Equal(t, int64(42), warning.UserID)
			require.Equal(t, orderNo, warning.OrderNo)
			require.Equal(t, string(model.PreDeductionStatusPendingPublish), warning.Status)
			require.Contains(t, warning.Detail, "继续发布")
		}
	}
	require.True(t, found, "reconciliation must identify the exact recoverable pre-deduction")
}

func TestReconcileActiveIdentifiesOrderedFactWithMissingReservation(t *testing.T) {
	fx := newReconcileFixture()
	a := fx.seedActive(t, 9)
	orderNo := "ordered-aof"
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: a.ID, OrderNo: &orderNo, Quantity: 1,
		Status: model.PreDeductionStatusOrdered,
	}
	require.NoError(t, fx.pd.Create(context.Background(), pd))

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	var found bool
	for _, warning := range warnings {
		if warning.PreDeductionID == pd.ID {
			found = true
			require.Equal(t, "ordered", warning.Status)
			require.Contains(t, warning.Detail, "reservation marker 缺失")
		}
	}
	require.True(t, found)
}

// 进行中对账：Redis 高于 MySQL（多回补/缺预扣）→ 告警，不写回。
func TestReconcileActiveRedisAboveMySQLWarns(t *testing.T) {
	fx := newReconcileFixture()
	a := fx.seedActive(t, 10)
	fx.cache.stock[stockKey(a.ID)] = 12

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0].Detail, "高于")
}

// 进行中对账：上架进行中但预热库存 key 缺失 → 告警（上架异常）。
func TestReconcileActiveMissingStockKeyWarns(t *testing.T) {
	fx := newReconcileFixture()
	a := fx.seedActive(t, 10)
	delete(fx.cache.stock, stockKey(a.ID))

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0].Detail, "缺失")
}

// 进行中对账：跳过未上架/未开始/已结束活动（仅比对进行中）。
func TestReconcileActiveSkipsNonInProgress(t *testing.T) {
	fx := newReconcileFixture()
	base := func(id int64, status string, start, end time.Time, stock int) {
		a := &model.Activity{ID: id, SKUID: 1, Title: "活动", Price: 100,
			Stock: stock, PerUserLimit: 1, Status: status, StartAt: start, EndAt: end}
		require.NoError(t, fx.acts.Create(context.Background(), a))
	}
	now := time.Now()
	base(1, model.ActivityStatusOffSale, now.Add(-time.Minute), now.Add(time.Hour), 5) // 下架
	base(2, model.ActivityStatusOnSale, now.Add(time.Minute), now.Add(time.Hour), 5)   // 未开始
	base(3, model.ActivityStatusOnSale, now.Add(-2*time.Hour), now.Add(-time.Hour), 5) // 已结束
	// 这些活动的 Redis key 与 MySQL 均不一致，但不应产生告警。
	fx.cache.stock[stockKey(1)] = 0
	fx.cache.stock[stockKey(2)] = 0
	fx.cache.stock[stockKey(3)] = 0

	warnings, err := fx.svc.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.Empty(t, warnings)
}

// 进行中对账：计数端口失败透传（下个 tick 重试）。
func TestReconcileActivePropagatesCounterError(t *testing.T) {
	fx := newReconcileFixture()
	fx.seedActive(t, 10)
	fx.cnt.err = errors.New("mysql down")

	_, err := fx.svc.ReconcileActive(context.Background())
	require.ErrorContains(t, err, "mysql down")
}

// 收尾对账：刚结束活动的 Redis 库存与 MySQL 不一致 → 以 MySQL 为准 SET 对齐；
// 返回对齐数（差异即告警信号）。
func TestReconcileEndedAlignsToMySQL(t *testing.T) {
	fx := newReconcileFixture()
	a := &model.Activity{
		ID: 1, SKUID: 1, Title: "已结束", Price: 100, Stock: 7,
		PerUserLimit: 1, Status: model.ActivityStatusOnSale,
		StartAt: time.Now().Add(-2 * time.Hour), EndAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, fx.acts.Create(context.Background(), a))
	fx.cache.stock[stockKey(a.ID)] = 3 // Redis 残留扣减

	aligned, err := fx.svc.ReconcileEnded(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, aligned)
	require.Equal(t, a.Stock, fx.cache.stock[stockKey(a.ID)], "以 MySQL 为准对齐 Redis")
}

func TestReconcileEndedSkipsActivityWithUnresolvedLifecycle(t *testing.T) {
	fx := newReconcileFixture()
	a := &model.Activity{
		ID: 1, SKUID: 1, Title: "已结束", Price: 100, Stock: 7,
		PerUserLimit: 1, Status: model.ActivityStatusOnSale,
		StartAt: time.Now().Add(-2 * time.Hour), EndAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, fx.acts.Create(context.Background(), a))
	fx.cache.stock[stockKey(a.ID)] = 3
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: a.ID, Quantity: 1,
		Status: model.PreDeductionStatusPendingRollback,
	}
	require.NoError(t, fx.pd.Create(context.Background(), pd))

	aligned, err := fx.svc.ReconcileEnded(context.Background())
	require.NoError(t, err)
	require.Zero(t, aligned)
	require.Equal(t, 3, fx.cache.stock[stockKey(a.ID)],
		"aggregate alignment must wait for exact lifecycle compensation")
}

// 收尾对账：已一致不重复对齐（对齐数 0）。
func TestReconcileEndedSkipsInSync(t *testing.T) {
	fx := newReconcileFixture()
	a := &model.Activity{
		ID: 1, SKUID: 1, Title: "已结束", Price: 100, Stock: 7,
		PerUserLimit: 1, Status: model.ActivityStatusOnSale,
		StartAt: time.Now().Add(-2 * time.Hour), EndAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, fx.acts.Create(context.Background(), a))
	fx.cache.stock[stockKey(a.ID)] = 7

	aligned, err := fx.svc.ReconcileEnded(context.Background())
	require.NoError(t, err)
	require.Zero(t, aligned)
}

// 收尾对账：仅处理已结束的上架活动——
// 下架/未结束跳过；key 存活期间任何结束时长都可对齐（窗口外漂移也收敛）；
// key 缺失仅在收尾窗口内回建，窗口外不回建（生命周期已结束）。
func TestReconcileEndedScopeFilter(t *testing.T) {
	fx := newReconcileFixture()
	now := time.Now()
	seed := func(id int64, status string, end time.Time, stock int) {
		a := &model.Activity{ID: id, SKUID: 1, Title: "a", Price: 100, Stock: stock,
			PerUserLimit: 1, Status: status, StartAt: now.Add(-2 * time.Hour), EndAt: end}
		require.NoError(t, fx.acts.Create(context.Background(), a))
		fx.cache.stock[stockKey(id)] = stock - 1 // 均与 MySQL 不一致
	}
	seed(1, model.ActivityStatusOffSale, now.Add(-time.Minute), 5)           // 下架
	seed(2, model.ActivityStatusOnSale, now.Add(time.Hour), 5)               // 未结束
	seed(3, model.ActivityStatusOnSale, now.Add(-2*endedReconcileWindow), 5) // 结束超窗但 key 存活 → 仍对齐
	seed(4, model.ActivityStatusOnSale, now.Add(-time.Minute), 5)            // 窗口内 → 对齐
	seed(5, model.ActivityStatusOnSale, now.Add(-endedReconcileWindow/2), 5) // 窗口内 → 对齐

	aligned, err := fx.svc.ReconcileEnded(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, aligned, "仅已结束且 key 存活的上架活动对齐（3、4、5）")
	require.Equal(t, 5, fx.cache.stock[stockKey(3)], "超窗但 key 存活仍应对齐（兜底窗口外漂移）")
	require.Equal(t, 5, fx.cache.stock[stockKey(4)], "活动 4 对齐")
	require.Equal(t, 5, fx.cache.stock[stockKey(5)], "活动 5 对齐")
	require.Equal(t, 4, fx.cache.stock[stockKey(1)], "下架不回建")
	require.Equal(t, 4, fx.cache.stock[stockKey(2)], "未结束不对齐")
}

// 收尾对账：key 缺失仅在收尾窗口内回建；窗口外不回建（生命周期已结束）。
func TestReconcileEndedKeyResurrectionWindow(t *testing.T) {
	fx := newReconcileFixture()
	now := time.Now()
	seed := func(id int64, end time.Time, stock int) {
		a := &model.Activity{ID: id, SKUID: 1, Title: "a", Price: 100, Stock: stock,
			PerUserLimit: 1, Status: model.ActivityStatusOnSale,
			StartAt: now.Add(-2 * time.Hour), EndAt: end}
		require.NoError(t, fx.acts.Create(context.Background(), a))
		// 不种 Redis key：缺失场景
	}
	seed(1, now.Add(-time.Minute), 5)            // 窗口内缺失 → 回建
	seed(2, now.Add(-2*endedReconcileWindow), 5) // 超窗缺失 → 不回建

	aligned, err := fx.svc.ReconcileEnded(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, aligned)
	require.Equal(t, 5, fx.cache.stock[stockKey(1)], "窗口内 key 缺失应回建对齐")
	_, ok := fx.cache.stock[stockKey(2)]
	require.False(t, ok, "超窗 key 缺失不回建")
}

// 收尾对账：Redis 读取/写入失败透传（下个 tick 重试）。
func TestReconcileEndedPropagatesError(t *testing.T) {
	fx := newReconcileFixture()
	a := &model.Activity{
		ID: 1, SKUID: 1, Title: "已结束", Price: 100, Stock: 7,
		PerUserLimit: 1, Status: model.ActivityStatusOnSale,
		StartAt: time.Now().Add(-2 * time.Hour), EndAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, fx.acts.Create(context.Background(), a))
	fx.cache.stock[stockKey(a.ID)] = 3
	fx.cache.err = errors.New("redis down")

	_, err := fx.svc.ReconcileEnded(context.Background())
	require.ErrorContains(t, err, "redis down")
}

var _ SeckillOrderCounter = (*fakeCounter)(nil)
