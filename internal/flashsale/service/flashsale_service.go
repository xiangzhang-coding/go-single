// Package service 承载 flashsale 模块业务：admin 创建/编辑/上架/下架秒杀活动，
// 上架时预热独立库存进 Redis（未开始可覆盖、进行中只减不增），
// 并提供原子预扣与抢购接口（限流 → 幂等键 → 预扣，成功返回排队中）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrActivityNotFound        = errors.New("flashsale activity not found")
	ErrInvalidInput            = errors.New("invalid input")
	ErrStockIncreaseInProgress = errors.New("stock can only decrease while activity is in progress")
	ErrNotInWindow             = errors.New("flashsale not in time window")
	ErrSoldOut                 = errors.New("flashsale sold out")
	ErrLimitReached            = errors.New("per user limit reached")
	ErrOffline                 = errors.New("flashsale activity offline")
	ErrDuplicateRequest        = errors.New("duplicate flashsale request")
	ErrRateLimited             = errors.New("flashsale rate limited")
)

// Redis key 约定（DESIGN.md）：flashsale:stock:{id}（活动库存，上架预热）/
// flashsale:count:{id}:{user}（用户抢购计数，原子预扣 INCR）/
// flashsale:idem:{id}:{user}（幂等键，挡预扣请求重复提交）/
// flashsale:rl:{user}（按用户限流计数）。
func stockKey(id int64) string { return fmt.Sprintf("flashsale:stock:%d", id) }
func countKey(id, userID int64) string {
	return fmt.Sprintf("flashsale:count:%d:%d", id, userID)
}
func idemKey(activityID, userID int64) string {
	return fmt.Sprintf("flashsale:idem:%d:%d", activityID, userID)
}
func rlKey(userID int64) string { return fmt.Sprintf("flashsale:rl:%d", userID) }

// SeckillOrderQueue 秒杀异步落单队列：预扣成功后发布"抢购成功"消息，
// 消费者落单（唯一约束幂等）+ 同事务扣活动库存；失败重投/死信（平台 mq 层）。
const SeckillOrderQueue = "flashsale.order.create"

// SeckillSuccessMessage 抢购成功消息体：携带预扣时生成的订单号，
// 前端据此轮询 GET /api/orders/{order_no} 得知异步落单结果。
type SeckillSuccessMessage struct {
	OrderNo    string `json:"order_no"`
	UserID     int64  `json:"user_id"`
	ActivityID int64  `json:"activity_id"`
}

// stockKeyMargin 预热库存 TTL 的余量：剩余时长 + 1h，活动结束后自清理。
const stockKeyMargin = time.Hour

// idemTTL 幂等键 TTL：与规格一致（30min，DESIGN.md），仅挡预扣请求的重复提交。
const idemTTL = 30 * time.Minute

// ActivityParams 活动参数（创建/编辑共用；状态经 上架/下架 端点变更）。
type ActivityParams struct {
	SKUID        int64
	Title        string
	Price        int64
	Stock        int
	PerUserLimit int
	StartAt      time.Time
	EndAt        time.Time
}

