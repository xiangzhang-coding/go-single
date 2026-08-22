package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	carthandler "github.com/xiangzhang-coding/go-single/internal/cart/handler"
	cartrepo "github.com/xiangzhang-coding/go-single/internal/cart/repository"
	cartsvc "github.com/xiangzhang-coding/go-single/internal/cart/service"
	chathandler "github.com/xiangzhang-coding/go-single/internal/chat/handler"
	chatrepo "github.com/xiangzhang-coding/go-single/internal/chat/repository"
	chatsvc "github.com/xiangzhang-coding/go-single/internal/chat/service"
	couponhandler "github.com/xiangzhang-coding/go-single/internal/coupon/handler"
	couponrepo "github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	flashsalehandler "github.com/xiangzhang-coding/go-single/internal/flashsale/handler"
	flashsalerepo "github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	orderhandler "github.com/xiangzhang-coding/go-single/internal/order/handler"
	orderrepo "github.com/xiangzhang-coding/go-single/internal/order/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	paymenthandler "github.com/xiangzhang-coding/go-single/internal/payment/handler"
	paymentrepo "github.com/xiangzhang-coding/go-single/internal/payment/repository"
	paymentsvc "github.com/xiangzhang-coding/go-single/internal/payment/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	platformcors "github.com/xiangzhang-coding/go-single/internal/platform/cors"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/health"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	"github.com/xiangzhang-coding/go-single/internal/platform/requestbody"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	"github.com/xiangzhang-coding/go-single/internal/platform/snowflake"
	"github.com/xiangzhang-coding/go-single/internal/platform/ws"
	producthandler "github.com/xiangzhang-coding/go-single/internal/product/handler"
	productrepo "github.com/xiangzhang-coding/go-single/internal/product/repository"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
	socialhandler "github.com/xiangzhang-coding/go-single/internal/social/handler"
	socialrepo "github.com/xiangzhang-coding/go-single/internal/social/repository"
	socialsvc "github.com/xiangzhang-coding/go-single/internal/social/service"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

