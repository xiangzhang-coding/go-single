// Package service 承载 flashsale 模块业务：admin 创建/编辑/上架/下架秒杀活动，
// 上架时预热独立库存进 Redis（未开始可覆盖、进行中只减不增），
// 并提供 Lua 原子预扣与抢购接口（限流 → 幂等键 → 预扣，成功返回排队中）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
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
// flashsale:count:{id}:{user}（用户抢购计数，Lua 预扣 INCR）/
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

// prewarmScript Lua 原子预热（进行中场景）：key 不存在则写入（SETNX 语义），
// 已存在时仅当配置库存更低才覆盖（只减不增）；一次调用避免与预扣 DECR 竞态。
// 返回 1 已写入 / 0 保持存量。
// KEYS[1] 库存 key；ARGV[1] 配置库存；ARGV[2] TTL(秒)。
const prewarmScript = `
local cur = tonumber(redis.call('GET', KEYS[1]) or '-1')
if cur < 0 or tonumber(ARGV[1]) < cur then
    redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return 1
end
return 0
`

// preDeductScript Lua 原子预扣（学习点，优惠券领券复用同模式）：
// 校验 status 下架标志 → 校验时间窗口 → 检查库存 → 检查每人限购 →
// DECR 库存 + INCR 用户计数。
// 返回码：1 成功 / 0 已抢光 / -1 不在时间窗口 / -2 超过每人限购 / -3 已下架。
// KEYS[1] 库存 key；KEYS[2] 用户计数 key。
// ARGV[1] 当前时间(ms)；ARGV[2] 开始时间(ms)；ARGV[3] 结束时间(ms)；
// ARGV[4] 活动状态（on_sale/off_sale）；ARGV[5] 每人限购。
const preDeductScript = `
if ARGV[4] ~= 'on_sale' then
    return -3
end
if ARGV[1] < ARGV[2] or ARGV[1] > ARGV[3] then
    return -1
end
local stock = tonumber(redis.call('GET', KEYS[1]) or '0')
if stock <= 0 then
    return 0
end
local per_user = tonumber(redis.call('GET', KEYS[2]) or '0')
if per_user >= tonumber(ARGV[5]) then
    return -2
end
redis.call('DECR', KEYS[1])
redis.call('INCR', KEYS[2])
return 1
`

// 预扣脚本返回码。
const (
	preDeductOK          int64 = 1
	preDeductSoldOut     int64 = 0
	preDeductNotInWindow int64 = -1
	preDeductLimitReach  int64 = -2
	preDeductOffline     int64 = -3
)

// idemScript 原子抢占幂等键（SETNX + EXPIRE）：返回 1 抢占成功 / 0 已存在。
// KEYS[1] 幂等键；ARGV[1] 值；ARGV[2] TTL 秒（与 order 模块 client_request_id 同模式）。
const idemScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
    return 1
end
return 0
`

// restoreScript Lua 原子回补（秒杀订单取消/超时取消后，允许再次抢购，T13）：
//   - 库存 key 存在才 INCR 回补（预热 TTL 过期自清理后不回建僵尸 key）；
//   - 用户计数 key 存在才 DECR（计数仅作限购判定，抢购前必 INCR）；
//   - 释放幂等键（30min TTL 内用户可立即重抢）。
//
// 一次调用避免与预扣 DECR/INCR 竞态（与 preDeductScript 同模式）。
// 已知取舍：与"再次抢购"的幂等键抢占（SETNX）存在毫秒级竞态——用户重抢
// 先于脚本执行时，DEL 会释放新抢购的幂等键。可接受：重复落单仍由
// user_activity_key 唯一约束（DB 幂等段）兜底，Redis 差额由对账 cron 收敛。
// KEYS[1] 库存 key；KEYS[2] 用户计数 key；KEYS[3] 幂等键；ARGV[1] 回补数量。
const restoreScript = `
if redis.call('EXISTS', KEYS[1]) == 1 then
    redis.call('INCRBY', KEYS[1], ARGV[1])
