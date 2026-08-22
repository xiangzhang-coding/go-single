package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/logger"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	"github.com/xiangzhang-coding/go-single/internal/platform/ws"
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
	db, err := openMySQL(cfg, log)
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
	validationCtx, validationCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = validateReleaseDatabase(validationCtx, cfg.Server.Mode, sqlSeedAdminChecker{db: sqlDB})
	validationCancel()
	if err != nil {
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
	}, file.NewGORMUsage(db), file.QuotaConfig{
		MaxBytesPerUser: cfg.Upload.MaxBytesPerUser, MaxObjectsPerUser: cfg.Upload.MaxObjectsPerUser,
	})
	if err != nil {
		return err
	}
	uploadRecoveryCtx, uploadRecoveryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	resolvedUploads, uploadRecoveryErr := fileSvc.ReconcilePendingUploads(uploadRecoveryCtx)
	uploadRecoveryCancel()
	if uploadRecoveryErr != nil {
		log.Warn("未完成上传启动对账失败（定时任务将重试）", zap.Error(uploadRecoveryErr))
	} else if resolvedUploads > 0 {
		log.Warn("未完成上传启动对账已清理", zap.Int("resolved", resolvedUploads))
	}

	// WebSocket 实时通道：JWT 授权期限、连接配额与心跳共同约束会话生命周期；
	// 关闭在 HTTP 优雅关闭之后执行。
	wsHub := ws.New(ws.Config{
		HeartbeatInterval:     cfg.WS.HeartbeatInterval,
		WriteWait:             cfg.WS.WriteWait,
		AllowOrigins:          cfg.WS.AllowOrigins,
		MaxConnections:        cfg.WS.MaxConnections,
		MaxConnectionsPerUser: cfg.WS.MaxConnectionsPerUser,
		MaxConnectionsPerIP:   cfg.WS.MaxConnectionsPerIP,
	}, log)
	defer wsHub.Close()

	router, background, err := newRouter(cfg, log, db, sqlDB, cacheClient, mqClient, fileSvc, wsHub)
	if err != nil {
		return err
	}
	background.Start()

	srv := &http.Server{
		Addr:              listenAddress(cfg.Server),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.Upload.RequestTimeout + 10*time.Second,
		WriteTimeout:      cfg.Upload.RequestTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP 服务启动", zap.String("host", cfg.Server.Host), zap.Int("port", cfg.Server.Port))
		serverErr <- srv.ListenAndServe()
	}()

	// 优雅关闭：等待 SIGINT / SIGTERM；先停 cron（等待执行中的任务），再关 HTTP。
	// 两段使用独立超时预算：cron 停止不应耗尽 HTTP 关闭的时间。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)
	serveErr := waitForServerStop(quit, serverErr)
	if serveErr != nil {
		log.Error("HTTP 服务异常退出，正在关闭", zap.Error(serveErr))
	} else {
		log.Info("收到退出信号，正在关闭")
	}

	backgroundCtx, backgroundCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := background.Stop(backgroundCtx); err != nil {
		log.Warn("后台任务停止超时", zap.Error(err))
	}
	backgroundCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(serveErr, srv.Shutdown(ctx))
}

func waitForServerStop(quit <-chan os.Signal, serverErr <-chan error) error {
	select {
	case <-quit:
		return nil
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func openMySQL(cfg *config.Config, log *zap.Logger) (*gorm.DB, error) {
	gdb, err := gorm.Open(gormmysql.Open(cfg.MySQL.DSN()), &gorm.Config{
		// SQL 始终保留占位符，错误与慢查询不输出绑定参数或原始数据库错误。
		Logger: logger.NewGORM(log, gormlogger.Warn, 200*time.Millisecond),
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
	driverCfg := mysqldriver.NewConfig()
	driverCfg.User = cfg.MySQL.User
	driverCfg.Passwd = cfg.MySQL.Password
	driverCfg.Net = "tcp"
	driverCfg.Addr = cfg.MySQL.Host + ":" + strconv.Itoa(cfg.MySQL.Port)
	driverCfg.DBName = cfg.MySQL.Database
	driverCfg.ParseTime = true
	driverCfg.Loc = time.Local
	driverCfg.MultiStatements = true
	connector, err := mysqldriver.NewConnector(driverCfg)
	if err != nil {
		return fmt.Errorf("构造迁移连接: %w", err)
	}
	migrationDB := sql.OpenDB(connector)
	driver, err := migratemysql.WithInstance(migrationDB, &migratemysql.Config{})
	if err != nil {
		_ = migrationDB.Close()
		return fmt.Errorf("初始化迁移驱动: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+cfg.Migrations.Path, "mysql", driver)
	if err != nil {
		_ = driver.Close()
		return fmt.Errorf("初始化迁移: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("执行迁移: %w", err)
	}
	log.Info("数据库迁移完成")
	return nil
}