func newRouter(cfg *config.Config, log *zap.Logger, db *gorm.DB, sqlDB *sql.DB, cacheClient cache.Client, mqClient mq.MQ, fileSvc *file.MinIO, wsHub *ws.Hub) (*gin.Engine, *applicationRuntime, error) {
	if cfg.Server.Mode != gin.DebugMode && cfg.Server.Mode != gin.ReleaseMode && cfg.Server.Mode != gin.TestMode {
		return nil, nil, fmt.Errorf("invalid Gin mode %q", cfg.Server.Mode)
	}
	gin.SetMode(cfg.Server.Mode)

	r := gin.New()
	configureHTTPErrorResponses(r)
	// 可信反代白名单（安全收尾）：命中才采信 X-Forwarded-For/X-Real-IP，
	// 还原 Nginx 反代后的真实客户端 IP（requestLogger 的 client_ip 与指标维度）。
	configureTrustedProxies(r, cfg.Server.TrustedProxies, log)
	// metrics 在最外层：panic 被 Recovery 恢复为 500 后仍能完成计数。
	metricRegistry := metrics.New()
	// 业务指标（T19c）：秒杀预扣/库存余量、订单创建/状态/支付、MQ 发布消费、
	// 优惠券发放核销；注册于同一 registry，随 /metrics 抓取。
	businessMetrics := metricRegistry.Business()
	// MQ 客户端装饰：发布/消费/消费失败打点（queue 维度，与业务指标同 registry）。
	mqClient = metrics.WrapMQ(mqClient, businessMetrics)
	// MQ 消费者熔断（T20，gobreaker）：连续失败打开 → 消息快速失败（不触下游），
	// 冷却后半开探活、恢复即闭合；仅包消费者——Publish 直通（发布失败由
	// 幂等操作的有限重试 + 对账兜底），进程内调用与本地 Redis/MySQL 不包。
	mqClient = mq.WrapCircuitBreaker(mqClient, mq.BreakerSettings{
		Name:                   "mq-consumer",
		MaxConsecutiveFailures: cfg.MQ.Circuit.MaxConsecutiveFailures,
		Interval:               cfg.MQ.Circuit.Interval,
		Timeout:                cfg.MQ.Circuit.Timeout,
	})
	// CORS（T26）：跨源场景（云端前端独立部署）按配置白名单放行；
	// 置于 requestLogger 之后，使预检 OPTIONS 也进入访问日志（排障可见），
	// 同时仍先于路由匹配（Use 中间件均在 handler 前执行）。
	r.Use(metricRegistry.GinMiddleware(), safeRecovery(log), requestLogger(log), platformcors.Middleware(cfg.CORS.AllowOrigins))

	r.GET("/metrics", gin.WrapH(metricRegistry.Handler()))

	checker := &health.Checker{
		MySQL:   sqlDB,
		Cache:   cacheClient,
		MQ:      mqClient,
		Timeout: 2 * time.Second,
	}
	r.GET("/healthz", healthHandler(checker))

	// user 模块：注册/登录/鉴权 + 地址簿（默认地址唯一由 users.default_address_id 指针保证）。
	verifier := auth.NewJWT(auth.JWTConfig{Secret: cfg.Auth.Secret, TTL: cfg.Auth.TTL})
	userSvc := usersvc.NewWithMedia(userrepo.Store{Users: userrepo.NewGORM(db), Addresses: userrepo.NewGORMAddress(db)}, verifier, fileSvc)
	authLimits, err := limiter.NewAuthAttempts(cacheClient, userSvc, limiter.AuthAttemptsConfig{
		Login: limiter.AuthAttemptConfig{
			PerIPMax: cfg.Auth.LoginRateLimit.PerIPMax, PerAccountMax: cfg.Auth.LoginRateLimit.PerAccountMax,
			Window: cfg.Auth.LoginRateLimit.Window,
		},
		Register: limiter.AuthAttemptConfig{
			PerIPMax: cfg.Auth.RegisterRateLimit.PerIPMax, PerAccountMax: cfg.Auth.RegisterRateLimit.PerAccountMax,
			Window: cfg.Auth.RegisterRateLimit.Window,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("初始化认证限流: %w", err)
	}
	userHandler := userhandler.New(userSvc, verifier, authLimits)
	addressHandler := userhandler.NewAddress(userSvc, verifier)

	// product 模块：admin 维护类目/商品/SKU，游客浏览（详情走缓存）。
	productRepo := productrepo.NewGORMProduct(db)
	productSvc := productsvc.New(productrepo.Store{Category: productrepo.NewGORMCategory(db), Product: productRepo, SKU: productrepo.NewGORMSKU(db)}, cacheClient, log)
	productHandler := producthandler.New(productSvc, verifier)

	// cart 模块：加购校验 SKU 存在/上架（跨模块经 product 服务接口），列表拼装展示快照。
	cartSvc := cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(db)}, productSvc)
	cartHandler := carthandler.New(cartSvc, verifier)

	// coupon 模块：admin 发布券模板，用户领券（Lua 原子防超发）与我的券。
	couponSvc := couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(db), UserCoupon: couponrepo.NewGORMUserCoupon(db)}, cacheClient, businessMetrics)
	couponHandler := couponhandler.New(couponSvc, verifier)

	// 雪花订单号生成器：普通下单与秒杀抢购共用同一实例（worker_id 单实例唯一）。
	orderNoGen, err := snowflake.New(cfg.Snowflake.WorkerID)
	if err != nil {
		return nil, nil, fmt.Errorf("初始化雪花订单号生成器: %w", err)
	}

	// 幂等操作有限重试配置（T20）：仅下单/支付回调/秒杀消息发布启用，
	// 非幂等操作不重试；与全链路超时（requestTimeout 中间件）配合容错。
	retryCfg := retry.Config{
		Attempts:       cfg.Retry.Attempts,
		InitialBackoff: cfg.Retry.InitialBackoff,
		MaxBackoff:     cfg.Retry.MaxBackoff,
	}

	// flashsale 模块：admin 秒杀活动管理（创建/编辑/上架/下架），
	// 上架预热库存进 Redis（未开始可覆盖、进行中只减不增）；SKU 校验经 product 服务接口。
	// 抢购（T11/T12）：全局令牌桶限流中间件 + 按用户 Redis 计数限流 + 幂等键 +
	// Lua 原子预扣 + 生成雪花订单号发 MQ（异步落单），成功返回 202 排队中 + order_no。
	seckillTokenBucket, err := limiter.NewTokenBucket(limiter.TokenBucketConfig{
		QPS:   cfg.FlashSale.QPS,
		Burst: cfg.FlashSale.Burst,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("初始化秒杀令牌桶限流: %w", err)
	}
	flashsaleActivityStore := flashsalerepo.NewGORMActivity(db)
	flashsalePreDeductions := flashsalerepo.NewGORMPreDeduction(db)
	flashsaleStore := flashsalerepo.Store{
		Activities: flashsaleActivityStore, PreDeductions: flashsalePreDeductions, Tx: flashsaleActivityStore,
	}
	flashsaleSvc := flashsalesvc.New(flashsaleStore, productSvc, cacheClient,
		limiter.RedisCounterConfig{Max: cfg.FlashSale.PerUserMax, Window: cfg.FlashSale.PerUserWindow},
		mqClient, orderNoGen, businessMetrics, retryCfg)
	flashsaleHandler := flashsalehandler.New(flashsaleSvc, verifier)

	// order 模块：购物车结算/直购下单（单事务：订单+订单项+库存+地址快照+券核销+
	// 删除购物车条目）、client_request_id 幂等（Redis SETNX）、雪花订单号、
	// 订单列表/详情、取消（回补库存+回退券）、确认收货与 admin 发货；
	// 秒杀事务编排由 flashsale 模块单向调用 order 的事务内能力，order 不持有
	// flashsale 实例，避免运行时依赖环。
	orderStore := orderrepo.NewGORMOrder(db)
	orderSvc := ordersvc.New(orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(db), Tx: orderStore},
		cacheClient, orderNoGen, productSvc, couponSvc, cartSvc, userSvc, businessMetrics, retryCfg)
	reservationCleanup := flashsalesvc.NewReservationCleanup(
		flashsaleStore.Activities, flashsaleStore.PreDeductions, cacheClient, orderSvc)
	seckillCancellation := flashsalesvc.NewSeckillCancellation(
		flashsaleStore.Tx, orderSvc, flashsaleStore.Activities, flashsaleStore.PreDeductions,
		flashsaleSvc, businessMetrics)
	orderHandler := orderhandler.New(orderSvc, verifier, orderCancellationCoordinator{orders: orderSvc, seckill: seckillCancellation})

	// 秒杀库存对账（T13）：进行中只比对告警（补单信号），收尾以 MySQL 对齐 Redis；
	// 有效订单数经 order 服务端口统计（进程内调用，flashsale → order 单向依赖）。
	reconcile := flashsalesvc.NewReconciliation(flashsaleStore, cacheClient, orderSvc)

	// social 模块：好友申请/通过/拒绝与好友列表；用户名经 userSvc 跨模块进程内调用补齐。
	// 好友圈动态：分享购买校验经 orderSvc（已支付/已发货/已完成订单含该 SKU）。
	socialStore := socialrepo.Store{
		Requests: socialrepo.NewGORMRequest(db), Friendships: socialrepo.NewGORMFriendship(db),
		Posts: socialrepo.NewGORMPost(db), Tx: socialrepo.NewGORMTx(db),
	}
	socialSvc := socialsvc.New(socialStore, userSvc)
	postSvc := socialsvc.NewPostsWithMedia(socialStore, userSvc, orderSvc, fileSvc)
	socialHandler := socialhandler.New(socialSvc, postSvc, verifier)

	// chat 模块（T17 REST 通道）：发送消息（text/image/file，client_request_id 幂等）、
	// 会话列表（最近消息 + 未读数）与消息列表（游标分页）、已读推进（离线消息上线可拉取）；
	// 仅好友可单聊（跨模块经 socialSvc.AreFriends）、仅会话双方可访问（owner 校验）。
	// T18 实时通道：落库成功经 wsMessageNotifier 推送给在线接收方（断线不丢由落库+REST 兜底）。
	chatConversationRepo := chatrepo.NewGORMConversation(db)
	chatStore := chatrepo.Store{
		Conversations: chatConversationRepo,
		Messages:      chatrepo.NewGORMMessage(db),
		Reads:         chatrepo.NewGORMReadState(db),
		Tx:            chatConversationRepo,
	}
	chatSvc := chatsvc.NewWithMedia(chatStore, userSvc, socialSvc, wsMessageNotifier{hub: wsHub}, fileSvc)
	chatHandler := chathandler.New(chatSvc, verifier)

	// T12 秒杀异步落单消费者：订阅"抢购成功"消息 → 查活动 → order 准备地址/商品快照 →
	// order 服务幂等建单 + 同事务扣活动库存；瞬时失败 Nack 重投、
	// 永久失败进死信（对账兜底）。消费中断 3s 后重连（at-least-once 不丢消息）。
	seckillConsumer := flashsalesvc.NewSeckillOrderConsumer(
		flashsaleStore.Activities, flashsaleStore.PreDeductions, cacheClient, flashsaleStore.Tx,
		orderSvc, businessMetrics, log)

	// payment 模块：模拟支付回调（成功/失败），流水唯一约束（payment_id）挡重复回调，
	// 成功路径单事务（流水 + 订单 待支付→已支付，WHERE 校验状态机、金额与期限）；
	// 订单读取与状态迁移经 order 服务进程内调用。
	paymentStore := paymentrepo.NewGORMPayment(db)
	paymentHandler := paymenthandler.New(
		paymentsvc.New(paymentrepo.Store{Payments: paymentStore, Tx: paymentStore}, orderSvc, businessMetrics, retryCfg),
		verifier,
	)

	// cron 定时任务：平台级注册机制（platform/cron），单实例调度；
	// T09 订单超时取消——每分钟扫描已过 expire_at 的待支付普通订单，
	// 事务内取消 + 回补库存 + 回退券；单订单失败跳过（失败数记日志，下个 tick 重试）。
	// T13 秒杀超时取消——每分钟扫描待支付秒杀订单，回补活动库存 + Redis 库存 +
	// 用户计数（允许再次抢购）；对账——每小时进行中只比对告警（补单信号）、
	// 每分钟收尾以 MySQL 对齐刚结束活动的 Redis 库存。
	cronRegistry, cronJobs, err := registerCron(log, orderSvc, flashsaleSvc, flashsaleSvc, reservationCleanup, fileSvc, seckillCancellation, reconcile)
	if err != nil {
		return nil, nil, err
	}

	// file 基础设施：统一文件上传代理（类型白名单 + ≤5MB + MinIO 私有桶）。
	fileHandler := file.NewHandler(fileSvc, verifier, mediaAccessAuthorizer{users: userSvc, posts: postSvc, chat: chatSvc}, file.UploadConcurrencyConfig{
		MaxConcurrent: cfg.Upload.MaxConcurrent, MaxConcurrentPerUser: cfg.Upload.MaxConcurrentPerUser,
		MaxConcurrentPerIP: cfg.Upload.MaxConcurrentPerIP,
	})

	// WebSocket 实时通道：JWT 经 Sec-WebSocket-Protocol 携带，不进入请求 URL。
	r.GET("/ws", wsHub.Handler(verifier))

	// 业务 API 组：挂全链路请求超时（T20），超时快速失败（504）；
	// /ws 长连接不适用请求超时（连接生命周期自行管理），/metrics、/healthz
	// 为本地快速检查不挂超时（healthz 自带 2s 内部超时）。
	api := r.Group("/api")
	jsonLimit, err := requestbody.LimitJSON(cfg.Server.MaxJSONBodyBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("初始化 JSON 请求体上限: %w", err)
	}
	api.Use(requestTimeout(cfg.Server.RequestTimeout), jsonLimit)
	userHandler.RegisterRoutes(api)
	addressHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
	cartHandler.RegisterRoutes(api)
	couponHandler.RegisterRoutes(api)
	flashsaleHandler.RegisterRoutes(api, seckillTokenBucket)
	socialHandler.RegisterRoutes(api)
	chatHandler.RegisterRoutes(api)
	orderHandler.RegisterRoutes(api)
	paymentHandler.RegisterRoutes(api)

	// Multipart 上传使用独立的 21 MiB 解析前预算，不能经过 64 KiB JSON 路由组。
	fileAPI := r.Group("/api")
	fileAPI.Use(requestTimeout(cfg.Upload.RequestTimeout))
	fileHandler.RegisterRoutes(fileAPI)

	background := &applicationRuntime{
		log: log, mq: mqClient, cron: cronRegistry, cronJobs: cronJobs,
		recovery: flashsaleSvc, recoveryGate: flashsaleSvc, reservationCleanup: reservationCleanup,
		consumers: []consumerBinding{
			{queue: flashsalesvc.SeckillOrderQueue, name: "秒杀落单消费者", handler: seckillConsumer.Handle},
			{queue: flashsalesvc.SeckillOrderDeadLetterQueue, name: "秒杀死信消费者", handler: seckillConsumer.HandleDeadLetter},
		},
	}
	return r, background, nil
}

func healthHandler(checker *health.Checker) gin.HandlerFunc {
	return func(c *gin.Context) {
		res := checker.Check(c.Request.Context())
		code := http.StatusOK
		if res.Status != "ok" {
			code = http.StatusServiceUnavailable
		}
		c.JSON(code, res)
	}
}
