// Package service 承载 flashsale 模块业务：admin 创建/编辑/上架/下架秒杀活动，
// 上架时预热独立库存进 Redis（未开始可覆盖、进行中只减不增），
// 并提供原子预扣与抢购接口（限流 → 幂等键 → 预扣，成功返回排队中）。
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrActivityNotFound               = errors.New("flashsale activity not found")
	ErrInvalidInput                   = errors.New("invalid input")
	ErrStockIncreaseInProgress        = errors.New("stock can only decrease while activity is in progress")
	ErrActivityFieldsLocked           = errors.New("sku, price, time window, and per-user limit are locked while activity is in progress")
	ErrActivityEnded                  = errors.New("flashsale activity has ended")
	ErrStockBelowAcceptedReservations = errors.New("stock cannot be reduced below accepted reservations")
	ErrReservationsUnsettled          = errors.New("flashsale reservations must settle before this activity change")
	ErrNotInWindow                    = errors.New("flashsale not in time window")
	ErrSoldOut                        = errors.New("flashsale sold out")
	ErrLimitReached                   = errors.New("per user limit reached")
	ErrOffline                        = errors.New("flashsale activity offline")
	ErrRateLimited                    = errors.New("flashsale rate limited")
	ErrPreDeductionNotFound           = errors.New("flashsale purchase not found")
	ErrRecoveryIncomplete             = errors.New("flashsale recovery incomplete")
)

// Redis key 约定（DESIGN.md）：flashsale:stock:{id}（活动库存，上架预热）/
// flashsale:count:{id}:{user}（用户抢购计数，原子预扣 INCR）/
// flashsale:idem:{id}:{user}:{slot}（槽位所有权键）/
// flashsale:reservation:{pre_deduction_id}（预扣标记，与库存/计数原子写入）/
// flashsale:pause:{id}（进行中编辑的短暂预扣栅栏）/
// flashsale:rl:{user}（按用户限流计数）。
func stockKey(id int64) string { return fmt.Sprintf("flashsale:stock:%d", id) }
func countKey(id, userID int64) string {
	return fmt.Sprintf("flashsale:count:%d:%d", id, userID)
}
func legacyIdemKey(activityID, userID int64) string {
	return fmt.Sprintf("flashsale:idem:%d:%d", activityID, userID)
}
func slotIdemKey(activityID, userID, purchaseSlot int64) string {
	return fmt.Sprintf("flashsale:idem:%d:%d:%d", activityID, userID, purchaseSlot)
}
func reservationKey(preDeductionID int64) string {
	return fmt.Sprintf("flashsale:reservation:%d", preDeductionID)
}
func pauseKey(activityID int64) string { return fmt.Sprintf("flashsale:pause:%d", activityID) }
func rlKey(userID int64) string        { return fmt.Sprintf("flashsale:rl:%d", userID) }

const (
	redisAOFTimeout         = 2 * time.Second
	preDeductionLockStripes = 256
)

type PurchaseResult struct {
	PreDeductionID int64
	OrderNo        string
	Status         model.PreDeductionStatus
}

type RecoveryStats struct {
	Published  int
	RolledBack int
	Failed     int
}

type PreDeductionRecovery interface {
	RecoverPreDeductions(ctx context.Context) (RecoveryStats, error)
	RecoverPreDeductionsAtStartup(ctx context.Context) (RecoveryStats, error)
}

type PurchaseRecoveryGate interface {
	BlockPurchases()
	AllowPurchases()
	PurchasesBlocked() bool
}

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
	PreDeductionRecovery
	PurchaseRecoveryGate
	// ---- admin ----
	CreateActivity(ctx context.Context, p ActivityParams) (*model.Activity, error)
	UpdateActivity(ctx context.Context, id int64, p ActivityParams) error
	// ListActivities 后台活动列表（T25 富化）：全状态（含下架/已结束），
	// 每项携带派生状态（off_sale/not_started/in_progress/ended）与 SKU/商品摘要。
	ListActivities(ctx context.Context) ([]model.ActivityView, error)
	// PublishActivity 上架：durable Lua 预热库存（未开始可覆盖，进行中原子只减不增）。
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
	// Seckill 抢购全流程：持久 preparing 事实 → 以稳定 ID 抢占幂等键 →
	// 缓存原子写入库存/计数/reservation marker → 持久订单号 → 发布 MQ。
	// 预扣成功后即由恢复任务接管，发布失败不再让请求事实丢失。
	Seckill(ctx context.Context, userID, activityID int64, clientRequestID string) (*PurchaseResult, error)
	GetPreDeduction(ctx context.Context, userID, id int64) (*model.PreDeduction, error)
	RecoverPreDeduction(ctx context.Context, id int64) error
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
// 发布"抢购成功"消息驱动异步落单；确认失败由持久预扣事实重试。
type MessagePublisher interface {
	Publish(ctx context.Context, queue string, body []byte) error
}

// OrderNoGenerator 秒杀订单号生成器（雪花 ID，与 order 模块共用同一实例）。
type OrderNoGenerator interface {
	Next() (int64, error)
}

type flashSaleCache interface {
	cache.IdempotencyStore
	cache.DurableIdempotencyStore
	cache.FlashSaleStore
	cache.FixedWindowStore
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

type flashsaleService struct {
	store           repository.Store
	products        ProductService
	cache           flashSaleCache
	perUser         *limiter.RedisCounter
	publisher       MessagePublisher
	nos             OrderNoGenerator
	metrics         *metrics.Business // 业务指标打点（T19c）
	retryCfg        retry.Config      // 幂等操作有限重试（T20）：仅"抢购成功"消息发布
	adminMu         sync.RWMutex      // 编辑与“读取活动→接受预扣”互斥，抢购之间仍可并发
	recoveryBlocked atomic.Bool
	preDeductionMu  [preDeductionLockStripes]sync.Mutex
}

func (s *flashsaleService) BlockPurchases() {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	s.recoveryBlocked.Store(true)
}

func (s *flashsaleService) AllowPurchases() {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	s.recoveryBlocked.Store(false)
}

func (s *flashsaleService) PurchasesBlocked() bool { return s.recoveryBlocked.Load() }

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
