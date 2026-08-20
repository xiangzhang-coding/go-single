package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/config"
	"github.com/xiangzhang-coding/go-single/internal/platform/file"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	"github.com/xiangzhang-coding/go-single/internal/platform/ws"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
)

func TestProductionRouterRegistersCompleteApplication(t *testing.T) {
	cfg, err := config.LoadFrom("../../configs")
	require.NoError(t, err)
	cfg.Server.Mode = "test"
	cfg.Server.RequestTimeout = 2 * time.Second
	cfg.MySQL.Database = "go_shop_router_test"
	cfg.Redis.DB = 12
	cfg.MinIO.Bucket = "go-shop-router-test"
	cfg.Migrations.Path = "../../migrations"
	cfg.Auth.Secret = "production-router-test-secret"

	root, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/",
		routerEnvOr("GO_SINGLE_MYSQL_ROOT_USER", "root"),
		routerEnvOr("GO_SINGLE_MYSQL_ROOT_PASSWORD", "root123"),
		cfg.MySQL.Host, cfg.MySQL.Port,
	))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	testsupport.RequireDependency(t, "MySQL", root.PingContext(ctx))
	_, err = root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+cfg.MySQL.Database)
	require.NoError(t, err)
	_, err = root.ExecContext(ctx, "CREATE DATABASE "+cfg.MySQL.Database)
	require.NoError(t, err)
	_, err = root.ExecContext(ctx, fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%%'", cfg.MySQL.Database, cfg.MySQL.User))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + cfg.MySQL.Database) })

	log := zap.NewNop()
	gdb, err := openMySQL(cfg, log)
	require.NoError(t, err)
	sqlDB, err := gdb.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, runMigrations(cfg, log))

	cacheClient, err := cache.NewRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	testsupport.RequireDependency(t, "Redis", err)
	require.NoError(t, cacheClient.Del(context.Background(), "router-test-probe"))
	t.Cleanup(func() {
		_ = cacheClient.Close()
	})

	mqClient, err := mq.NewRabbitMQ(cfg.MQ.URL)
	testsupport.RequireDependency(t, "RabbitMQ", err)
	t.Cleanup(func() { _ = mqClient.Close() })

	fileSvc, err := file.NewMinIO(file.MinIOConfig{
		Endpoint: cfg.MinIO.Endpoint, AccessKey: cfg.MinIO.AccessKey,
		SecretKey: cfg.MinIO.SecretKey, Bucket: cfg.MinIO.Bucket, UseSSL: cfg.MinIO.UseSSL,
	}, file.NewGORMUsage(gdb), file.QuotaConfig{
		MaxBytesPerUser: cfg.Upload.MaxBytesPerUser, MaxObjectsPerUser: cfg.Upload.MaxObjectsPerUser,
	})
	testsupport.RequireDependency(t, "MinIO", err)

	wsHub := ws.New(ws.Config{
		HeartbeatInterval: cfg.WS.HeartbeatInterval, WriteWait: cfg.WS.WriteWait,
		AllowOrigins: cfg.WS.AllowOrigins, MaxConnections: cfg.WS.MaxConnections,
		MaxConnectionsPerUser: cfg.WS.MaxConnectionsPerUser, MaxConnectionsPerIP: cfg.WS.MaxConnectionsPerIP,
	}, log)
	t.Cleanup(wsHub.Close)

	router, background, err := newRouter(cfg, log, gdb, sqlDB, cacheClient, mqClient, fileSvc, wsHub)
	require.NoError(t, err)
	t.Cleanup(func() { _ = background.Stop(context.Background()) })

	actual := make(map[string]struct{})
	for _, route := range router.Routes() {
		actual[route.Method+" "+route.Path] = struct{}{}
	}
	expected := []string{
		"GET /metrics", "GET /healthz", "GET /ws",
		"POST /api/auth/register", "POST /api/auth/login", "GET /api/users/me", "PATCH /api/users/me", "GET /api/users", "GET /api/users/:id",
		"GET /api/addresses", "POST /api/addresses", "PUT /api/addresses/:id", "DELETE /api/addresses/:id", "PUT /api/addresses/:id/default",
		"GET /api/categories", "GET /api/products", "GET /api/products/:id", "POST /api/admin/categories", "PUT /api/admin/categories/:id", "DELETE /api/admin/categories/:id",
		"POST /api/admin/products", "GET /api/admin/products", "GET /api/admin/products/:id", "PUT /api/admin/products/:id", "POST /api/admin/products/:id/publish", "POST /api/admin/products/:id/unpublish", "POST /api/admin/products/:id/skus", "PUT /api/admin/skus/:id", "DELETE /api/admin/skus/:id",
		"GET /api/cart", "POST /api/cart", "PUT /api/cart/items/:id", "DELETE /api/cart/items/:id",
		"GET /api/coupons", "POST /api/coupons/:id/claim", "GET /api/coupons/mine", "POST /api/admin/coupons", "GET /api/admin/coupons", "PUT /api/admin/coupons/:id",
		"POST /api/admin/flashsales", "GET /api/admin/flashsales", "PUT /api/admin/flashsales/:id", "POST /api/admin/flashsales/:id/publish", "POST /api/admin/flashsales/:id/unpublish", "GET /api/flashsales", "POST /api/flashsales/:id/purchase", "GET /api/flashsales/purchases/:id",
		"POST /api/friend-requests", "GET /api/friend-requests", "POST /api/friend-requests/:id/accept", "POST /api/friend-requests/:id/reject", "GET /api/friends", "POST /api/posts", "GET /api/posts/feed", "GET /api/posts/mine", "DELETE /api/posts/:id",
		"POST /api/messages", "GET /api/conversations", "GET /api/conversations/:key/messages", "POST /api/conversations/:key/read",
		"POST /api/orders", "GET /api/orders", "GET /api/orders/:order_no", "POST /api/orders/:order_no/cancel", "POST /api/orders/:order_no/confirm", "GET /api/admin/orders", "POST /api/admin/orders/:order_no/ship",
		"POST /api/payments/mock", "POST /api/files", "GET /api/files/:reference",
	}
	require.Len(t, actual, len(expected))
	for _, route := range expected {
		require.Contains(t, actual, route)
	}

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/products", nil))
	require.Equal(t, http.StatusOK, response.Code)

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/cart", nil))
	require.Equal(t, http.StatusUnauthorized, response.Code)

	migrations, err := migrate.New("file://"+cfg.Migrations.Path, "mysql://"+cfg.MySQL.DSN())
	require.NoError(t, err)
	assertIdempotencyCollations := func(want string) {
		t.Helper()
		for _, column := range []struct{ table, name string }{
			{table: "flashsale_pre_deductions", name: "client_request_id"},
			{table: "messages", name: "client_request_id"},
			{table: "payments", name: "payment_id"},
		} {
			var collation string
			require.NoError(t, gdb.Raw(`
				SELECT COLLATION_NAME FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
				cfg.MySQL.Database, column.table, column.name).Scan(&collation).Error)
			require.Equal(t, want, collation, column.table+"."+column.name)
		}
	}
	require.NoError(t, migrations.Steps(-1), "latest migration must downgrade cleanly without conflicting data")
	assertIdempotencyCollations("utf8mb4_unicode_ci")
	require.NoError(t, migrations.Steps(1), "latest migration must reapply cleanly")
	assertIdempotencyCollations("utf8mb4_0900_bin")
	sourceErr, databaseErr := migrations.Close()
	require.NoError(t, sourceErr)
	require.NoError(t, databaseErr)

	require.NoError(t, gdb.Exec("INSERT INTO users (username, password_hash, role) VALUES (?, ?, 'user')", "migration-case-user", "unused").Error)
	var adminID, insertedUserID int64
	require.NoError(t, gdb.Table("users").Select("id").Where("username = ?", "admin").Scan(&adminID).Error)
	require.NoError(t, gdb.Table("users").Select("id").Where("username = ?", "migration-case-user").Scan(&insertedUserID).Error)
	conversationKey := fmt.Sprintf("%d:%d", adminID, insertedUserID)
	require.NoError(t, gdb.Exec("INSERT INTO conversations (conversation_key, user_a, user_b) VALUES (?, ?, ?)", conversationKey, adminID, insertedUserID).Error)
	require.NoError(t, gdb.Exec(`
		INSERT INTO messages (conversation_key, sender_id, recipient_id, type, content, url, client_request_id)
		VALUES (?, ?, ?, 'text', 'one', '', 'Case-Key'), (?, ?, ?, 'text', 'two', '', 'case-key')`,
		conversationKey, adminID, insertedUserID, conversationKey, adminID, insertedUserID).Error)
	conflictingDown, err := migrate.New("file://"+cfg.Migrations.Path, "mysql://"+cfg.MySQL.DSN())
	require.NoError(t, err)
	require.Error(t, conflictingDown.Steps(-1), "downgrade must reject identities that old collation would merge")
	assertIdempotencyCollations("utf8mb4_0900_bin")
	_, _ = conflictingDown.Close()
}

func routerEnvOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
