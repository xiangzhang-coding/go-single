// Package service 承载 flashsale 模块业务：admin 创建/编辑/上架/下架秒杀活动，
// 上架时预热独立库存进 Redis（未开始可覆盖、进行中只减不增），
// 并提供 Lua 原子预扣（T11 抢购接口复用）。
package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
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
)

// Redis key 约定（DESIGN.md）：flashsale:stock:{id}（活动库存，上架预热）/
// flashsale:count:{id}:{user}（用户抢购计数，Lua 预扣 INCR）。
func stockKey(id int64) string { return fmt.Sprintf("flashsale:stock:%d", id) }
func countKey(id, userID int64) string {
	return fmt.Sprintf("flashsale:count:%d:%d", id, userID)
}

// stockKeyMargin 预热库存 TTL 的余量：剩余时长 + 1h，活动结束后自清理。
const stockKeyMargin = time.Hour

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

	// ---- 抢购预扣（T11 抢购接口复用）----
	// PreDeduct Lua 原子预扣：活动由调用方读库（与优惠券领券同模式，
	// 状态/窗口/限购作为 ARGV 传入，Redis 内原子扣减）。
	PreDeduct(ctx context.Context, userID, activityID int64) error
}

// ProductService product 模块暴露的最小查询接口（跨模块进程内调用，面向接口非 HTTP；
// productSvc 天然满足，未来拆模块时换实现即可）。
type ProductService interface {
	// GetSKU 校验 SKU 存在。
	GetSKU(ctx context.Context, id int64) (*productmodel.SKU, error)
}

type flashsaleService struct {
	store    repository.Store
	products ProductService
	cache    cache.Cache
}

// New 构造秒杀服务。
func New(store repository.Store, products ProductService, c cache.Cache) Service {
	return &flashsaleService{store: store, products: products, cache: c}
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