// Service flashsale 模块的业务接口。
type Service interface {
	// ---- admin ----
	CreateActivity(ctx context.Context, p ActivityParams) (*model.Activity, error)
	UpdateActivity(ctx context.Context, id int64, p ActivityParams) error
	// ListActivities 后台活动列表（T25 富化）：全状态（含下架/已结束），
	// 每项携带派生状态（off_sale/not_started/in_progress/ended）与 SKU/商品摘要。
	ListActivities(ctx context.Context) ([]model.ActivityView, error)
	// PublishActivity 上架：预热库存进 Redis（未开始可覆盖 DEL+SET，进行中原子只减不增）。
	PublishActivity(ctx context.Context, id int64) error
	// UnpublishActivity 下架：清除预热库存，后续抢购被拒。
	UnpublishActivity(ctx context.Context, id int64) error

	// ---- 秒杀页（T23）----
	// ListUserActivities 秒杀页活动列表：仅返回已上架且未结束的活动
	// （进行中 + 即将开始），每项携带剩余库存（Redis 预扣余量，缺失降级配置库存）、
	// 派生状态（not_started/in_progress）与 SKU/商品摘要；状态按服务端时间派生，
	// 倒计时对齐由调用方在响应中附加 server_time。
	ListUserActivities(ctx context.Context) ([]model.ActivityView, error)

	// ---- 抢购（T11/T12）----
	// Seckill 抢购全流程：按用户限流（Redis 固定窗口计数）→ 幂等键
	// （用户+活动，TTL 30min，挡预扣请求重复提交）→ 缓存原子预扣 →
	// 生成雪花订单号 → 发 MQ"抢购成功"消息（异步落单）。
	// 预扣被业务拒绝（抢光/限购/窗口外/下架）时释放幂等键允许重试，
	// 基础设施失败保留幂等键（防瞬时故障下重复预扣）；成功返回订单号
	// （前端据此轮询订单接口），MQ 发布失败同样保留幂等键（对账兜底）。
	Seckill(ctx context.Context, userID, activityID int64) (orderNo string, err error)

	// ---- 抢购预扣（底层原子操作，T11 抢购接口复用）----
	// PreDeduct 缓存原子预扣：活动由调用方读库（与优惠券领券同模式，
	// 状态/窗口/限购作为 ARGV 传入，Redis 内原子扣减）。
	PreDeduct(ctx context.Context, userID, activityID int64) error

	// ---- 取消回补（T13，flashsale 应用编排调用）----
	// RestoreRedis 在订单取消与 MySQL 活动库存回补事务提交后，回补 Redis
	// 库存 + 用户计数并释放幂等键（允许再次抢购）。
	RestoreRedis(ctx context.Context, activityID, userID int64, quantity int) error
}

// ProductService product 模块暴露的最小查询接口（跨模块进程内调用，面向接口非 HTTP；
// productSvc 天然满足，未来拆模块时换实现即可）。
type ProductService interface {
	// GetSKU 校验 SKU 存在。
	GetSKU(ctx context.Context, id int64) (*productmodel.SKU, error)
	// GetProduct 读取 SPU（秒杀页展示商品标题）。
	GetProduct(ctx context.Context, id int64) (*productmodel.Product, error)
}

// MessagePublisher MQ 发布端口（platform/mq 实现）：秒杀预扣成功后
// 发布"抢购成功"消息驱动异步落单；发布确认确保不丢（失败保留幂等键 + 对账兜底）。
type MessagePublisher interface {
	Publish(ctx context.Context, queue string, body []byte) error
}

// OrderNoGenerator 秒杀订单号生成器（雪花 ID，与 order 模块共用同一实例）。
type OrderNoGenerator interface {
	Next() (int64, error)
}

