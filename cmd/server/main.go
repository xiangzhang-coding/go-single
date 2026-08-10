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
	couponhandler "github.com/xiangzhang-coding/go-single/internal/coupon/handler"
	couponrepo "github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	flashsalehandler "github.com/xiangzhang-coding/go-single/internal/flashsale/handler"
	flashsalerepo "github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	orderhandler "github.com/xiangzhang-coding/go-single/internal/order/handler"
	orderrepo "github.com/xiangzhang-coding/go-single/internal/order/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/health"
	"github.com/xiangzhang-coding/go-single/internal/platform/logger"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	"github.com/xiangzhang-coding/go-single/internal/platform/snowflake"
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

	log, err := logger.New(cfg.Log.Level)
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

	router := newRouter(cfg, log, db, sqlDB, cacheClient, mqClient, fileSvc)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		log.Info("HTTP 服务启动", zap.Int("port", cfg.Server.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常退出", zap.Error(err))
			os.Exit(1)
		}
	}()

	// 优雅关闭：等待 SIGINT / SIGTERM。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Info("收到退出信号，正在关闭")

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

func newRouter(cfg *config.Config, log *zap.Logger, db *gorm.DB, sqlDB *sql.DB, cacheClient cache.Cache, mqClient mq.MQ, fileSvc *file.MinIO) *gin.Engine {
	gin.SetMode(cfg.Server.Mode)

	r := gin.New()
	// metrics 在最外层：panic 被 Recovery 恢复为 500 后仍能完成计数。
	metricRegistry := metrics.New()
	r.Use(metricRegistry.GinMiddleware(), gin.Recovery(), requestLogger(log))

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

	// flashsale 模块：admin 秒杀活动管理（创建/编辑/上架/下架），
	// 上架预热库存进 Redis（未开始可覆盖、进行中只减不增）；SKU 校验经 product 服务接口。
	flashsaleHandler := flashsalehandler.New(
		flashsalesvc.New(flashsalerepo.Store{Activities: flashsalerepo.NewGORMActivity(db)}, productSvc, cacheClient),
		verifier,
	)

	// social 模块：好友申请/通过/拒绝与好友列表；用户名经 userSvc 跨模块进程内调用补齐。
	socialHandler := socialhandler.New(
		socialsvc.New(socialrepo.Store{Requests: socialrepo.NewGORMRequest(db), Friendships: socialrepo.NewGORMFriendship(db)}, userSvc),
		verifier,
	)

	// order 模块：购物车结算/直购下单（单事务：订单+订单项+库存+地址快照+券核销+
	// 删除购物车条目）、client_request_id 幂等（Redis SETNX）、雪花订单号、
	// 订单列表/详情、取消（回补库存+回退券）、确认收货与 admin 发货。
	orderNoGen, err := snowflake.New(1)
	if err != nil {
		log.Fatal("初始化雪花订单号生成器失败", zap.Error(err))
	}
	orderStore := orderrepo.NewGORMOrder(db)
	orderHandler := orderhandler.New(
		ordersvc.New(orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(db), Tx: orderStore},
			cacheClient, orderNoGen, productSvc, couponSvc, cartSvc, userSvc),
		verifier,
	)

	// file 基础设施：统一文件上传代理（类型白名单 + ≤5MB + MinIO 私有桶）。
	fileHandler := file.NewHandler(fileSvc, verifier)

	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	addressHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
	cartHandler.RegisterRoutes(api)
	couponHandler.RegisterRoutes(api)
	flashsaleHandler.RegisterRoutes(api)
	socialHandler.RegisterRoutes(api)
	orderHandler.RegisterRoutes(api)
	fileHandler.RegisterRoutes(api)

	return r
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
