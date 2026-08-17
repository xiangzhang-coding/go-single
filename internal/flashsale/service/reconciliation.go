// 秒杀库存对账（T13）：定时比对 Redis 活动库存 vs MySQL 活动库存 vs
// 秒杀有效订单数，分两档——
//   - 进行中对账（cron 每小时）：只比对告警、不自动回写（Redis 预扣领先属
//     正常），识别"Redis 有扣减但无对应订单"作为补单信号输出告警；
//   - 收尾对账（cron 扫描刚过 end_at 的活动）：以 MySQL 为准对齐 Redis 库存。
package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
)

// endedReconcileWindow 收尾对账回建窗口：仅在此窗口内回建缺失的库存 key。
// Redis 库存 key TTL = 结束 + 1h（remainingTTL），窗口内必未过期；
// key 缺失（回建）场景仅此窗口内有效，超窗 key 已自清理不回建。
const endedReconcileWindow = 30 * time.Minute

// ReconcileWarning 单活动对账差异（进行中对账告警内容，cron 回调转 zap 结构化日志）。
type ReconcileWarning struct {
	PreDeductionID int64
	UserID         int64
	OrderNo        string
	Status         string
	ActivityID     int64
	Title          string
	RedisStock     int // Redis 活动库存（预扣为准）
	MySQLStock     int // flashsale.stock（落单事实源）
	OrderCount     int // 秒杀有效订单数（非取消，解释差额的上下文）
	Detail         string
}

// SeckillOrderCounter 秒杀有效订单数统计端口（order 服务实现，进程内调用；
// flashsale → order 单向依赖，与消费者同方向）。
type SeckillOrderCounter interface {
	CountValidSeckill(ctx context.Context, activityID int64) (int, error)
}

// Reconciliation 秒杀库存对账服务。
type Reconciliation interface {
	// ReconcileActive 进行中对账（cron 每小时）：比对进行中活动的 Redis 库存
	// vs MySQL 库存 vs 秒杀有效订单数。只比对告警、不自动回写——Redis 预扣
	// 领先属正常；redis < mysql 识别为"Redis 有扣减但无对应订单"补单信号
	// （MQ 发布失败/落单死信/取消未回补 Redis）。返回告警列表供 cron 落日志。
	ReconcileActive(ctx context.Context) ([]ReconcileWarning, error)
	// ReconcileEnded 收尾对账（cron 扫描刚过 end_at 的上架活动）：以 MySQL
	// 库存为准 SET Redis 库存（含差异时按 key 剩余 TTL 对齐一次），返回对齐数。
	ReconcileEnded(ctx context.Context) (aligned int, err error)
}

type stockCache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

type reconciliationService struct {
	store   repository.Store
	cache   stockCache
	counter SeckillOrderCounter
}

// NewReconciliation 构造秒杀库存对账服务。
func NewReconciliation(store repository.Store, c stockCache, counter SeckillOrderCounter) Reconciliation {
	return &reconciliationService{store: store, cache: c, counter: counter}
}

// ReconcileActive 进行中对账（只读）：遍历进行中活动，逐活动比对
// Redis 库存 vs MySQL 库存；差异输出告警（补单信号/多扣），不写回。
func (s *reconciliationService) ReconcileActive(ctx context.Context) ([]ReconcileWarning, error) {
	activities, err := s.store.Activities.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var warnings []ReconcileWarning
	active := make(map[int64]*model.Activity)
	for i := range activities {
		if activities[i].InProgress(now) {
			active[activities[i].ID] = &activities[i]
		}
	}
	if s.store.PreDeductions != nil {
		facts, err := s.store.PreDeductions.ListRecoverable(ctx, 0)
		if err != nil {
			return nil, err
		}
		for i := range facts {
			fact := &facts[i]
			a := active[fact.ActivityID]
			if a == nil {
				continue
			}
			warnings = append(warnings, ReconcileWarning{
				PreDeductionID: fact.ID,
				UserID:         fact.UserID,
				OrderNo:        fact.OrderNumber(),
				Status:         string(fact.Status),
				ActivityID:     fact.ActivityID,
				Title:          a.Title,
				Detail:         recoveryDetail(fact.Status),
			})
		}
	}
	for i := range activities {
		a := &activities[i]
		if !a.InProgress(now) {
			continue
		}
		w, err := s.diffActive(ctx, a)
		if err != nil {
			return nil, err
		}
		if w != nil {
			warnings = append(warnings, *w)
		}
	}
	return warnings, nil
}