type flashSaleCache interface {
	cache.IdempotencyStore
	cache.FlashSaleStore
	cache.FixedWindowStore
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

type flashsaleService struct {
	store     repository.Store
	products  ProductService
	cache     flashSaleCache
	perUser   *limiter.RedisCounter
	publisher MessagePublisher
	nos       OrderNoGenerator
	metrics   *metrics.Business // 业务指标打点（T19c）
	retryCfg  retry.Config      // 幂等操作有限重试（T20）：仅"抢购成功"消息发布
}

// New 构造秒杀服务。userLimiter 为按用户限流配置（Max<=0 不启用）；
// publisher 为 MQ 发布端口（预扣成功后发"抢购成功"消息，T12）；
// nos 为雪花订单号生成器（与 order 模块共用，抢购即生成订单号供前端轮询）；
// m 为业务指标（平台注册器创建，秒杀预扣/库存余量打点）；
// retryCfg 为 MQ 发布重试配置（T20 有限重试；省略 = 不重试，消息由对账兜底）。
func New(store repository.Store, products ProductService, c flashSaleCache, userLimiter limiter.RedisCounterConfig,
	publisher MessagePublisher, nos OrderNoGenerator, m *metrics.Business, retryCfg ...retry.Config) Service {
	cfg := retry.OrDefault(retryCfg...)
	return &flashsaleService{
		store:     store,
		products:  products,
		cache:     c,
		perUser:   limiter.NewRedisCounter(c, userLimiter),
		publisher: publisher,
		nos:       nos,
		metrics:   m,
		retryCfg:  cfg,
	}
}

// ---- admin ----

func (s *flashsaleService) CreateActivity(ctx context.Context, p ActivityParams) (*model.Activity, error) {
	if err := validateActivity(&p); err != nil {
		return nil, err
	}
	if err := s.checkSKU(ctx, p.SKUID); err != nil {
		return nil, err
	}
	a := &model.Activity{
		SKUID:        p.SKUID,
		Title:        p.Title,
		Price:        p.Price,
		Stock:        p.Stock,
		PerUserLimit: p.PerUserLimit,
		Status:       model.ActivityStatusOffSale,
		StartAt:      p.StartAt,
		EndAt:        p.EndAt,
	}
	if err := s.store.Activities.Create(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *flashsaleService) UpdateActivity(ctx context.Context, id int64, p ActivityParams) error {
	if err := validateActivity(&p); err != nil {
		return err
	}
	old, err := s.store.Activities.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if old == nil {
		return ErrActivityNotFound
	}
	if p.SKUID != old.SKUID {
		if err := s.checkSKU(ctx, p.SKUID); err != nil {
			return err
		}
	}

	// 进行中（当前上架且窗口覆盖 now，或编辑后窗口覆盖 now）编辑库存只减不增：
	// DB 拒绝调高，与 Redis 预热规则保持一致（DESIGN.md 上架预热节）。
	// 按"当前是否进行中"判定，避免同一次编辑改窗口绕过（把 start_at 移到未来）。
	now := time.Now()
	if p.Stock > old.Stock && old.IsOnSale() && (old.InProgress(now) || inWindow(p, now)) {
		return ErrStockIncreaseInProgress
	}

	newA := &model.Activity{
		ID:           id,
		SKUID:        p.SKUID,
		Title:        p.Title,
		Price:        p.Price,
		Stock:        p.Stock,
		PerUserLimit: p.PerUserLimit,
		Status:       old.Status,
		StartAt:      p.StartAt,
		EndAt:        p.EndAt,
	}
	if err := s.store.Activities.Update(ctx, newA); err != nil {
		return err
	}
	// 已上架的活动编辑后同步预热库存。
	if old.IsOnSale() {
		return s.syncStock(ctx, newA, now)
	}
	return nil
}

func (s *flashsaleService) ListActivities(ctx context.Context) ([]model.ActivityView, error) {
	all, err := s.store.Activities.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]model.ActivityView, 0, len(all))
	for i := range all {
		a := all[i]
		v := model.ActivityView{Activity: a}
		switch {
		case !a.IsOnSale():
			v.State = model.ActivityStateOffSale
		case now.Before(a.StartAt):
			v.State = model.ActivityStateNotStarted
		case now.After(a.EndAt):
			v.State = model.ActivityStateEnded
		default:
			v.State = model.ActivityStateInProgress
		}
		s.attachSKU(ctx, &v)
		views = append(views, v)
	}
	return views, nil
}

// ListUserActivities 秒杀页活动列表（T23）：过滤 已上架 && 未结束，
// 剩余库存读 Redis 预扣余量（key 缺失/读失败降级配置库存，规格"缓存挂直查 DB"），
// 派生状态与服务端时间对齐（进行中 / 即将开始），并拼接 SKU 规格/原价与商品标题。
// 单个 SKU/商品读取失败仅留空摘要（活动仍展示，摘要缺失不影响抢购）。
func (s *flashsaleService) ListUserActivities(ctx context.Context) ([]model.ActivityView, error) {
	all, err := s.store.Activities.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]model.ActivityView, 0, len(all))
	for i := range all {
		a := all[i]
		if !a.IsOnSale() || now.After(a.EndAt) {
			continue
		}
		v := model.ActivityView{Activity: a}
		switch {
		case now.Before(a.StartAt):
			v.State = model.ActivityStateNotStarted
		default:
			v.State = model.ActivityStateInProgress
		}
		if remaining, err := s.cache.Get(ctx, stockKey(a.ID)); err == nil {
			if n, convErr := strconv.Atoi(remaining); convErr == nil {
				v.Stock = n
				// 库存余量 gauge 同步（T19c）：秒杀页浏览即刷新余量。
				s.metrics.SetSeckillStock(a.ID, n)
			}
		}
		s.attachSKU(ctx, &v)
		views = append(views, v)
	}
	return views, nil
}

