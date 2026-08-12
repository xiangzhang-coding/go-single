package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	carthandler "github.com/xiangzhang-coding/go-single/internal/cart/handler"
	cartrepo "github.com/xiangzhang-coding/go-single/internal/cart/repository"
	cartsvc "github.com/xiangzhang-coding/go-single/internal/cart/service"
	chathandler "github.com/xiangzhang-coding/go-single/internal/chat/handler"
	chatmodel "github.com/xiangzhang-coding/go-single/internal/chat/model"
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
	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/health"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/logger"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log, err := logger.New(cfg.Log.Level, cfg.Log.File)
	if err != nil {
		return err
	}
	defer log.Sync()

	// MySQL：连接 + 迁移。
	db, err := openMySQL(cfg)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	if err := runMigrations(cfg, log); err != nil {
		return err
	}

	// 依赖：Redis 缓存 + RabbitMQ 消息。
	cacheClient, err := cache.NewRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		return err
	}
	defer cacheClient.Close()

	mqClient, err := mq.NewRabbitMQ(cfg.MQ.URL)
	if err != nil {
		return err
	}
	defer mqClient.Close()

	// 文件上传：MinIO 私有桶 + 后端代理（前端不直连）。
	fileSvc, err := file.NewMinIO(file.MinIOConfig{
		Endpoint:  cfg.MinIO.Endpoint,
		AccessKey: cfg.MinIO.AccessKey,
		SecretKey: cfg.MinIO.SecretKey,
		Bucket:    cfg.MinIO.Bucket,
		UseSSL:    cfg.MinIO.UseSSL,
		PublicURL: cfg.MinIO.PublicURL,
	})
	if err != nil {
		return err
	}

	// WebSocket 实时通道（T18）：连接管理 + 心跳保活；握手经 query token 鉴权
	// （token 进访问日志的风险为演示取舍）；关闭在 HTTP 优雅关闭之后执行。
	wsHub := ws.New(ws.Config{
		HeartbeatInterval: cfg.WS.HeartbeatInterval,
		WriteWait:         cfg.WS.WriteWait,
		AllowOrigins:      cfg.WS.AllowOrigins,
	}, log)

	router, cronRegistry := newRouter(cfg, log, db, sqlDB, cacheClient, mqClient, fileSvc, wsHub)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	// WebSocket 连接经 hijack 脱离 http.Server 超时管理，需显式关闭（优雅关闭最后一步）。
	defer wsHub.Close()

	go func() {
		log.Info("HTTP 服务启动", zap.Int("port", cfg.Server.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常退出", zap.Error(err))
			os.Exit(1)
		}
	}()

	// 优雅关闭：等待 SIGINT / SIGTERM；先停 cron（等待执行中的任务），再关 HTTP。
	// 两段使用独立超时预算：cron 停止不应耗尽 HTTP 关闭的时间。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("收到退出信号，正在关闭")

	cronCtx, cronCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := cronRegistry.Stop(cronCtx); err != nil {
		log.Warn("cron 停止超时", zap.Error(err))
	}
	cronCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

func openMySQL(cfg *config.Config) (*gorm.DB, error) {
	gdb, err := gorm.Open(mysql.Open(cfg.MySQL.DSN()), &gorm.Config{
		// record not found 属正常分支（仓储已处理），仅记录 warn 及以上。
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL: %w", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 SQL 连接池: %w", err)
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("MySQL 连接失败: %w", err)
	}
	return gdb, nil
}

func runMigrations(cfg *config.Config, log *zap.Logger) error {
	m, err := migrate.New("file://"+cfg.Migrations.Path, "mysql://"+cfg.MySQL.DSN())
	if err != nil {
		return fmt.Errorf("初始化迁移: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("执行迁移: %w", err)
	}
	log.Info("数据库迁移完成")
	return nil
}

func newRouter(cfg *config.Config, log *zap.Logger, db *gorm.DB, sqlDB *sql.DB, cacheClient cache.Cache, mqClient mq.MQ, fileSvc *file.MinIO, wsHub *ws.Hub) (*gin.Engine, *platformcron.Registry) {
	gin.SetMode(cfg.Server.Mode)

	r := gin.New()
	// metrics 在最外层：panic 被 Recovery 恢复为 500 后仍能完成计数。
	metricRegistry := metrics.New()
	// CORS（T26）：跨源场景（云端前端独立部署）按配置白名单放行；
	// 置于 requestLogger 之后，使预检 OPTIONS 也进入访问日志（排障可见），
	// 同时仍先于路由匹配（Use 中间件均在 handler 前执行）。
	r.Use(metricRegistry.GinMiddleware(), gin.Recovery(), requestLogger(log), platformcors.Middleware(cfg.CORS.AllowOrigins))

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
	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(db), Addresses: userrepo.NewGORMAddress(db)}, verifier)
	userHandler := userhandler.New(userSvc, verifier)
	addressHandler := userhandler.NewAddress(userSvc, verifier)

	// product 模块：admin 维护类目/商品/SKU，游客浏览（详情走缓存）。
	productRepo := productrepo.NewGORMProduct(db)
	productSvc := productsvc.New(productrepo.Store{Category: productrepo.NewGORMCategory(db), Product: productRepo, SKU: productrepo.NewGORMSKU(db)}, cacheClient)
	productHandler := producthandler.New(productSvc, verifier)

	// cart 模块：加购校验 SKU 存在/上架（跨模块经 product 服务接口），列表拼装展示快照。
	cartSvc := cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(db)}, productSvc)
	cartHandler := carthandler.New(cartSvc, verifier)

	// coupon 模块：admin 发布券模板，用户领券（Lua 原子防超发）与我的券。
	couponSvc := couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(db), UserCoupon: couponrepo.NewGORMUserCoupon(db)}, cacheClient)
	couponHandler := couponhandler.New(couponSvc, verifier)

	// 雪花订单号生成器：普通下单与秒杀抢购共用同一实例（worker_id 单实例唯一）。
	orderNoGen, err := snowflake.New(cfg.Snowflake.WorkerID)
	if err != nil {
		log.Fatal("初始化雪花订单号生成器失败", zap.Error(err))
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
		log.Fatal("初始化秒杀令牌桶限流失败", zap.Error(err))
	}
	flashsaleStore := flashsalerepo.Store{Activities: flashsalerepo.NewGORMActivity(db)}
	flashsaleSvc := flashsalesvc.New(flashsaleStore, productSvc, cacheClient,
		limiter.RedisCounterConfig{Max: cfg.FlashSale.PerUserMax, Window: cfg.FlashSale.PerUserWindow},
		mqClient, orderNoGen)
	flashsaleHandler := flashsalehandler.New(flashsaleSvc, verifier)

	// order 模块：购物车结算/直购下单（单事务：订单+订单项+库存+地址快照+券核销+
	// 删除购物车条目）、client_request_id 幂等（Redis SETNX）、雪花订单号、
	// 订单列表/详情、取消（回补库存+回退券）、确认收货与 admin 发货；
	// CreateSeckill（T12）：秒杀异步落单单事务 + 扣活动库存（flashsale 实现端口）；
	// CancelExpiredSeckill（T13）：秒杀超时取消回补（flashsale 实现 SeckillRestore 端口）。
	orderStore := orderrepo.NewGORMOrder(db)
	orderSvc := ordersvc.New(orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(db), Tx: orderStore},
		cacheClient, orderNoGen, productSvc, couponSvc, cartSvc, userSvc, flashsaleSvc, flashsaleSvc)
	orderHandler := orderhandler.New(orderSvc, verifier)

	// 秒杀库存对账（T13）：进行中只比对告警（补单信号），收尾以 MySQL 对齐 Redis；
	// 有效订单数经 order 服务端口统计（进程内调用，flashsale → order 单向依赖）。
	reconcile := flashsalesvc.NewReconciliation(flashsaleStore, cacheClient, orderSvc)

	// social 模块：好友申请/通过/拒绝与好友列表；用户名经 userSvc 跨模块进程内调用补齐。
	// 好友圈动态：分享购买校验经 orderSvc（已支付/已发货/已完成订单含该 SKU）。
	socialStore := socialrepo.Store{Requests: socialrepo.NewGORMRequest(db), Friendships: socialrepo.NewGORMFriendship(db), Posts: socialrepo.NewGORMPost(db)}
	socialSvc := socialsvc.New(socialStore, userSvc)
	socialHandler := socialhandler.New(
		socialSvc,
		socialsvc.NewPosts(socialStore, userSvc, orderSvc),
		verifier,
	)

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
	chatSvc := chatsvc.New(chatStore, userSvc, socialSvc, wsMessageNotifier{hub: wsHub})
	chatHandler := chathandler.New(chatSvc, verifier)

	// T12 秒杀异步落单消费者：订阅"抢购成功"消息 → 查活动/默认地址 →
	// order 服务幂等建单 + 同事务扣活动库存；瞬时失败 Nack 重投、
	// 永久失败进死信（对账兜底）。消费中断 3s 后重连（at-least-once 不丢消息）。
	seckillConsumer := flashsalesvc.NewSeckillOrderConsumer(
		flashsaleStore.Activities, orderSvc, userSvc, log)
	go func() {
		for {
			err := mqClient.Consume(context.Background(), flashsalesvc.SeckillOrderQueue, seckillConsumer.Handle)
			if err != nil {
				log.Error("秒杀落单消费者中断，3s 后重连", zap.Error(err))
				time.Sleep(3 * time.Second)
				continue
			}
			return
		}
	}()

	// payment 模块：模拟支付回调（成功/失败），流水唯一约束（payment_id）挡重复回调，
	// 成功路径单事务（流水 + 订单 待支付→已支付，WHERE 校验状态机与金额）；
	// 订单读取与状态迁移经 order 服务进程内调用。
	paymentStore := paymentrepo.NewGORMPayment(db)
	paymentHandler := paymenthandler.New(
		paymentsvc.New(paymentrepo.Store{Payments: paymentStore, Tx: paymentStore}, orderSvc),
		verifier,
	)

	// cron 定时任务：平台级注册机制（platform/cron），单实例调度；
	// T09 订单超时取消——每分钟扫描已过 expire_at 的待支付普通订单，
	// 事务内取消 + 回补库存 + 回退券；单订单失败跳过（失败数记日志，下个 tick 重试）。
	// T13 秒杀超时取消——每分钟扫描待支付秒杀订单，回补活动库存 + Redis 库存 +
	// 用户计数（允许再次抢购）；对账——每小时进行中只比对告警（补单信号）、
	// 每分钟收尾以 MySQL 对齐刚结束活动的 Redis 库存。
	cronRegistry := registerCron(log, orderSvc, reconcile)
	cronRegistry.Start()

	// file 基础设施：统一文件上传代理（类型白名单 + ≤5MB + MinIO 私有桶）。
	fileHandler := file.NewHandler(fileSvc, verifier)

	// WebSocket 实时通道（T18）：GET /ws?token=<jwt>（token 进日志风险为演示取舍）。
	r.GET("/ws", wsHub.Handler(verifier))

	api := r.Group("/api")
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
	fileHandler.RegisterRoutes(api)

	return r, cronRegistry
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

