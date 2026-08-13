// Package service 承载 order 模块业务：下单（购物车结算/直购）、订单列表与详情、
// 取消待支付订单、确认收货与后台发货，状态机非法跃迁一律拒绝。
//
// 下单为单事务（订单 + 订单项 + 库存条件更新 stock>=N + 地址快照 + 券核销 +
// 删除购物车条目），事务由 order 仓储开启，跨模块写操作经各模块 service 的
// tx 参数汇入同一事务；client_request_id 幂等（Redis SETNX + TTL 15min）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"

	cartmodel "github.com/xiangzhang-coding/go-single/internal/cart/model"
	couponmodel "github.com/xiangzhang-coding/go-single/internal/coupon/model"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/order/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrInvalidInput          = errors.New("invalid input")
	ErrOrderNotFound         = errors.New("order not found")
	ErrOrderForbidden        = errors.New("order does not belong to user")
	ErrOrderChanged          = errors.New("order status changed, retry")
	ErrIllegalTransition     = errors.New("illegal order status transition")
	ErrCartEmpty             = errors.New("cart is empty")
	ErrInsufficientStock     = errors.New("insufficient stock")
	ErrSKUNotFound           = errors.New("sku not found")
	ErrSKUUnavailable        = errors.New("sku product is not on sale")
	ErrCouponNotFound        = errors.New("coupon not found")
	ErrCouponUsed            = errors.New("coupon already used")
	ErrCouponExpired         = errors.New("coupon not in valid period")
	ErrCouponThresholdNotMet = errors.New("coupon threshold not met")
	ErrAddressNotFound       = errors.New("address not found")
	ErrAddressForbidden      = errors.New("address does not belong to user")
	// ErrSeckillStockInsufficient 秒杀落单时活动 MySQL 库存不足（条件扣减未命中）：
	// 预扣成功但落单无法扣库存（如活动期间后台调低库存），永久失败进死信，对账兜底。
	ErrSeckillStockInsufficient = errors.New("seckill activity stock insufficient")
)

// 超时取消：普通订单默认 15min（expire_at 写入订单，T09 定时任务扫描）；
// 秒杀订单 10min（T12 落单时写入；取消回补见后续对账/取消任务）。
const (
	normalExpire  = 15 * time.Minute
	seckillExpire = 10 * time.Minute
	// 幂等键 TTL：与规格一致（15min），超时后允许同一 client_request_id 重新下单。
	idemTTL = 15 * time.Minute
	// 分页上限与默认页大小。
	defaultPageSize = 20
	maxPageSize     = 50
	// 超时扫描每 tick 处理上限：余量下个 tick 续扫（cron 每分钟一次）。
	cancelExpiredBatch = 500
)

// 幂等键约定：order:idem:{user_id}:{client_request_id} → 订单号。
func idemKey(userID int64, clientRequestID string) string {
	return fmt.Sprintf("order:idem:%d:%s", userID, clientRequestID)
}

// idemScript 原子抢占幂等键（SETNX + EXPIRE）：返回 1 抢占成功 / 0 已存在。
// KEYS[1] 幂等键；ARGV[1] 订单号；ARGV[2] TTL 秒。
const idemScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
    return 1