// attachSKU 拼接 SKU 规格/原价与商品标题到活动视图（admin 列表与秒杀页共用）；
// 单个 SKU/商品读取失败仅留空摘要（活动仍展示，摘要缺失不影响抢购/管理）。
func (s *flashsaleService) attachSKU(ctx context.Context, v *model.ActivityView) {
	sku, skuErr := s.products.GetSKU(ctx, v.SKUID)
	if skuErr != nil {
		return
	}
	v.SKU = model.SKUView{
		ID:        sku.ID,
		ProductID: sku.ProductID,
		Specs:     sku.Specs,
		Price:     sku.Price,
	}
	if p, pErr := s.products.GetProduct(ctx, sku.ProductID); pErr == nil {
		v.ProductTitle = p.Title
	}
}

// PublishActivity 上架：先预热 Redis 库存、后写状态——预热失败时活动保持下架，
// 避免出现"已上架但无预热库存"（那样抢购会误报已抢光）。
func (s *flashsaleService) PublishActivity(ctx context.Context, id int64) error {
	a, err := s.store.Activities.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrActivityNotFound
	}
	now := time.Now()
	if now.After(a.EndAt) {
		return fmt.Errorf("%w: activity already ended", ErrInvalidInput)
	}
	if err := s.syncStock(ctx, a, now); err != nil {
		return err
	}
	return s.store.Activities.UpdateStatus(ctx, id, model.ActivityStatusOnSale)
}

func (s *flashsaleService) UnpublishActivity(ctx context.Context, id int64) error {
	a, err := s.store.Activities.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrActivityNotFound
	}
	if err := s.store.Activities.UpdateStatus(ctx, id, model.ActivityStatusOffSale); err != nil {
		return err
	}
	// 清除预热库存：key 生命周期 = 上架时预热、下架时清理（再上架重新预热）。
	if err := s.cache.Del(ctx, stockKey(id)); err != nil {
		return err
	}
	// 库存余量 gauge 同步移除（与 Redis key 生命周期一致，T19c）。
	s.metrics.DeleteSeckillStock(id)
	return nil
}

// ---- 抢购（T11）----

// Seckill 抢购全流程（DESIGN.md 秒杀时序）：
//  1. 按用户限流：Redis 固定窗口计数（INCR+TTL，跨请求状态）；
//  2. 幂等键：原子抢占（用户+活动，TTL 30min），已存在即重复提交被拦；
//  3. 缓存原子预扣（PreDeduct，窗口/状态/限购/库存全在 Redis 内原子判定）；
//  4. 预扣成功 → 生成雪花订单号 + 发 MQ"抢购成功"消息 → 返回订单号。
//
// 预扣被业务拒绝时释放幂等键（允许窗口内重试）；基础设施失败保留幂等键
// （防瞬时故障下重复预扣，与 order 模块 client_request_id 同取舍）。
// MQ 发布失败同样保留幂等键：用户不可重复抢购，订单由对账（T15）兜底补单。
func (s *flashsaleService) Seckill(ctx context.Context, userID, activityID int64) (string, error) {
	// 1. 按用户限流：fail-closed（限流不可用时拒绝放行，保护后端）。
	ok, err := s.perUser.Allow(ctx, rlKey(userID))
	if err != nil {
		return "", fmt.Errorf("flashsale rate limit: %w", err)
	}
	if !ok {
		return "", ErrRateLimited
	}

	// 2. 幂等键：挡预扣请求的重复提交（DB 唯一约束挡落单重复，见 T12）。
	key := idemKey(activityID, userID)
	result, err := s.cache.AcquireIdempotency(ctx, key, "1", idemTTL)
	if err != nil {
		return "", fmt.Errorf("acquire flashsale idempotency: %w", err)
	}
	if result == cache.IdempotencyExists {
		return "", ErrDuplicateRequest
	}
	if result != cache.IdempotencyAcquired {
		return "", fmt.Errorf("unexpected flashsale idempotency result: %d", result)
	}

	// 3. 缓存原子预扣。
	if err := s.PreDeduct(ctx, userID, activityID); err != nil {
		if isBusinessReject(err) {
			// 业务拒绝：释放幂等键，允许用户重试（如未开始时提前抢、活动恢复上架后重抢）。
			if delErr := s.cache.Del(ctx, key); delErr != nil {
				return "", fmt.Errorf("%w: release idempotency key: %v", err, delErr)
			}
		}
		return "", err
	}

	// 4. 生成订单号 + 发 MQ（预扣成功绝不丢单：发布确认失败保留幂等键，对账兜底）。
	orderNo, err := s.publishSeckillSuccess(ctx, userID, activityID)
	if err != nil {
		return "", err
	}
	return orderNo, nil
}