// wsMessageNotifier 将 chat 服务"消息已落库"事件接入 WS Hub（T18）：
// 向在线接收方推送 new_message 事件；离线为无操作（落库 + 上线 REST 补拉兜底）。
type wsMessageNotifier struct {
	hub *ws.Hub
}

func (n wsMessageNotifier) NotifyMessageSent(_ context.Context, msg *chatmodel.Message) {
	n.hub.PushToUser(msg.RecipientID, ws.EventNewMessage, msg)
}

var _ chatsvc.MessageNotifier = wsMessageNotifier{}

// registerCron 注册全部定时任务并返回调度器（Start 由调用方执行）。
func registerCron(log *zap.Logger, orderSvc ordersvc.Service, reconcile flashsalesvc.Reconciliation) *platformcron.Registry {
	registry := platformcron.New(log, 5*time.Minute)
	if err := registry.Register(platformcron.Job{
		Name: "order-timeout-cancel",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			cancelled, failed, err := orderSvc.CancelExpired(ctx)
			if err != nil {
				return err
			}
			log.Info("订单超时取消完成", zap.Int("cancelled", cancelled), zap.Int("failed", failed))
			return nil
		},
	}); err != nil {
		log.Fatal("注册超时取消任务失败", zap.Error(err))
	}
	if err := registry.Register(platformcron.Job{
		Name: "seckill-timeout-cancel",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			cancelled, failed, redisFailed, err := orderSvc.CancelExpiredSeckill(ctx)
			if err != nil {
				return err
			}
			log.Info("秒杀订单超时取消完成",
				zap.Int("cancelled", cancelled), zap.Int("failed", failed), zap.Int("redis_failed", redisFailed))
			return nil
		},
	}); err != nil {
		log.Fatal("注册秒杀超时取消任务失败", zap.Error(err))
	}
	if err := registry.Register(platformcron.Job{
		Name: "flashsale-reconcile-active",
		Spec: "0 * * * *",
		Fn: func(ctx context.Context) error {
			warnings, err := reconcile.ReconcileActive(ctx)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				log.Warn("秒杀库存对账差异（进行中，仅告警不写回）",
					zap.Int64("activity_id", w.ActivityID), zap.String("title", w.Title),
					zap.Int("redis_stock", w.RedisStock), zap.Int("mysql_stock", w.MySQLStock),
					zap.Int("order_count", w.OrderCount), zap.String("detail", w.Detail))
			}
			if len(warnings) == 0 {
				log.Info("秒杀库存对账无差异")
			}
			return nil
		},
	}); err != nil {
		log.Fatal("注册秒杀对账任务失败", zap.Error(err))
	}
	if err := registry.Register(platformcron.Job{
		Name: "flashsale-reconcile-ended",
		Spec: "* * * * *",
		Fn: func(ctx context.Context) error {
			aligned, err := reconcile.ReconcileEnded(ctx)
			if err != nil {
				return err
			}
			if aligned > 0 {
				log.Warn("秒杀收尾对账已对齐", zap.Int("aligned", aligned))
			}
			return nil
		},
	}); err != nil {
		log.Fatal("注册秒杀收尾对账任务失败", zap.Error(err))
	}
	return registry
}
