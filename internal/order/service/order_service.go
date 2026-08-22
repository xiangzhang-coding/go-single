// Package service 承载 order 模块业务：下单（购物车结算/直购）、订单列表与详情、
// 取消待支付订单、确认收货与后台发货，状态机非法跃迁一律拒绝。
//
// 下单为单事务（订单 + 订单项 + 库存条件更新 stock>=N + 地址快照 + 券核销 +
// 删除购物车条目），事务由 order 仓储开启，跨模块写操作经各模块 service 的
// tx 参数汇入同一事务；client_request_id 以 MySQL 唯一事实保证持久幂等，
// Redis SETNX + TTL 15min 协调在途请求。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	cartmodel "github.com/xiangzhang-coding/go-single/internal/cart/model"
	couponmodel "github.com/xiangzhang-coding/go-single/internal/coupon/model"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"

	"github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/order/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrInvalidInput                = errors.New("invalid input")
	ErrOrderNotFound               = errors.New("order not found")
	ErrOrderForbidden              = errors.New("order does not belong to user")
	ErrOrderChanged                = errors.New("order status changed, retry")
	ErrIllegalTransition           = errors.New("illegal order status transition")
	ErrCartEmpty                   = errors.New("cart is empty")
	ErrInsufficientStock           = errors.New("insufficient stock")
	ErrSKUNotFound                 = errors.New("sku not found")
	ErrSKUUnavailable              = errors.New("sku product is not on sale")
	ErrSeckillOrderConflict        = errors.New("seckill order conflicts with an existing purchase")
	ErrCouponNotFound              = errors.New("coupon not found")
	ErrCouponUsed                  = errors.New("coupon already used")
	ErrCouponExpired               = errors.New("coupon not in valid period")
	ErrCouponThresholdNotMet       = errors.New("coupon threshold not met")
	ErrAddressNotFound             = errors.New("address not found")
	ErrAddressForbidden            = errors.New("address does not belong to user")
	ErrSeckillCancellationRequired = errors.New("seckill cancellation requires flashsale compensation")
	errCartChangedWhileFencing     = errors.New("cart changed while fencing product details")
)

// 超时取消：普通订单默认 15min（expire_at 写入订单，T09 定时任务扫描）；
// 秒杀订单 10min（T12 落单时写入；取消回补见后续对账/取消任务）。
const (
	normalExpire  = 15 * time.Minute
	seckillExpire = 10 * time.Minute
	// 幂等键 TTL 仅协调在途请求；过期后仍由数据库请求身份返回原订单。
	idemTTL = 15 * time.Minute
	// 分页上限与默认页大小。
	defaultPageSize = 20
	maxPageSize     = 50
	// 超时扫描每 tick 处理上限：余量下个 tick 续扫（cron 每分钟一次）。
	cancelExpiredBatch          = 500
	productDetailCleanupTimeout = 3 * time.Second
)

// 幂等键约定：order:idem:{user_id}:{client_request_id} → 订单号。
func idemKey(userID int64, clientRequestID string) string {
	return fmt.Sprintf("order:idem:%d:%s", userID, clientRequestID)
}

// ---- 跨模块最小接口（进程内调用，面向接口非 HTTP；实现见 main 装配） ----

// ProductService 商品模块最小接口：下单读取 SKU/上架校验，
// 事务内条件扣减与回补库存（tx 由 order 模块开启）。
type ProductService interface {
	GetSKU(ctx context.Context, id int64) (*productmodel.SKU, error)
	GetProduct(ctx context.Context, id int64) (*productmodel.Product, error)
	GetSKUForUpdate(ctx context.Context, tx *transaction.Handle, id int64) (*productmodel.SKU, error)
	GetDetail(ctx context.Context, id int64) (*productmodel.ProductDetail, error)
	DeductStock(ctx context.Context, tx *transaction.Handle, skuID int64, quantity int) (bool, error)
	RestoreStock(ctx context.Context, tx *transaction.Handle, skuID int64, quantity int) error
	BeginDetailMutation(ctx context.Context, productID int64) (string, error)
	FinishDetailMutation(ctx context.Context, productID int64, token string)
}

// CouponService is the order module's complete coupon capability: checkout
// redeems from the same transaction snapshot, while cancellation rolls it back.
type CouponService interface {
	RedeemForOrder(ctx context.Context, tx *transaction.Handle, userID, couponID, totalAmount int64) (couponmodel.CouponRedemption, error)
	RollbackCoupon(ctx context.Context, tx *transaction.Handle, userID, couponID int64) error
}

// CartService 购物车模块最小接口：在订单事务内锁定并读取当前条目，
// 再按条目 ID 删除已购行。
type CartService interface {
	ListItems(ctx context.Context, userID int64) ([]cartmodel.CartItemView, error)
	LockItems(ctx context.Context, tx *transaction.Handle, userID int64) ([]cartmodel.CartItem, error)
	DeletePurchased(ctx context.Context, tx *transaction.Handle, userID int64, itemIDs []int64) error
}