func recoveryDetail(status model.PreDeductionStatus) string {
	switch status {
	case model.PreDeductionStatusPreparing:
		return "核验 Redis 预扣事实后继续发布或完整回退"
	case model.PreDeductionStatusPendingPublish:
		return "持久预扣待继续发布"
	case model.PreDeductionStatusPendingOrder:
		return "消息待落单，恢复任务将用同一订单号重投"
	case model.PreDeductionStatusPendingRollback:
		return "持久预扣待完整回退"
	default:
		return "未知预扣恢复状态"
	}
}

// diffActive 单活动差异判定（只读比对，进行中绝不写回）。
// 三方比对（Redis 库存 vs MySQL 库存 vs 有效订单数）在 MySQL 侧由事务耦合
// 化简为两方：落单同事务扣 MySQL 库存、取消同事务回补，故
// mysql_stock ≡ 初始库存 − 有效订单数恒成立（订单数仅作告警上下文解释差额）。
//   - redis < mysql：预扣成功但未落单（MQ 发布失败/死信）或取消未回补 Redis
//     → 补单/回补信号；
//   - redis > mysql：多回补/缺预扣（异常）；
//   - redis key 缺失：上架进行中却无预热库存（异常）。
func (s *reconciliationService) diffActive(ctx context.Context, a *model.Activity) (*ReconcileWarning, error) {
	orders, err := s.counter.CountValidSeckill(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	raw, err := s.cache.Get(ctx, stockKey(a.ID))
	if err != nil {
		if errors.Is(err, cache.ErrMiss) {
			return &ReconcileWarning{
				ActivityID: a.ID, Title: a.Title,
				MySQLStock: a.Stock, OrderCount: orders,
				Detail: "进行中活动 Redis 预热库存缺失（上架异常）",
			}, nil
		}
		return nil, err
	}
	redisStock, err := strconv.Atoi(raw)
	if err != nil {
		return nil, err
	}
	w := &ReconcileWarning{
		ActivityID: a.ID, Title: a.Title,
		RedisStock: redisStock, MySQLStock: a.Stock, OrderCount: orders,
	}
	switch {
	case redisStock < a.Stock:
		// 差额 = 预扣成功但无对应订单（补单信号）或取消未回补 Redis。
		w.Detail = "Redis 有扣减但无对应订单（补单/回补信号，差额 " +
			strconv.Itoa(a.Stock-redisStock) + "）"
	case redisStock > a.Stock:
		w.Detail = "Redis 库存高于 MySQL（多回补或缺预扣，差额 " +
			strconv.Itoa(redisStock-a.Stock) + "）"
	default:
		return nil, nil // 一致
	}
	return w, nil
}

// ReconcileEnded 收尾对账：遍历已结束（end_at ≤ now）的上架活动，Redis 库存与
// MySQL 不一致时以 MySQL 为准 SET 对齐（TTL 沿用 结束 + 1h 自清理）；
// 下架活动不回建（下架已清除预热库存）。
// 对齐不设结束时间上限——只要库存 key 仍存活（TTL = 结束 + 1h 内），任何漂移
// （含超时取消回补 Redis 失败等窗口外情形）都会被收敛；key 缺失仅在收尾窗口
// （endedReconcileWindow）内回建（该窗口内 key 理应存活，缺失即预热丢失异常）。
// 返回实际发生对齐的数量（差异即告警信号，由 cron 记录日志）。
func (s *reconciliationService) ReconcileEnded(ctx context.Context) (int, error) {
	activities, err := s.store.Activities.List(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	aligned := 0
	for i := range activities {
		a := &activities[i]
		if !a.IsOnSale() || now.Before(a.EndAt) {
			continue
		}
		if s.store.PreDeductions != nil {
			unresolved, err := s.store.PreDeductions.HasUnresolved(ctx, a.ID)
			if err != nil {
				return aligned, err
			}
			if unresolved {
				continue
			}
		}
		raw, err := s.cache.Get(ctx, stockKey(a.ID))
		if err != nil && !errors.Is(err, cache.ErrMiss) {
			return aligned, err
		}
		if errors.Is(err, cache.ErrMiss) {
			if now.After(a.EndAt.Add(endedReconcileWindow)) {
				continue // key 生命周期已结束，不回建
			}
			// 窗口内 key 理应存活（TTL = 结束 + 1h）：缺失即上架预热丢失，回建对齐。
			if err := s.cache.Set(ctx, stockKey(a.ID), strconv.Itoa(a.Stock), remainingTTL(a)); err != nil {
				return aligned, err
			}
			aligned++
			continue
		}
		if raw == strconv.Itoa(a.Stock) {
			continue // 已一致
		}
		if err := s.cache.Set(ctx, stockKey(a.ID), strconv.Itoa(a.Stock), remainingTTL(a)); err != nil {
			return aligned, err
		}
		aligned++
	}
	return aligned, nil
}