// publishSeckillSuccess 生成雪花订单号并发布"抢购成功"消息（异步落单队列）。
// 发布为幂等操作（消费者按 (user_id, activity_id) 唯一约束去重），瞬时失败
// 有限重试 + 退避（T20）；重试耗尽仍失败则返回错误（保留幂等键；
// 对账 cron 识别"有预扣无订单"补单）。
func (s *flashsaleService) publishSeckillSuccess(ctx context.Context, userID, activityID int64) (string, error) {
	no, err := s.nos.Next()
	if err != nil {
		return "", fmt.Errorf("generate seckill order no: %w", err)
	}
	orderNo := strconv.FormatInt(no, 10)
	body, err := json.Marshal(SeckillSuccessMessage{
		OrderNo:    orderNo,
		UserID:     userID,
		ActivityID: activityID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal seckill message: %w", err)
	}
	// 重试复用同一 order_no 与消息体：重复投递由消费侧唯一约束幂等。
	if err := retry.Do(ctx, s.retryCfg, func(ctx context.Context) error {
		return s.publisher.Publish(ctx, SeckillOrderQueue, body)
	}); err != nil {
		return "", fmt.Errorf("publish seckill success message: %w", err)
	}
	return orderNo, nil
}

// isBusinessReject 预扣的业务拒绝分支（非基础设施故障）。
func isBusinessReject(err error) bool {
	return errors.Is(err, ErrSoldOut) || errors.Is(err, ErrNotInWindow) ||
		errors.Is(err, ErrLimitReached) || errors.Is(err, ErrOffline) ||
		errors.Is(err, ErrActivityNotFound)
}

// ---- 取消回补（T13）----

// RestoreRedis 事务提交后回补 Redis（best-effort，允许再次抢购）：
// 缓存原子 INCR 库存 + DECR 用户计数 + 释放幂等键；key 不存在不重建
// （预热过期自清理）。失败由对账 cron 兜底（Redis 有扣减但无对应订单信号）。
func (s *flashsaleService) RestoreRedis(ctx context.Context, activityID, userID int64, quantity int) error {
	err := s.cache.RestoreFlashSale(ctx, cache.FlashSaleRestoreParams{
		StockKey: stockKey(activityID), CountKey: countKey(activityID, userID),
		IdempotencyKey: idemKey(activityID, userID), Quantity: quantity,
	})
	if err == nil {
		// 库存余量 gauge 随回补刷新（best-effort 回读，T19c）。
		s.refreshStockGauge(ctx, activityID)
	}
	return err
}

// refreshStockGauge 回读 Redis 库存余量并同步 gauge（best-effort：
// key 缺失（预热过期自清理）或读失败时不更新，不影响业务）。
func (s *flashsaleService) refreshStockGauge(ctx context.Context, activityID int64) {
	if remaining, err := s.cache.Get(ctx, stockKey(activityID)); err == nil {
		if n, convErr := strconv.Atoi(remaining); convErr == nil {
			s.metrics.SetSeckillStock(activityID, n)
		}
	}
}

// ---- 抢购预扣 ----

func (s *flashsaleService) PreDeduct(ctx context.Context, userID, activityID int64) error {
	a, err := s.store.Activities.GetByID(ctx, activityID)
	if err != nil {
		s.preDeductFailed()
		return err
	}
	if a == nil {
		s.preDeductFailed()
		return ErrActivityNotFound
	}

	now := time.Now()
	result, err := s.cache.PreDeductFlashSale(ctx, cache.FlashSalePreDeductParams{
		StockKey: stockKey(a.ID), CountKey: countKey(a.ID, userID), Now: now,
		StartAt: a.StartAt, EndAt: a.EndAt, OnSale: a.Status == model.ActivityStatusOnSale,
		PerUserLimit: a.PerUserLimit,
	})
	if err != nil {
		s.preDeductFailed()
		return err
	}

	switch result {
	case cache.FlashSalePreDeducted:
		s.preDeductSuccess()
		// 库存余量 gauge 随预扣刷新（best-effort 回读，T19c）。
		s.refreshStockGauge(ctx, a.ID)
		return nil
	case cache.FlashSaleSoldOut:
		s.preDeductFailed()
		return ErrSoldOut
	case cache.FlashSaleNotInWindow:
		s.preDeductFailed()
		return ErrNotInWindow
	case cache.FlashSaleLimitReached:
		s.preDeductFailed()
		return ErrLimitReached
	case cache.FlashSaleOffline:
		s.preDeductFailed()
		return ErrOffline
	default:
		s.preDeductFailed()
		return fmt.Errorf("%w: unexpected pre-deduct result %d", ErrInvalidInput, result)
	}
}

// preDeductSuccess/preDeductFailed 预扣结果打点（T19c）。
func (s *flashsaleService) preDeductSuccess() { s.metrics.SeckillPreDeduct(true) }

func (s *flashsaleService) preDeductFailed() { s.metrics.SeckillPreDeduct(false) }

// ---- 内部 ----

// checkSKU 校验 SKU 存在（跨模块经 product 服务接口，与购物车同模式）。
func (s *flashsaleService) checkSKU(ctx context.Context, skuID int64) error {
	if _, err := s.products.GetSKU(ctx, skuID); err != nil {
		if errors.Is(err, productsvc.ErrSKUNotFound) {
			return fmt.Errorf("%w: sku not found", ErrInvalidInput)
		}
		return err
	}
	return nil
}

// syncStock 预热/同步活动库存：
//   - 未开始：可覆盖（DEL + SET），以配置为准；
//   - 进行中：只减不增（原子缓存能力：key 缺失时写入；已存在时仅配置库存更低才覆盖）；
//   - 已结束：不预热。
func (s *flashsaleService) syncStock(ctx context.Context, a *model.Activity, now time.Time) error {
	key := stockKey(a.ID)
	switch {
	case now.Before(a.StartAt): // 未开始：覆盖
		if err := s.cache.Del(ctx, key); err != nil {
			return err
		}
		if err := s.cache.Set(ctx, key, strconv.Itoa(a.Stock), remainingTTL(a)); err != nil {
			return err
		}
		s.metrics.SetSeckillStock(a.ID, a.Stock)
		return nil
	case now.After(a.EndAt): // 已结束：不预热
		return nil
	}
	// 进行中：原子只减不增（缓存适配器内含 SETNX 语义与存量保护）。
	if _, err := s.cache.WarmFlashSaleStock(ctx, cache.FlashSaleWarmParams{
		StockKey: key, Stock: a.Stock, TTL: remainingTTL(a),
	}); err != nil {
		return err
	}
	// 回读实际余量同步 gauge（进行中可能保留更低存量，T19c）。
	s.refreshStockGauge(ctx, a.ID)
	return nil
}

// remainingTTL 库存 key 存活时长：活动结束 + 1h 余量后自清理。
func remainingTTL(a *model.Activity) time.Duration {
	return time.Until(a.EndAt) + stockKeyMargin
}

// inWindow 时间窗口判定（与状态无关，仅比较时间）。
func inWindow(p ActivityParams, now time.Time) bool {
	return !now.Before(p.StartAt) && !now.After(p.EndAt)
}

// ---- 校验 ----

func validateActivity(p *ActivityParams) error {
	p.Title = strings.TrimSpace(p.Title)
	if p.Title == "" || len(p.Title) > 128 {
		return fmt.Errorf("%w: invalid title", ErrInvalidInput)
	}
	if p.SKUID <= 0 {
		return fmt.Errorf("%w: invalid sku_id", ErrInvalidInput)
	}
	if p.Price <= 0 {
		return fmt.Errorf("%w: invalid price", ErrInvalidInput)
	}
	if p.Stock < 1 {
		return fmt.Errorf("%w: invalid stock", ErrInvalidInput)
	}
	if p.PerUserLimit < 1 {
		return fmt.Errorf("%w: invalid per_user_limit", ErrInvalidInput)
	}
	if !p.StartAt.Before(p.EndAt) {
		return fmt.Errorf("%w: end_at must be after start_at", ErrInvalidInput)
	}
	return nil
}