end
if redis.call('EXISTS', KEYS[2]) == 1 then
    redis.call('DECRBY', KEYS[2], ARGV[1])
end
redis.call('DEL', KEYS[3])
return 1
`

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
	ListActivities(ctx context.Context) ([]model.Activity, error)
	// PublishActivity 上架：预热库存进 Redis（未开始可覆盖 DEL+SET，进行中原子只减不增）。
	PublishActivity(ctx context.Context, id int64) error
	// UnpublishActivity 下架：清除预热库存，后续抢购被拒。
	UnpublishActivity(ctx context.Context, id int64) error

	// ---- 抢购（T11/T12）----
	// Seckill 抢购全流程：按用户限流（Redis 固定窗口计数）→ 幂等键
	// （用户+活动，TTL 30min，挡预扣请求重复提交）→ Lua 原子预扣 →
	// 生成雪花订单号 → 发 MQ"抢购成功"消息（异步落单）。
	// 预扣被业务拒绝（抢光/限购/窗口外/下架）时释放幂等键允许重试，
	// 基础设施失败保留幂等键（防瞬时故障下重复预扣）；成功返回订单号
	// （前端据此轮询订单接口），MQ 发布失败同样保留幂等键（对账兜底）。
	Seckill(ctx context.Context, userID, activityID int64) (orderNo string, err error)

	// ---- 抢购预扣（底层原子操作，T11 抢购接口复用）----
	// PreDeduct Lua 原子预扣：活动由调用方读库（与优惠券领券同模式，
	// 状态/窗口/限购作为 ARGV 传入，Redis 内原子扣减）。
	PreDeduct(ctx context.Context, userID, activityID int64) error

	// ---- 落单库存扣减（T12，order 模块事务内调用）----
	// DeductStock 事务内条件扣减活动库存（MySQL 落单事实源）；实现 order 侧
	// ActivityStock 端口，返回是否扣减成功（库存不足返回 (false, nil)）。
	DeductStock(ctx context.Context, tx *gorm.DB, activityID int64, quantity int) (bool, error)

	// ---- 取消回补（T13，order 模块超时取消调用）----
	// RestoreStock 事务内回补活动库存（MySQL，与订单状态迁移同事务）；
	// RestoreRedis 事务提交后回补 Redis 库存 + 用户计数并释放幂等键
	// （允许再次抢购）；两者共同实现 order 侧 SeckillRestore 端口。
	RestoreStock(ctx context.Context, tx *gorm.DB, activityID int64, quantity int) error
	RestoreRedis(ctx context.Context, activityID, userID int64, quantity int) error
}

// ProductService product 模块暴露的最小查询接口（跨模块进程内调用，面向接口非 HTTP；
// productSvc 天然满足，未来拆模块时换实现即可）。
type ProductService interface {
	// GetSKU 校验 SKU 存在。
	GetSKU(ctx context.Context, id int64) (*productmodel.SKU, error)
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

type flashsaleService struct {
	store     repository.Store
	products  ProductService
	cache     cache.Cache
	perUser   *limiter.RedisCounter
	publisher MessagePublisher
	nos       OrderNoGenerator
}

// New 构造秒杀服务。userLimiter 为按用户限流配置（Max<=0 不启用）；
// publisher 为 MQ 发布端口（预扣成功后发"抢购成功"消息，T12）；
// nos 为雪花订单号生成器（与 order 模块共用，抢购即生成订单号供前端轮询）。
func New(store repository.Store, products ProductService, c cache.Cache, userLimiter limiter.RedisCounterConfig,
	publisher MessagePublisher, nos OrderNoGenerator) Service {
	return &flashsaleService{
		store:     store,
		products:  products,
		cache:     c,
		perUser:   limiter.NewRedisCounter(c, userLimiter),
		publisher: publisher,
		nos:       nos,
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

func (s *flashsaleService) ListActivities(ctx context.Context) ([]model.Activity, error) {
	return s.store.Activities.List(ctx)
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
	a.Status = model.ActivityStatusOnSale
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
	return s.cache.Del(ctx, stockKey(id))
}

// ---- 抢购（T11）----

// Seckill 抢购全流程（DESIGN.md 秒杀时序）：
//  1. 按用户限流：Redis 固定窗口计数（INCR+TTL，跨请求状态）；
//  2. 幂等键：原子抢占（用户+活动，TTL 30min），已存在即重复提交被拦；
//  3. Lua 原子预扣（PreDeduct，窗口/状态/限购/库存全在 Redis 内原子判定）；
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
	code, err := s.cache.Eval(ctx, idemScript, []string{key}, 1, int64(idemTTL.Seconds()))
	if err != nil {
		return "", fmt.Errorf("acquire flashsale idempotency: %w", err)
	}
	if code != 1 {
		return "", ErrDuplicateRequest
	}

	// 3. Lua 原子预扣。
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
// 失败返回错误（保留幂等键；对账 cron 识别"有预扣无订单"补单）。
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
	if err := s.publisher.Publish(ctx, SeckillOrderQueue, body); err != nil {
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

// RestoreStock 事务内回补活动库存（MySQL，对账事实源）：秒杀订单取消在
// 订单事务内调用，与状态迁移同事务（实现 order 侧 SeckillRestore 端口）。
func (s *flashsaleService) RestoreStock(ctx context.Context, tx *gorm.DB, activityID int64, quantity int) error {
	return s.store.Activities.RestoreStock(ctx, tx, activityID, quantity)
}

// RestoreRedis 事务提交后回补 Redis（best-effort，允许再次抢购）：
// Lua 原子 INCR 库存 + DECR 用户计数 + 释放幂等键；key 不存在不重建
// （预热过期自清理）。失败由对账 cron 兜底（Redis 有扣减但无对应订单信号）。
func (s *flashsaleService) RestoreRedis(ctx context.Context, activityID, userID int64, quantity int) error {
	_, err := s.cache.Eval(ctx, restoreScript,
		[]string{stockKey(activityID), countKey(activityID, userID), idemKey(activityID, userID)},
		quantity)
	return err
}

// ---- 抢购预扣 ----

func (s *flashsaleService) PreDeduct(ctx context.Context, userID, activityID int64) error {
	a, err := s.store.Activities.GetByID(ctx, activityID)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrActivityNotFound
	}

	now := time.Now()
	code, err := s.cache.Eval(ctx, preDeductScript,
		[]string{stockKey(a.ID), countKey(a.ID, userID)},
		now.UnixMilli(), a.StartAt.UnixMilli(), a.EndAt.UnixMilli(), a.Status, a.PerUserLimit)
	if err != nil {
		return err
	}

	switch code {
	case preDeductOK:
		return nil
	case preDeductSoldOut:
		return ErrSoldOut
	case preDeductNotInWindow:
		return ErrNotInWindow
	case preDeductLimitReach:
		return ErrLimitReached
	case preDeductOffline:
		return ErrOffline
	default:
		return fmt.Errorf("%w: unexpected pre-deduct code %d", ErrInvalidInput, code)
	}
}

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
//   - 进行中：只减不增（原子 Lua：key 缺失时写入；已存在时仅配置库存更低才覆盖）；
//   - 已结束：不预热。
func (s *flashsaleService) syncStock(ctx context.Context, a *model.Activity, now time.Time) error {
	key := stockKey(a.ID)
	switch {
	case now.Before(a.StartAt): // 未开始：覆盖
		if err := s.cache.Del(ctx, key); err != nil {
			return err
		}
		return s.cache.Set(ctx, key, strconv.Itoa(a.Stock), remainingTTL(a))
	case now.After(a.EndAt): // 已结束：不预热
		return nil
	}
	// 进行中：原子只减不增（prewarmScript 内含 SETNX 语义与存量保护）。
	_, err := s.cache.Eval(ctx, prewarmScript, []string{key}, a.Stock, int(remainingTTL(a).Seconds()))
	return err
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
