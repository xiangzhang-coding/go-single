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

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	"github.com/xiangzhang-coding/go-single/internal/platform/health"
	"github.com/xiangzhang-coding/go-single/internal/platform/logger"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
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

	router := newRouter(cfg, log, db, sqlDB, cacheClient, mqClient)

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

func newRouter(cfg *config.Config, log *zap.Logger, db *gorm.DB, sqlDB *sql.DB, cacheClient cache.Cache, mqClient mq.MQ) *gin.Engine {
	gin.SetMode(cfg.Server.Mode)

	r := gin.New()
	r.Use(gin.Recovery(), requestLogger(log))

	checker := &health.Checker{
		MySQL:   sqlDB,
		Cache:   cacheClient,
		MQ:      mqClient,
		Timeout: 2 * time.Second,
	}
	r.GET("/healthz", healthHandler(checker))

	// user 模块：注册/登录/鉴权。
	verifier := auth.NewJWT(auth.JWTConfig{Secret: cfg.Auth.Secret, TTL: cfg.Auth.TTL})
	userHandler := userhandler.New(usersvc.New(userrepo.NewGORM(db), verifier), verifier)
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)

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