end
return 0
`

// ---- 跨模块最小接口（进程内调用，面向接口非 HTTP；实现见 main 装配） ----

// ProductService 商品模块最小接口：下单读取 SKU/上架校验，
// 事务内条件扣减与回补库存（tx 由 order 模块开启）。
type ProductService interface {
	GetSKU(ctx context.Context, id int64) (*productmodel.SKU, error)
	GetSKUForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*productmodel.SKU, error)
	GetDetail(ctx context.Context, id int64) (*productmodel.ProductDetail, error)
	DeductStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) (bool, error)
	RestoreStock(ctx context.Context, tx *gorm.DB, skuID int64, quantity int) error
}

// CouponService 优惠券模块最小接口：结算前校验可用券，事务内核销/回退。
type CouponService interface {
	GetUsable(ctx context.Context, userID, couponID int64) (*couponmodel.UserCouponView, error)
	UseCoupon(ctx context.Context, tx *gorm.DB, userID, couponID int64) error
	RollbackCoupon(ctx context.Context, tx *gorm.DB, userID, couponID int64) error
}

// CartService 购物车模块最小接口：在订单事务内锁定并读取当前条目，
// 再按条目 ID 删除已购行。
type CartService interface {
	LockItems(ctx context.Context, tx *gorm.DB, userID int64) ([]cartmodel.CartItem, error)
	DeletePurchased(ctx context.Context, tx *gorm.DB, userID int64, itemIDs []int64) error
}

// UserService 用户模块最小接口：读取地址簿固化为地址快照（owner 校验）。
type UserService interface {
	GetAddress(ctx context.Context, userID, id int64) (*usermodel.Address, error)
}

// ActivityStock 秒杀活动库存最小接口（跨模块端口，实现方为 flashsale 模块）：
// 秒杀异步落单在同一事务内条件扣减活动库存（MySQL 为落单事实源，防超卖）。
// 接口定义在 order 侧、由 flashsale 实现（flashsale → order 单向依赖，无环）。
type ActivityStock interface {
	// DeductStock 事务内条件扣减活动库存（stock >= quantity）；库存不足返回 (false, nil)。
	DeductStock(ctx context.Context, tx *gorm.DB, activityID int64, quantity int) (bool, error)
}

// SeckillRestore 秒杀订单取消回补端口（跨模块端口，实现方为 flashsale 模块，
// 与 ActivityStock 同模式）：超时取消时回补活动库存与 Redis 预扣态，
// 允许用户再次抢购（T13）。
type SeckillRestore interface {
	// RestoreStock 事务内回补活动库存（与取消状态迁移同事务，MySQL 为对账事实源）。
	RestoreStock(ctx context.Context, tx *gorm.DB, activityID int64, quantity int) error
	// RestoreRedis 事务提交后回补 Redis（库存 + 用户计数 + 释放幂等键）。
	// best-effort：失败不阻断取消（订单已落取消态），由对账 cron 兜底。
	RestoreRedis(ctx context.Context, activityID, userID int64, quantity int) error
}

// OrderNoGenerator 订单号生成器（雪花 ID 手写实现，见 platform/snowflake）。
type OrderNoGenerator interface {
	Next() (int64, error)
}

// ItemParams 直购订单项。
type ItemParams struct {
	SKUID    int64
	Quantity int
}

// CreateParams 下单参数；FromCart 与 Items 二选一（购物车结算 / 商品直购）。
type CreateParams struct {
	ClientRequestID string
	AddressID       int64
	CouponID        int64 // 0 = 不使用券
	FromCart        bool
	Items           []ItemParams
}

// CreateResult 下单结果；Idempotent=true 表示命中了 client_request_id
// 幂等键（重复提交），Order 为既有订单。Processing=true 表示订单号已被
// 幂等请求占用但订单尚未提交，HTTP 层应返回 202，客户端轮询详情。
type CreateResult struct {
	Order      *model.OrderView
	Idempotent bool
	Processing bool
}

// SeckillCreateParams 秒杀异步落单参数（MQ 消费者侧组装）：
// OrderNo 预扣时已生成（雪花）；地址为消费时固化的默认地址快照。
type SeckillCreateParams struct {
	OrderNo    string
	UserID     int64
	ActivityID int64
	SKUID      int64
	Price      int64 // 秒杀价快照（活动价格）
	Quantity   int
	Address    *usermodel.Address // 默认地址快照（用户后续改地址不影响历史订单）
}

// Service order 模块的业务接口。
type Service interface {
	// Create 下单（购物车结算 / 直购）：单事务创建订单；client_request_id
	// 重复提交返回同一订单号（幂等）。
	Create(ctx context.Context, userID int64, p CreateParams) (*CreateResult, error)
	// CreateSeckill 秒杀异步落单（T12，MQ 消费者回调）：
	// 单事务创建秒杀订单（order_type=seckill，不使用券）+ 订单项 +
	// 条件扣减活动库存（ActivityStock）；user_activity_key 唯一约束（T13，
	// 取消置 NULL 后不再占位）与 order_no 主键命中重复（重复投递/并发消费）
	// 视为成功（幂等，不重复扣库存）。
	// 活动库存不足返回 ErrSeckillStockInsufficient（永久失败，死信 + 对账兜底）。
	CreateSeckill(ctx context.Context, p SeckillCreateParams) error
	// List 我的订单（状态筛选 + 分页）。
	List(ctx context.Context, userID int64, status string, page, pageSize int) ([]model.OrderView, int64, error)
	// ListAll 后台全量订单（admin，T25）：跨用户，状态筛选 + 分页，订单项随列表一次取出。
	ListAll(ctx context.Context, status string, page, pageSize int) ([]model.OrderView, int64, error)
	// GetDetail 订单详情（owner 校验，防 IDOR）。
	GetDetail(ctx context.Context, userID int64, orderNo string) (*model.OrderView, error)
	// Cancel 取消待支付订单：回补库存 + 回退券；状态机非法跃迁拒绝。
	Cancel(ctx context.Context, userID int64, orderNo string) error
	// CancelExpired 超时取消（cron 任务回调）：扫描待支付且已过 expire_at 的
	// 普通订单，逐个事务内取消（条件更新 + 回补库存 + 回退券）；
	// 返回 (取消数, 失败数)。并发下已被支付/取消的订单跳过（条件更新兜底）。
	CancelExpired(ctx context.Context) (cancelled, failed int, err error)
	// CancelExpiredSeckill 秒杀订单超时取消（cron 任务回调，T13）：扫描待支付
	// 且已过 expire_at 的秒杀订单，逐个事务内取消（条件更新 + 回补活动库存），
	// 事务提交后回补 Redis（库存 + 用户计数 + 释放幂等键，允许再次抢购）。
	// 返回 (取消数, DB 失败数, Redis 回补失败数)：DB 失败下个 tick 重试；
	// Redis 失败不重试（订单已取消，重试将命中状态已变），由对账 cron 兜底。
	CancelExpiredSeckill(ctx context.Context) (cancelled, failed, redisFailed int, err error)
	// CountValidSeckill 活动的秒杀有效订单数（非取消），对账端口
	// （flashsale 模块 ReconcileActive 进程内调用）。
	CountValidSeckill(ctx context.Context, activityID int64) (int, error)
	// MarkPaid 支付成功状态迁移：待支付 → 已支付（支付模块事务内条件更新；
	// WHERE 同时校验 status 与 pay_amount，false = 状态已变或金额不符）。
	MarkPaid(ctx context.Context, tx *gorm.DB, orderNo string, payAmount int64) (bool, error)
	// Ship 后台发货：已支付 → 已发货（admin）。
	Ship(ctx context.Context, orderNo string) error
	// ConfirmReceipt 确认收货：已发货 → 已完成（owner 校验）。
	ConfirmReceipt(ctx context.Context, userID int64, orderNo string) error
	// HasPurchasedSKU 用户是否已购某 SKU（已支付/已发货/已完成订单含该 SKU），
	// 好友圈分享的购买校验端口（social 模块进程内调用）。
	HasPurchasedSKU(ctx context.Context, userID, skuID int64) (bool, error)
}

// orderLine 下单行（购物车条目或直购项统一形态）。
type orderLine struct {
	skuID    int64
	quantity int
}

// lineSnapshot 订单项快照（下单时固化的商品信息）。
type lineSnapshot struct {
	skuID     int64
	productID int64
	title     string
	specs     json.RawMessage
	price     int64
	quantity  int
}

type orderService struct {
	store repository.Store
	cache cache.Cache
	nos   OrderNoGenerator

	products   ProductService
	coupons    CouponService
	cart       CartService
	users      UserService
	activities ActivityStock  // 秒杀活动库存（跨模块端口，flashsale 实现）
	seckill    SeckillRestore // 秒杀订单取消回补（跨模块端口，flashsale 实现，T13）
	metrics    *metrics.Business
	retryCfg   retry.Config // 幂等操作有限重试（T20）：仅普通下单 Create
}

// New 构造订单服务；retryCfg 为下单重试配置（T20 有限重试；省略 = 不重试）。
func New(store repository.Store, c cache.Cache, nos OrderNoGenerator,
	products ProductService, coupons CouponService, cart CartService, users UserService,
	activities ActivityStock, seckill SeckillRestore, m *metrics.Business, retryCfg ...retry.Config) Service {
	cfg := retry.OrDefault(retryCfg...)
	return &orderService{store: store, cache: c, nos: nos, products: products, coupons: coupons, cart: cart, users: users, activities: activities, seckill: seckill, metrics: m, retryCfg: cfg}
}

// ---- 下单 ----

// Create 下单（幂等接口有限重试，T20）：client_request_id 幂等 + 单事务原子性，
// 基础设施瞬时故障（DB/Redis 抖动）重试 + 退避；业务拒绝（校验类错误）经
// retry.Stop 立即返回不重试。真实执行见 createOrder。
func (s *orderService) Create(ctx context.Context, userID int64, p CreateParams) (*CreateResult, error) {
	var result *CreateResult
	err := retry.Do(ctx, s.retryCfg, func(ctx context.Context) error {
		var attemptErr error
		result, attemptErr = s.createOrder(ctx, userID, p)
		if attemptErr != nil && !isValidationError(attemptErr) {
			return attemptErr // 基础设施/未知错误：可重试
		}
		return retry.Stop(attemptErr) // 业务拒绝：重试无意义
	})
	return result, err
}

// createOrder 下单流程：
//  1. 生成雪花订单号并原子抢占幂等键（SETNX client_request_id，重复请求返回同一订单号）
//  2. 读取地址（固化为快照）→ 组装订单项（购物车/直购）→ 校验券可用
//  3. 读取 SKU 价格与上架状态，累计商品总额，计算券额与应付
//  4. 单事务：条件扣减库存 → 核销券 → 建订单+订单项 → 删除已购购物车条目
//
// 任一校验失败（库存不足/券不可用等）删除幂等键，允许修正后重试。
// 幂等语义保证重试安全：事务原子回滚 + 幂等键重复请求返回同一订单号。
func (s *orderService) createOrder(ctx context.Context, userID int64, p CreateParams) (result *CreateResult, err error) {
	if userID <= 0 || p.AddressID <= 0 {
		return nil, fmt.Errorf("%w: invalid user or address id", ErrInvalidInput)
	}
	if p.ClientRequestID == "" || len(p.ClientRequestID) > 64 {
		return nil, fmt.Errorf("%w: invalid client_request_id", ErrInvalidInput)
	}
	if p.CouponID < 0 {
		return nil, fmt.Errorf("%w: invalid coupon id", ErrInvalidInput)
	}
	if err := validateItems(&p); err != nil {
		return nil, err
	}

	orderNo, acquired, err := s.acquireIdempotency(ctx, userID, p.ClientRequestID)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return s.replayIdempotent(ctx, orderNo)
	}
	// 校验类失败释放幂等键，允许客户端修正后重试；基础设施类失败先查订单：
	// 已提交则保留幂等键返回同一订单号，确认未提交则释放，避免永久占用请求。
	defer func() {
		if err == nil {
			return
		}
		shouldRelease := isValidationError(err)
		if !shouldRelease {
			existing, lookupErr := s.store.Orders.GetByNo(ctx, orderNo)
			shouldRelease = lookupErr == nil && existing == nil
		}
		if shouldRelease {
			if delErr := s.cache.Del(ctx, idemKey(userID, p.ClientRequestID)); delErr != nil {
				err = fmt.Errorf("%w: release idempotency key: %v", err, delErr)
			}
		}
	}()

	address, err := s.users.GetAddress(ctx, userID, p.AddressID)
	if err != nil {
		if errors.Is(err, usersvc.ErrAddressNotFound) {
			return nil, ErrAddressNotFound
		}
		if errors.Is(err, usersvc.ErrAddressForbidden) {
			return nil, ErrAddressForbidden
		}
		return nil, err
	}
	if address == nil {
		return nil, ErrAddressNotFound
	}

	var coupon *couponmodel.UserCouponView
	if p.CouponID > 0 {
		coupon, err = s.coupons.GetUsable(ctx, userID, p.CouponID)
		if err != nil {
			return nil, translateCouponError(err)
		}
	}

	var (
		snapshots   []lineSnapshot
		total       int64
		order       *model.Order
		items       []model.OrderItem
		cartItemIDs []int64
	)
	err = s.store.Tx.WithinTx(ctx, func(tx *gorm.DB) error {
		if p.FromCart {
			cartItems, err := s.cart.LockItems(ctx, tx, userID)
			if err != nil {
				return err
			}
			if len(cartItems) == 0 {
				return ErrCartEmpty
			}
			lines := make([]orderLine, 0, len(cartItems))
			cartItemIDs = make([]int64, 0, len(cartItems))
			for _, item := range cartItems {
				lines = append(lines, orderLine{skuID: item.SKUID, quantity: item.Quantity})
				cartItemIDs = append(cartItemIDs, item.ID)
			}
			snapshots, total, err = s.loadSnapshots(ctx, tx, lines)
			if err != nil {
				return err
			}
		} else {
			lines := directLines(&p)
			snapshots, total, err = s.loadSnapshots(ctx, tx, lines)
			if err != nil {
				return err
			}
		}

		discount, pay, err := calculateAmounts(total, coupon)
		if err != nil {
			return err
		}
		order, items = buildOrder(userID, orderNo, address, snapshots, total, discount, pay, coupon)
		return s.persistOrder(ctx, tx, userID, p.CouponID, coupon, snapshots, order, items, cartItemIDs)
	})
	if err != nil {
		return nil, err
	}

	// 订单创建/状态流转打点（T19c）：事务提交后计数，幂等命中不重复计数。
	s.metrics.OrderCreated(model.OrderTypeNormal)
	s.metrics.OrderStatusChanged(model.OrderStatusPendingPayment)
	return &CreateResult{Order: &model.OrderView{Order: *order, Items: items}}, nil
}

// CreateSeckill 秒杀异步落单（T12，MQ 消费者回调）：
//  1. 读取 SKU 快照（specs/product_id 与商品标题；不锁 SKU 行——秒杀只扣活动库存）
//  2. 单事务：建秒杀订单（10min 超时）+ 订单项 + 条件扣减活动库存
//  3. 重复键（order_no 主键 / user_activity_key 唯一约束）→ 幂等成功
//     （重复投递或并发消费，订单已存在，不重复扣库存）
//  4. 活动库存不足 → ErrSeckillStockInsufficient（永久失败，死信 + 对账兜底）
func (s *orderService) CreateSeckill(ctx context.Context, p SeckillCreateParams) error {
	if p.OrderNo == "" || p.UserID <= 0 || p.ActivityID <= 0 || p.SKUID <= 0 ||
		p.Price < 0 || p.Quantity < 1 || p.Quantity > 99 || p.Address == nil {
		return fmt.Errorf("%w: invalid seckill order params", ErrInvalidInput)
	}

	// 商品快照（标题/规格），跨模块经 product 服务读取（同普通下单）。
	sku, err := s.products.GetSKU(ctx, p.SKUID)
	if err != nil {
		return translateProductError(err)
	}
	if sku == nil {
		return ErrSKUNotFound
	}
	detail, err := s.products.GetDetail(ctx, sku.ProductID)
	if err != nil {
		if errors.Is(err, productsvc.ErrProductNotFound) {
			return ErrSKUUnavailable
		}
		return err
	}

	now := time.Now()
	activityID := p.ActivityID
	// 秒杀去重键（T13）：写 "user_id:activity_id"，唯一约束挡重复落单；
	// 取消时同事务置 NULL，允许同一用户取消后再次抢购。
	dedupKey := fmt.Sprintf("%d:%d", p.UserID, activityID)
	order := &model.Order{
		OrderNo:         p.OrderNo,
		UserID:          p.UserID,
		OrderType:       model.OrderTypeSeckill,
		Status:          model.OrderStatusPendingPayment,
		ActivityID:      &activityID,
		TotalAmount:     p.Price * int64(p.Quantity),
		PayAmount:       p.Price * int64(p.Quantity),
		Receiver:        p.Address.Receiver,
		Phone:           p.Address.Phone,
		Province:        p.Address.Province,
		City:            p.Address.City,
		District:        p.Address.District,
		Detail:          p.Address.Detail,
		ExpireAt:        now.Add(seckillExpire),
		UserActivityKey: &dedupKey,
	}
	item := model.OrderItem{
		OrderNo:   p.OrderNo,
		SKUID:     p.SKUID,
		ProductID: sku.ProductID,
		Title:     detail.Product.Title,
		Specs:     sku.Specs,
		Price:     p.Price,
		Quantity:  p.Quantity,
		Subtotal:  p.Price * int64(p.Quantity),
	}

	err = s.store.Tx.WithinTx(ctx, func(tx *gorm.DB) error {
		if err := s.store.Orders.Create(ctx, tx, order); err != nil {
			return err
		}
		if err := s.store.Items.Create(ctx, tx, &item); err != nil {
			return err
		}
		ok, err := s.activities.DeductStock(ctx, tx, p.ActivityID, p.Quantity)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSeckillStockInsufficient
		}
		return nil
	})
	if err != nil {
		// 重复键 = 幂等命中（订单已存在，消费重投/并发建单成功路径），
		// 事务整体回滚（不重复扣库存）后视为成功。
		if errors.Is(err, repository.ErrOrderDuplicate) {
			return nil
		}
		return err
	}
	// 订单创建/状态流转打点（T19c）：重复键幂等命中不计数。
	s.metrics.OrderCreated(model.OrderTypeSeckill)
	s.metrics.OrderStatusChanged(model.OrderStatusPendingPayment)
	return nil
}

// persistOrder 在同一事务内完成库存、券、订单、订单项与购物车清理。
func (s *orderService) persistOrder(ctx context.Context, tx *gorm.DB, userID, couponID int64,
	coupon *couponmodel.UserCouponView, snapshots []lineSnapshot, order *model.Order,
	items []model.OrderItem, cartItemIDs []int64) error {
	for _, sn := range snapshots {
		ok, err := s.products.DeductStock(ctx, tx, sn.skuID, sn.quantity)
		if err != nil {
			return translateProductError(err)
		}
		if !ok {
			return ErrInsufficientStock
		}
	}
	if coupon != nil {
		// 核销失败：已用/过期/不存在经 translateCouponError 区分（409/404）。
		if err := s.coupons.UseCoupon(ctx, tx, userID, couponID); err != nil {
			return translateCouponError(err)
		}
	}
	if err := s.store.Orders.Create(ctx, tx, order); err != nil {
		return err
	}
	for i := range items {
		if err := s.store.Items.Create(ctx, tx, &items[i]); err != nil {
			return err
		}
	}
	if len(cartItemIDs) > 0 {
		if err := s.cart.DeletePurchased(ctx, tx, userID, cartItemIDs); err != nil {
			return err
		}
	}
	return nil
}

// acquireIdempotency 生成雪花订单号并原子抢占幂等键（SETNX+EX）；
// 返回 (订单号, 是否抢占成功)。未抢占成功时订单号为既有键中的值，
// 客户端据此查询/轮询同一订单（并发重复提交时订单可能尚未提交）。
// 注意：基础设施类失败（Redis 故障/时钟回拨）不包装为业务错误，
// 使调用方保持幂等键（防瞬时故障下重试生成第二单）。
func (s *orderService) acquireIdempotency(ctx context.Context, userID int64, clientRequestID string) (string, bool, error) {
	no, err := s.nos.Next()
	if err != nil {
		return "", false, fmt.Errorf("generate order no: %w", err)
	}
	orderNo := strconv.FormatInt(no, 10)

	key := idemKey(userID, clientRequestID)
	code, err := s.cache.Eval(ctx, idemScript, []string{key}, orderNo, int64(idemTTL.Seconds()))
	if err != nil {
		return "", false, fmt.Errorf("acquire idempotency key: %w", err)
	}
	if code == 1 {
		return orderNo, true, nil
	}
	// 已存在：返回既有订单号（同一 client_request_id 复用同一订单）。
	raw, err := s.cache.Get(ctx, key)
	if err != nil {
		return "", false, err
	}
	if raw == "" {
		return "", false, fmt.Errorf("idempotency key %s empty", key)
	}
	return raw, false, nil
}

// replayIdempotent 命中幂等键：返回既有订单（同一订单号）。
// 订单号已落键但订单尚未提交（并发在途，或首提遇基础设施故障且幂等键保留）时，
// 返回仅含 order_no 的订单视图（status 为空即"未落库"标记），
// 客户端应轮询 GET /api/orders/{order_no} 直至状态非空。
func (s *orderService) replayIdempotent(ctx context.Context, orderNo string) (*CreateResult, error) {
	view, err := s.loadView(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return &CreateResult{
			Order:      &model.OrderView{Order: model.Order{OrderNo: orderNo}},
			Idempotent: true,
			Processing: true,
		}, nil
	}
	return &CreateResult{Order: view, Idempotent: true}, nil
}

// translateCouponError 跨模块错误翻译为本模块业务错误（handler 据此映射 HTTP 状态码）。
func translateCouponError(err error) error {
	switch {
	case errors.Is(err, couponsvc.ErrCouponNotFound):
		return ErrCouponNotFound
	case errors.Is(err, couponsvc.ErrCouponUsed):
		return ErrCouponUsed
	case errors.Is(err, couponsvc.ErrCouponExpired):
		return ErrCouponExpired
	case errors.Is(err, couponsvc.ErrCouponRollbackFailed):
		return fmt.Errorf("%w: %v", ErrCouponUsed, err)
	}
	return err
}

func validateItems(p *CreateParams) error {
	if p.FromCart {
		if len(p.Items) > 0 {
			return fmt.Errorf("%w: from_cart and items are mutually exclusive", ErrInvalidInput)
		}
		return nil
	}
	if len(p.Items) == 0 {
		return fmt.Errorf("%w: empty items", ErrInvalidInput)
	}
	// 直购同 SKU 多行合并为一行：同一 SKU 只扣一次库存、只生成一条订单项。
	merged := make(map[int64]int, len(p.Items))
	for _, it := range p.Items {
		if it.SKUID <= 0 {
			return fmt.Errorf("%w: invalid sku id", ErrInvalidInput)
		}
		if it.Quantity < 1 || it.Quantity > 99 {
			return fmt.Errorf("%w: invalid quantity", ErrInvalidInput)
		}
		merged[it.SKUID] += it.Quantity
	}
	items := make([]ItemParams, 0, len(merged))
	for skuID, qty := range merged {
		// 合并后超过上限：明确拒绝（金额敏感路径不做静默裁剪）。
		if qty > 99 {
			return fmt.Errorf("%w: merged quantity exceeds 99", ErrInvalidInput)
		}
		items = append(items, ItemParams{SKUID: skuID, Quantity: qty})
	}
	p.Items = items
	return nil
}

// collectLines 购物车结算：读取购物车全部条目；直购：透传请求项。
// directLines 将直购请求转换为统一下单行；购物车行只在事务内经 LockItems 读取。
func directLines(p *CreateParams) []orderLine {
	lines := make([]orderLine, 0, len(p.Items))
	for _, it := range p.Items {
		lines = append(lines, orderLine{skuID: it.SKUID, quantity: it.Quantity})
	}
	return lines
}

func calculateAmounts(total int64, coupon *couponmodel.UserCouponView) (int64, int64, error) {
	discount := int64(0)
	if coupon != nil {
		if total < coupon.MinAmount {
			return 0, 0, ErrCouponThresholdNotMet
		}
		discount = coupon.Value
		if discount > total {
			discount = total
		}
	}
	return discount, total - discount, nil
}

// translateProductError 将事务内商品服务错误翻译为订单模块错误，避免 HTTP 500。
func translateProductError(err error) error {
	switch {
	case errors.Is(err, productsvc.ErrSKUNotFound):
		return ErrSKUNotFound
	case errors.Is(err, productsvc.ErrProductNotFound):
		return ErrSKUUnavailable
	}
	return err
}

// loadSnapshots 读取 SKU 价格并校验存在/上架（GetDetail 仅上架可见，404 即下架），
// 累计商品总额，产出订单项快照。
func (s *orderService) loadSnapshots(ctx context.Context, tx *gorm.DB, lines []orderLine) ([]lineSnapshot, int64, error) {
	type candidate struct {
		line      orderLine
		productID int64
	}
	candidates := make([]candidate, 0, len(lines))
	for _, line := range lines {
		sku, err := s.products.GetSKU(ctx, line.skuID)
		if err != nil {
			return nil, 0, translateProductError(err)
		}
		if sku == nil {
			return nil, 0, ErrSKUNotFound
		}
		candidates = append(candidates, candidate{line: line, productID: sku.ProductID})
	}
	// 先按商品再按 SKU 排序；GetSKUForUpdate 按 product → SKU 加锁，
	// 多商品订单之间使用全局一致的锁顺序，避免死锁。
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].productID != candidates[j].productID {
			return candidates[i].productID < candidates[j].productID
		}
		return candidates[i].line.skuID < candidates[j].line.skuID
	})

	snapshots := make([]lineSnapshot, 0, len(candidates))
	var total int64
	for _, candidate := range candidates {
		l := candidate.line
		sku, err := s.products.GetSKUForUpdate(ctx, tx, l.skuID)
		if err != nil {
			if errors.Is(err, productsvc.ErrSKUNotFound) {
				return nil, 0, ErrSKUNotFound
			}
			if errors.Is(err, productsvc.ErrProductNotFound) {
				return nil, 0, ErrSKUUnavailable
			}
			return nil, 0, err
		}
		if sku == nil {
			return nil, 0, ErrSKUNotFound
		}
		detail, err := s.products.GetDetail(ctx, sku.ProductID)
		if err != nil {
			if errors.Is(err, productsvc.ErrProductNotFound) {
				return nil, 0, ErrSKUUnavailable
			}
			return nil, 0, err
		}
		snapshots = append(snapshots, lineSnapshot{
			skuID:     sku.ID,
			productID: sku.ProductID,
			title:     detail.Product.Title,
			specs:     sku.Specs,
			price:     sku.Price,
			quantity:  l.quantity,
		})
		total += sku.Price * int64(l.quantity)
	}
	return snapshots, total, nil
}

// buildOrder 组装订单与订单项（含地址快照与金额）。
func buildOrder(userID int64, orderNo string, address *usermodel.Address,
	snapshots []lineSnapshot, total, discount, pay int64, coupon *couponmodel.UserCouponView) (*model.Order, []model.OrderItem) {

	order := &model.Order{
		OrderNo:        orderNo,
		UserID:         userID,
		OrderType:      model.OrderTypeNormal,
		Status:         model.OrderStatusPendingPayment,
		TotalAmount:    total,
		DiscountAmount: discount,
		PayAmount:      pay,
		Receiver:       address.Receiver,
		Phone:          address.Phone,
		Province:       address.Province,
		City:           address.City,
		District:       address.District,
		Detail:         address.Detail,
		ExpireAt:       time.Now().Add(normalExpire),
	}
	if coupon != nil {
		order.CouponID = &coupon.ID
	}

	items := make([]model.OrderItem, 0, len(snapshots))
	for _, sn := range snapshots {
		items = append(items, model.OrderItem{
			OrderNo:   orderNo,
			SKUID:     sn.skuID,
			ProductID: sn.productID,
			Title:     sn.title,
			Specs:     sn.specs,
			Price:     sn.price,
			Quantity:  sn.quantity,
			Subtotal:  sn.price * int64(sn.quantity),
		})
	}
	return order, items
}

// ---- 查询 ----

// List 我的订单：状态筛选（空 = 全部）+ 分页；订单项随列表一次取出。
func (s *orderService) List(ctx context.Context, userID int64, status string, page, pageSize int) ([]model.OrderView, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if status != "" && !validStatus(status) {
		return nil, 0, fmt.Errorf("%w: invalid status", ErrInvalidInput)
	}

	orders, total, err := s.store.Orders.List(ctx, userID, status, (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, 0, err
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return nil, 0, err
	}

	views := make([]model.OrderView, 0, len(orders))
	for _, o := range orders {
		views = append(views, model.OrderView{Order: o, Items: itemsByOrder[o.OrderNo]})
	}
	return views, total, nil
}

// ListAll 后台全量订单（T25）：跨用户，状态筛选 + 分页；与 List 同构。
func (s *orderService) ListAll(ctx context.Context, status string, page, pageSize int) ([]model.OrderView, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if status != "" && !validStatus(status) {
		return nil, 0, fmt.Errorf("%w: invalid status", ErrInvalidInput)
	}

	orders, total, err := s.store.Orders.ListAll(ctx, status, (page-1)*pageSize, pageSize)
	if err != nil {
		return nil, 0, err
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return nil, 0, err
	}

	views := make([]model.OrderView, 0, len(orders))
	for _, o := range orders {
		views = append(views, model.OrderView{Order: o, Items: itemsByOrder[o.OrderNo]})
	}
	return views, total, nil
}

// GetDetail 订单详情（owner 校验）。
func (s *orderService) GetDetail(ctx context.Context, userID int64, orderNo string) (*model.OrderView, error) {
	return s.loadOwned(ctx, userID, orderNo)
}

// ---- 生命周期 ----

// Cancel 取消待支付订单：事务内 条件更新 待支付→已取消 + 回补库存 + 回退券。
// 重复取消（或状态已变）由条件更新 RowsAffected=0 兜底，不重复回补。
// 秒杀订单拒绝用户主动取消（其取消路径为超时取消，见 CancelExpiredSeckill——
// 回补活动库存 + Redis 库存 + 用户计数；禁止走普通订单的 SKU 回补以免错补库存）。
func (s *orderService) Cancel(ctx context.Context, userID int64, orderNo string) error {
	view, err := s.loadOwned(ctx, userID, orderNo)
	if err != nil {
		return err
	}
	if view.OrderType == model.OrderTypeSeckill {
		return fmt.Errorf("%w: seckill cancel not supported yet", ErrIllegalTransition)
	}
	if !model.CanTransition(view.Status, model.OrderStatusCancelled) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, view.Status, model.OrderStatusCancelled)
	}

	err = s.store.Tx.WithinTx(ctx, func(tx *gorm.DB) error {
		return s.cancelInTx(ctx, tx, view)
	})
	if err != nil {
		return err
	}
	// 状态流转打点（T19c）：事务提交后计数（回补失败回滚不计数）。
	s.metrics.OrderStatusChanged(model.OrderStatusCancelled)
	return nil
}

// CancelExpired 超时取消（cron 每分钟扫描调用）：
//  1. 扫描待支付且已过 expire_at 的普通订单（分批上限 cancelExpiredBatch）
//  2. 逐个事务内取消：条件更新 待支付→已取消 → 回补库存 → 回退券
//
// 单订单失败不阻断整轮：跳过并继续其余订单，失败订单停留待支付、下个 tick
// 重试（at-least-once），失败数供调用方记录日志。并发下已被支付/取消
// （ErrOrderChanged）属正常跳过；其余失败（如券状态异常）同样计入失败数——
// 孤立异常订单不应阻塞全部超时取消。扫描/批量读取等系统性错误向上传播
// （cron 记录日志，下个 tick 重试）。
func (s *orderService) CancelExpired(ctx context.Context) (cancelled, failed int, err error) {
	orders, err := s.store.Orders.ListExpiredPending(ctx, time.Now(), cancelExpiredBatch)
	if err != nil {
		return 0, 0, err
	}
	if len(orders) == 0 {
		return 0, 0, nil
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return 0, 0, err
	}

	for _, o := range orders {
		view := &model.OrderView{Order: o, Items: itemsByOrder[o.OrderNo]}
		err := s.store.Tx.WithinTx(ctx, func(tx *gorm.DB) error {
			return s.cancelInTx(ctx, tx, view)
		})
		if err != nil {
			failed++
			continue
		}
		cancelled++
		// 状态流转打点（T19c）：事务提交后计数。
		s.metrics.OrderStatusChanged(model.OrderStatusCancelled)
	}
	return cancelled, failed, nil
}

// CancelExpiredSeckill 秒杀订单超时取消（cron 每分钟扫描调用，T13）：
//  1. 扫描待支付、秒杀且已过 expire_at 的订单（分批上限 cancelExpiredBatch）
//  2. 逐个事务内取消：条件更新 待支付→已取消 → 回补活动库存（MySQL，
//     SeckillRestore.RestoreStock——订单存在即活动存在，外键 RESTRICT 保证）
//  3. 事务提交后回补 Redis：库存 INCR + 用户计数 DECR + 释放幂等键
//     （SeckillRestore.RestoreRedis，允许再次抢购）
//
// 单订单失败不阻断整轮（与普通订单 CancelExpired 同取舍）。DB 失败（含并发
// 已支付/已取消）计入 failed，下个 tick 重试或自然跳过；Redis 回补失败计入
// redisFailed 但不重试——订单已落取消态，重试将命中状态已变，且库存差额由
// 对账 cron（进行中告警 + 收尾对齐）兜底。
func (s *orderService) CancelExpiredSeckill(ctx context.Context) (cancelled, failed, redisFailed int, err error) {
	orders, err := s.store.Orders.ListExpiredSeckillPending(ctx, time.Now(), cancelExpiredBatch)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(orders) == 0 {
		return 0, 0, 0, nil
	}
	orderNos := make([]string, 0, len(orders))
	for _, o := range orders {
		orderNos = append(orderNos, o.OrderNo)
	}
	itemsByOrder, err := s.store.Items.ListByOrders(ctx, orderNos)
	if err != nil {
		return 0, 0, 0, err
	}

	for _, o := range orders {
		quantity := sumItemQuantity(itemsByOrder[o.OrderNo])
		// 秒杀订单必有 activity_id 与订单项（CreateSeckill 保证）；数据异常
		// 跳过不取消（避免错补），计失败供日志排查。
		if o.ActivityID == nil || quantity < 1 {
			failed++
			continue
		}
		dbErr := s.store.Tx.WithinTx(ctx, func(tx *gorm.DB) error {
			ok, err := s.store.Orders.Cancel(ctx, tx, o.OrderNo)
			if err != nil {
				return err
			}
			if !ok {
				return ErrOrderChanged
			}
			return s.seckill.RestoreStock(ctx, tx, *o.ActivityID, quantity)
		})
		if dbErr != nil {
			failed++
			continue
		}
		cancelled++
		// 状态流转打点（T19c）：事务提交后计数（Redis 回补失败仍计——订单已落取消态）。
		s.metrics.OrderStatusChanged(model.OrderStatusCancelled)
		// 事务已提交：回补 Redis（best-effort；失败由对账 cron 兜底）。
		if err := s.seckill.RestoreRedis(ctx, *o.ActivityID, o.UserID, quantity); err != nil {
			redisFailed++
		}
	}
	return cancelled, failed, redisFailed, nil
}

// CountValidSeckill 活动的秒杀有效订单数（非取消），对账端口实现
// （flashsale ReconcileActive 以此解释 Redis/MySQL 库存差额）。
func (s *orderService) CountValidSeckill(ctx context.Context, activityID int64) (int, error) {
	return s.store.Orders.CountValidByActivity(ctx, activityID)
}

// sumItemQuantity 订单项数量合计（秒杀订单固定单条订单项 Quantity=1，
// 求和以数量维度正确回补，防未来多数量秒杀扩展）。
func sumItemQuantity(items []model.OrderItem) int {
	n := 0
	for _, it := range items {
		n += it.Quantity
	}
	return n
}

// cancelInTx 事务内取消：条件更新 待支付→已取消 + 回补库存 + 回退券；
// 用户取消与超时取消共用（库存/券补偿逻辑单点维护）。
func (s *orderService) cancelInTx(ctx context.Context, tx *gorm.DB, view *model.OrderView) error {
	ok, err := s.store.Orders.Cancel(ctx, tx, view.OrderNo)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOrderChanged
	}
	for _, it := range view.Items {
		if err := s.products.RestoreStock(ctx, tx, it.SKUID, it.Quantity); err != nil {
			return translateProductError(err)
		}
	}
	if view.CouponID != nil {
		if err := s.coupons.RollbackCoupon(ctx, tx, view.UserID, *view.CouponID); err != nil {
			return translateCouponError(err)
		}
	}
	return nil
}

// MarkPaid 支付成功状态迁移：待支付 → 已支付（事务由支付模块开启）。
// 状态机（仅 待支付→已支付）与金额核对（pay_amount = 回调金额）由条件更新
// WHERE 原子兜底；false 表示状态已变或金额不符，由支付模块区分错误。
func (s *orderService) MarkPaid(ctx context.Context, tx *gorm.DB, orderNo string, payAmount int64) (bool, error) {
	ok, err := s.store.Orders.MarkPaid(ctx, tx, orderNo, payAmount)
	if ok {
		s.metrics.OrderStatusChanged(model.OrderStatusPaid)
	}
	return ok, err
}

// Ship 后台发货：已支付 → 已发货（admin；发货不校验归属）。
func (s *orderService) Ship(ctx context.Context, orderNo string) error {
	order, err := s.store.Orders.GetByNo(ctx, orderNo)
	if err != nil {
		return err
	}
	if order == nil {
		return ErrOrderNotFound
	}
	if !model.CanTransition(order.Status, model.OrderStatusShipped) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, order.Status, model.OrderStatusShipped)
	}
	ok, err := s.store.Orders.Ship(ctx, nil, orderNo)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOrderChanged
	}
	s.metrics.OrderStatusChanged(model.OrderStatusShipped)
	return nil
}

// ConfirmReceipt 确认收货：已发货 → 已完成（owner 校验）。
func (s *orderService) ConfirmReceipt(ctx context.Context, userID int64, orderNo string) error {
	view, err := s.loadOwned(ctx, userID, orderNo)
	if err != nil {
		return err
	}
	if !model.CanTransition(view.Status, model.OrderStatusCompleted) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, view.Status, model.OrderStatusCompleted)
	}
	ok, err := s.store.Orders.ConfirmReceipt(ctx, nil, orderNo)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOrderChanged
	}
	s.metrics.OrderStatusChanged(model.OrderStatusCompleted)
	return nil
}

// HasPurchasedSKU 好友圈分享校验：存在 已支付/已发货/已完成 订单含该 SKU。
func (s *orderService) HasPurchasedSKU(ctx context.Context, userID, skuID int64) (bool, error) {
	return s.store.Items.HasPurchased(ctx, userID, skuID)
}

// ---- 内部 ----

// loadOwned 读取订单 + 订单项并校验归属（防 IDOR）。
func (s *orderService) loadOwned(ctx context.Context, userID int64, orderNo string) (*model.OrderView, error) {
	view, err := s.loadView(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, ErrOrderNotFound
	}
	if view.UserID != userID {
		return nil, ErrOrderForbidden
	}
	return view, nil
}

// loadView 读取订单 + 订单项；不存在返回 (nil, nil)。
func (s *orderService) loadView(ctx context.Context, orderNo string) (*model.OrderView, error) {
	order, err := s.store.Orders.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, nil
	}
	items, err := s.store.Items.ListByOrder(ctx, orderNo)
	if err != nil {
		return nil, err
	}
	return &model.OrderView{Order: *order, Items: items}, nil
}

func validStatus(status string) bool {
	switch status {
	case model.OrderStatusPendingPayment, model.OrderStatusPaid, model.OrderStatusShipped,
		model.OrderStatusCompleted, model.OrderStatusCancelled:
		return true
	}
	return false
}

// isValidationError 校验类业务错误：客户端修正输入后重试即可；
// 其余（基础设施/未知）错误保留幂等键，防重复下单。
func isValidationError(err error) bool {
	switch {
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrCartEmpty),
		errors.Is(err, ErrInsufficientStock), errors.Is(err, ErrSKUNotFound),
		errors.Is(err, ErrSKUUnavailable), errors.Is(err, ErrCouponNotFound),
		errors.Is(err, ErrCouponUsed), errors.Is(err, ErrCouponExpired),
		errors.Is(err, ErrCouponThresholdNotMet), errors.Is(err, ErrAddressNotFound),
		errors.Is(err, ErrAddressForbidden):
		return true
	}
	return false
}