// UserService 用户模块最小接口：读取地址簿固化为地址快照（owner 校验）。
type UserService interface {
	GetAddress(ctx context.Context, userID, id int64) (*usermodel.Address, error)
	GetDefaultAddress(ctx context.Context, userID int64) (*usermodel.Address, error)
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
// OrderNo 预扣时已生成（雪花）；地址与商品信息来自预先准备的快照。
type SeckillCreateParams struct {
	OrderNo      string
	UserID       int64
	ActivityID   int64
	PurchaseSlot int64
	SKUID        int64
	Price        int64 // 秒杀价快照（活动价格）
	Quantity     int
	Snapshot     *SeckillOrderSnapshot
}

// SeckillOrderSnapshot 是异步落单前由 order 模块准备的地址与商品快照。
// 商品读取不应用公开详情的上架过滤，因为预扣接受后商品可能被下架。
type SeckillOrderSnapshot struct {
	SKUID        int64
	ProductID    int64
	ProductTitle string
	Specs        json.RawMessage
	Address      usermodel.Address
}

// SeckillCancellationOrder 是秒杀取消编排所需的最小订单快照。
// order 模块负责从订单与订单项聚合该数据，flashsale 模块负责活动库存回补。
type SeckillCancellationOrder struct {
	OrderNo      string
	UserID       int64
	ActivityID   int64
	PurchaseSlot int64
	SKUID        int64
	Price        int64
	Quantity     int
}

// Service order 模块的业务接口。
type Service interface {
	// Create 下单（购物车结算 / 直购）：单事务创建订单；client_request_id
	// 重复提交返回同一订单号（幂等）。
	Create(ctx context.Context, userID int64, p CreateParams) (*CreateResult, error)
	// CreateSeckillInTx 在调用方事务内创建秒杀订单与订单项。返回 created=false
	// 表示唯一约束命中（MQ 重投/并发消费），flashsale 编排不得再次扣减
	// 活动库存；user_activity_key 在取消时置 NULL，允许再次抢购。
	CreateSeckillInTx(ctx context.Context, tx *transaction.Handle, p SeckillCreateParams) (created bool, err error)
	// PrepareSeckillOrder 在事务外读取默认地址与内部商品/SKU 快照，供 flashsale
	// 消费者准备已接受预扣的落单输入。
	PrepareSeckillOrder(ctx context.Context, userID, skuID int64) (*SeckillOrderSnapshot, error)
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
	// ListExpiredSeckill 返回待支付且已超时的秒杀订单最小快照，供 flashsale
	// 模块在应用编排层完成订单取消与活动库存回补。
	ListExpiredSeckill(ctx context.Context) ([]SeckillCancellationOrder, error)
	// CancelSeckill 在调用方事务内条件取消一笔待支付秒杀订单。
	CancelSeckill(ctx context.Context, tx *transaction.Handle, orderNo string) (bool, error)
	// SeckillCancellation 返回归属当前用户的待支付秒杀订单补偿快照。
	SeckillCancellation(ctx context.Context, userID int64, orderNo string) (*SeckillCancellationOrder, error)
	// CountValidSeckill 活动的秒杀有效订单数（非取消），对账端口
	// （flashsale 模块 ReconcileActive 进程内调用）。
	CountValidSeckill(ctx context.Context, activityID int64) (int, error)
	// SeckillOrderStatus 为 flashsale 清理已无需补偿的 reservation marker
	// 提供最小只读状态；非秒杀或不存在返回 found=false。
	SeckillOrderStatus(ctx context.Context, orderNo string) (status string, found bool, err error)
	// MarkPaid 支付成功状态迁移：待支付 → 已支付（支付模块事务内条件更新；
	// WHERE 同时校验 status、pay_amount 与 expire_at）。
	MarkPaid(ctx context.Context, tx *transaction.Handle, orderNo string, payAmount int64) (bool, error)
	// CanRecordFailedPayment locks and rechecks that the order is still pending
	// and unexpired in the payment transaction.
	CanRecordFailedPayment(ctx context.Context, tx *transaction.Handle, orderNo string) (bool, error)
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
	subtotal  int64
}

type idempotencyCache interface {
	cache.IdempotencyStore
	Get(ctx context.Context, key string) (string, error)
	Del(ctx context.Context, key string) error
}

type orderService struct {
	store repository.Store
	cache idempotencyCache
	nos   OrderNoGenerator

	products ProductService
	coupons  CouponService
	cart     CartService
	users    UserService
	metrics  *metrics.Business
	retryCfg retry.Config // 幂等操作有限重试（T20）：仅普通下单 Create
}

// New 构造订单服务；retryCfg 为下单重试配置（T20 有限重试；省略 = 不重试）。
func New(store repository.Store, c idempotencyCache, nos OrderNoGenerator,
	products ProductService, coupons CouponService, cart CartService, users UserService,
	m *metrics.Business, retryCfg ...retry.Config) Service {
	cfg := retry.OrDefault(retryCfg...)
	return &orderService{store: store, cache: c, nos: nos, products: products, coupons: coupons, cart: cart, users: users, metrics: m, retryCfg: cfg}
}
