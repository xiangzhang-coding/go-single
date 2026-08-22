package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
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
	cfg.Server.RequestTimeout = 15 * time.Second
	cfg.MySQL.Database = "go_shop_router_test"
	cfg.Redis.DB = 12
	cfg.MinIO.Bucket = fmt.Sprintf("go-shop-router-test-%d", time.Now().UnixNano())
	cfg.Migrations.Path = "../../migrations"
	cfg.Auth.Secret = "production-router-test-secret"
	cfg.Auth.RegisterRateLimit.PerIPMax = 20

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

	const migrationUser = "migration_password_test"
	const migrationPassword = "strong+password%not-url-escaped"
	const migrationDatabase = "go_shop_migration_password_test"
	_, _ = root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+migrationDatabase)
	_, _ = root.ExecContext(ctx, "DROP USER IF EXISTS '"+migrationUser+"'@'%'")
	_, err = root.ExecContext(ctx, "CREATE DATABASE "+migrationDatabase)
	require.NoError(t, err)
	_, err = root.ExecContext(ctx, "CREATE USER '"+migrationUser+"'@'%' IDENTIFIED BY '"+migrationPassword+"'")
	require.NoError(t, err)
	_, err = root.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+migrationDatabase+".* TO '"+migrationUser+"'@'%'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + migrationDatabase)
		_, _ = root.Exec("DROP USER IF EXISTS '" + migrationUser + "'@'%'")
	})
	migrationCfg := *cfg
	migrationCfg.MySQL.User = migrationUser
	migrationCfg.MySQL.Password = migrationPassword
	migrationCfg.MySQL.Database = migrationDatabase
	require.NoError(t, runMigrations(&migrationCfg, zap.NewNop()))

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

	redisProbe := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	redisCtx, redisCancel := context.WithTimeout(context.Background(), 5*time.Second)
	testsupport.RequireDependency(t, "Redis", redisProbe.Ping(redisCtx).Err())
	require.NoError(t, redisProbe.FlushDB(redisCtx).Err())
	redisCancel()
	require.NoError(t, redisProbe.Close())

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
	minioCleanup, err := miniogo.New(cfg.MinIO.Endpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.MinIO.AccessKey, cfg.MinIO.SecretKey, ""),
		Secure: cfg.MinIO.UseSSL,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for object := range minioCleanup.ListObjects(cleanupCtx, cfg.MinIO.Bucket, miniogo.ListObjectsOptions{Recursive: true}) {
			if object.Err != nil {
				t.Errorf("列出测试 MinIO 对象: %v", object.Err)
				return
			}
			if removeErr := minioCleanup.RemoveObject(cleanupCtx, cfg.MinIO.Bucket, object.Key, miniogo.RemoveObjectOptions{}); removeErr != nil {
				t.Errorf("删除测试 MinIO 对象 %s: %v", object.Key, removeErr)
				return
			}
		}
		if removeErr := minioCleanup.RemoveBucket(cleanupCtx, cfg.MinIO.Bucket); removeErr != nil {
			t.Errorf("删除测试 MinIO bucket: %v", removeErr)
		}
	})

	wsHub := ws.New(ws.Config{
		HeartbeatInterval: cfg.WS.HeartbeatInterval, WriteWait: cfg.WS.WriteWait,
		AllowOrigins: cfg.WS.AllowOrigins, MaxConnections: cfg.WS.MaxConnections,
		MaxConnectionsPerUser: cfg.WS.MaxConnectionsPerUser, MaxConnectionsPerIP: cfg.WS.MaxConnectionsPerIP,
	}, log)
	t.Cleanup(wsHub.Close)

	router, background, err := newRouter(cfg, log, gdb, sqlDB, cacheClient, mqClient, fileSvc, wsHub)
	require.NoError(t, err)

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
	var unauthorized productionErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &unauthorized))
	require.Equal(t, "missing token", unauthorized.Error)
	var unauthorizedFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &unauthorizedFields))
	productionRequireKeys(t, unauthorizedFields, "error")

	background.Start()
	stopBackground := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		require.NoError(t, background.Stop(stopCtx))
	}
	t.Cleanup(stopBackground)
	productionWaitForHealth(t, router, 10*time.Second)

	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	t.Run("core transaction through production router", func(t *testing.T) {
		username := "core" + runID
		userID, userToken := productionRegisterAndLogin(t, router, username)
		addressID := productionCreateAddress(t, router, userToken, "Core Receiver")
		adminToken := productionLogin(t, router, "admin", "admin123", "admin")
		productID, skuID := productionCreateOnSaleSKU(t, router, adminToken, "core-"+runID, 1250, 10)

		cartItem, cartFields := productionJSON[productionCartItemResponse](t, router, http.MethodPost, "/api/cart", userToken,
			map[string]any{"sku_id": skuID, "quantity": 2}, http.StatusCreated)
		productionRequireKeys(t, cartFields, "id", "user_id", "sku_id", "quantity", "created_at", "updated_at")
		require.Positive(t, cartItem.ID)
		require.Equal(t, userID, cartItem.UserID)
		require.Equal(t, skuID, cartItem.SKUID)
		require.Equal(t, 2, cartItem.Quantity)

		orderRequestID := "core-order-" + runID
		order, orderFields := productionJSON[productionOrderResponse](t, router, http.MethodPost, "/api/orders", userToken,
			map[string]any{"client_request_id": orderRequestID, "address_id": addressID, "from_cart": true}, http.StatusCreated)
		productionRequireKeys(t, orderFields,
			"order_no", "user_id", "order_type", "status", "total_amount", "discount_amount", "pay_amount",
			"receiver", "phone", "province", "city", "district", "detail", "expire_at", "created_at", "updated_at", "items")
		require.NotEmpty(t, order.OrderNo)
		require.Equal(t, userID, order.UserID)
		require.Equal(t, "normal", order.OrderType)
		require.Equal(t, "pending_payment", order.Status)
		require.Equal(t, int64(2500), order.TotalAmount)
		require.Equal(t, int64(2500), order.PayAmount)
		require.Equal(t, "Core Receiver", order.Receiver)
		require.Len(t, order.Items, 1)
		productionRequireOrderItem(t, order.Items[0], order.OrderNo, productID, skuID, 1250, 2)

		cart, cartListFields := productionJSON[productionCartListResponse](t, router, http.MethodGet, "/api/cart", userToken, nil, http.StatusOK)
		productionRequireKeys(t, cartListFields, "items")
		require.Empty(t, cart.Items, "from-cart checkout must remove purchased items")
		product, productFields := productionJSON[productionProductDetailResponse](t, router, http.MethodGet,
			fmt.Sprintf("/api/products/%d", productID), userToken, nil, http.StatusOK)
		productionRequireKeys(t, productFields, "id", "category_id", "title", "description", "status", "created_at", "updated_at", "skus")
		require.Equal(t, 8, productionSKUStock(t, product, skuID), "checkout must deduct the purchased SKU stock")

		paymentID := "core-payment-" + runID
		payment, paymentFields := productionJSON[productionPaymentResponse](t, router, http.MethodPost, "/api/payments/mock", userToken,
			map[string]any{"order_id": order.OrderNo, "payment_id": paymentID, "amount": order.PayAmount, "result": "success"}, http.StatusCreated)
		productionRequireKeys(t, paymentFields, "id", "payment_id", "order_no", "user_id", "amount", "result", "created_at", "updated_at")
		require.Equal(t, paymentID, payment.PaymentID)
		require.Equal(t, order.OrderNo, payment.OrderNo)
		require.Equal(t, userID, payment.UserID)
		require.Equal(t, order.PayAmount, payment.Amount)
		require.Equal(t, "success", payment.Result)

		productionNoContent(t, router, http.MethodPost, "/api/admin/orders/"+order.OrderNo+"/ship", adminToken)
		productionNoContent(t, router, http.MethodPost, "/api/orders/"+order.OrderNo+"/confirm", userToken)

		completed, completedFields := productionJSON[productionOrderResponse](t, router, http.MethodGet, "/api/orders/"+order.OrderNo, userToken, nil, http.StatusOK)
		productionRequireKeys(t, completedFields,
			"order_no", "user_id", "order_type", "status", "total_amount", "discount_amount", "pay_amount",
			"receiver", "phone", "province", "city", "district", "detail", "paid_at", "shipped_at", "completed_at",
			"expire_at", "created_at", "updated_at", "items")
		require.Equal(t, "completed", completed.Status)
		require.NotNil(t, completed.PaidAt)
		require.NotNil(t, completed.ShippedAt)
		require.NotNil(t, completed.CompletedAt)
		require.Equal(t, int64(2500), completed.PayAmount)
		require.Equal(t, "Core Receiver", completed.Receiver)
		require.Len(t, completed.Items, 1)
		productionRequireOrderItem(t, completed.Items[0], order.OrderNo, productID, skuID, 1250, 2)

		productionExerciseMediaAuthorization(t, router, runID, userID, userToken, skuID)
	})

	t.Run("flash sale cancellation restores a purchasable slot", func(t *testing.T) {
		username := "flash" + runID
		_, userToken := productionRegisterAndLogin(t, router, username)
		productionCreateAddress(t, router, userToken, "Flash Receiver")
		adminToken := productionLogin(t, router, "admin", "admin123", "admin")
		_, skuID := productionCreateOnSaleSKU(t, router, adminToken, "flash-"+runID, 5000, 10)

		activity, activityFields := productionJSON[productionActivityResponse](t, router, http.MethodPost, "/api/admin/flashsales", adminToken,
			map[string]any{
				"sku_id": skuID, "title": "flash-" + runID, "price": int64(1900), "stock": 1, "per_user_limit": 1,
				"start_at": time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
				"end_at":   time.Now().Add(10 * time.Minute).Format(time.RFC3339Nano),
			}, http.StatusCreated)
		productionRequireKeys(t, activityFields,
			"id", "sku_id", "title", "price", "stock", "per_user_limit", "status", "start_at", "end_at", "created_at", "updated_at")
		require.Positive(t, activity.ID)
		require.Equal(t, skuID, activity.SKUID)
		require.Equal(t, 1, activity.Stock)
		require.Equal(t, 1, activity.PerUserLimit)
		require.Equal(t, "off_sale", activity.Status)
		productionNoContent(t, router, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", activity.ID), adminToken)

		list, listFields := productionJSON[productionFlashSaleListResponse](t, router, http.MethodGet, "/api/flashsales", userToken, nil, http.StatusOK)
		productionRequireKeys(t, listFields, "server_time", "items")
		require.NotEmpty(t, list.ServerTime)
		listItemFields := productionJSONObjects(t, listFields["items"])
		require.Len(t, listItemFields, len(list.Items))
		activeFound := false
		for index, item := range list.Items {
			if item.ID == activity.ID {
				activeFound = true
				productionRequireFlashSaleItemContract(t, listItemFields[index])
				require.Equal(t, "in_progress", item.State)
				require.Equal(t, 1, item.Stock)
			}
		}
		require.True(t, activeFound, "published activity must be observable as active through the user API")

		firstRequestID := "flash-first-" + runID
		first, firstFields := productionPurchaseAccepted(t, router, userToken, activity.ID, firstRequestID, 90*time.Second)
		productionRequireKeys(t, firstFields, "pre_deduction_id", "pre_deduction_status", "status", "order_no", "message")
		require.Equal(t, "queued", first.Status)
		require.NotEmpty(t, first.PreDeductionID)
		require.NotEmpty(t, first.OrderNo)

		firstLifecycle := productionPollPurchaseOrdered(t, router, userToken, first.PreDeductionID, 10*time.Second)
		require.Equal(t, first.OrderNo, firstLifecycle.OrderNo)
		firstOrder, firstOrderFields := productionJSON[productionOrderResponse](t, router, http.MethodGet, "/api/orders/"+first.OrderNo, userToken, nil, http.StatusOK)
		productionRequireKeys(t, firstOrderFields,
			"order_no", "user_id", "order_type", "status", "activity_id", "purchase_slot", "total_amount", "discount_amount", "pay_amount",
			"receiver", "phone", "province", "city", "district", "detail", "expire_at", "created_at", "updated_at", "items")
		productionRequireSeckillOrder(t, firstOrder, activity.ID, first.PreDeductionID, skuID)
		require.Equal(t, 0, productionFlashSaleItem(t, router, userToken, activity.ID).Stock,
			"the accepted purchase must exhaust Redis stock before cancellation")
		blocked := productionRequest(t, router, http.MethodPost, fmt.Sprintf("/api/flashsales/%d/purchase", activity.ID), userToken,
			map[string]any{"client_request_id": "flash-blocked-" + runID})
		require.Equal(t, http.StatusConflict, blocked.Code, blocked.Body.String())
		var blockedBody map[string]any
		require.NoError(t, json.Unmarshal(blocked.Body.Bytes(), &blockedBody))
		testsupport.AssertAPIError(t, blockedBody)

		productionNoContent(t, router, http.MethodPost, "/api/orders/"+first.OrderNo+"/cancel", userToken)
		cancelled, cancelledFields := productionJSON[productionOrderResponse](t, router, http.MethodGet, "/api/orders/"+first.OrderNo, userToken, nil, http.StatusOK)
		productionRequireKeys(t, cancelledFields,
			"order_no", "user_id", "order_type", "status", "activity_id", "purchase_slot", "total_amount", "discount_amount", "pay_amount",
			"receiver", "phone", "province", "city", "district", "detail", "cancelled_at", "expire_at", "created_at", "updated_at", "items")
		require.Equal(t, "cancelled", cancelled.Status)
		require.NotNil(t, cancelled.CancelledAt)
		require.Equal(t, 1, productionFlashSaleItem(t, router, userToken, activity.ID).Stock,
			"cancellation must restore Redis stock before replacement purchase")

		secondRequestID := "flash-second-" + runID
		second, secondFields := productionPurchaseAccepted(t, router, userToken, activity.ID, secondRequestID, 90*time.Second)
		productionRequireKeys(t, secondFields, "pre_deduction_id", "pre_deduction_status", "status", "order_no", "message")
		require.Equal(t, "queued", second.Status)
		require.NotEqual(t, first.PreDeductionID, second.PreDeductionID)
		require.NotEqual(t, first.OrderNo, second.OrderNo)

		secondLifecycle := productionPollPurchaseOrdered(t, router, userToken, second.PreDeductionID, 10*time.Second)
		require.Equal(t, second.OrderNo, secondLifecycle.OrderNo)
		replacement, replacementFields := productionJSON[productionOrderResponse](t, router, http.MethodGet, "/api/orders/"+second.OrderNo, userToken, nil, http.StatusOK)
		productionRequireKeys(t, replacementFields,
			"order_no", "user_id", "order_type", "status", "activity_id", "purchase_slot", "total_amount", "discount_amount", "pay_amount",
			"receiver", "phone", "province", "city", "district", "detail", "expire_at", "created_at", "updated_at", "items")
		productionRequireSeckillOrder(t, replacement, activity.ID, second.PreDeductionID, skuID)
	})

	t.Run("seckill permanent failure rolls back through registered recovery job", func(t *testing.T) {
		stopCronCtx, stopCronCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCronCancel()
		require.NoError(t, background.cron.Stop(stopCronCtx))

		userID, userToken := productionRegisterAndLogin(t, router, "rollback"+runID)
		adminToken := productionLogin(t, router, "admin", "admin123", "admin")
		_, skuID := productionCreateOnSaleSKU(t, router, adminToken, "rollback-"+runID, 5000, 10)
		activity, activityFields := productionJSON[productionActivityResponse](t, router, http.MethodPost, "/api/admin/flashsales", adminToken,
			map[string]any{
				"sku_id": skuID, "title": "rollback-" + runID, "price": int64(1900), "stock": 10, "per_user_limit": 1,
				"start_at": time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
				"end_at":   time.Now().Add(10 * time.Minute).Format(time.RFC3339Nano),
			}, http.StatusCreated)
		productionRequireKeys(t, activityFields,
			"id", "sku_id", "title", "price", "stock", "per_user_limit", "status", "start_at", "end_at", "created_at", "updated_at")
		productionNoContent(t, router, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", activity.ID), adminToken)

		purchase, purchaseFields := productionPurchaseAccepted(t, router, userToken, activity.ID, "rollback-"+runID, 30*time.Second)
		productionRequireKeys(t, purchaseFields, "pre_deduction_id", "pre_deduction_status", "status", "order_no", "message")

		type preDeductionState struct {
			Status       string
			LastError    string
			PurchaseSlot int64
			RolledBackAt *time.Time
		}
		var state preDeductionState
		require.Eventually(t, func() bool {
			state = preDeductionState{}
			err := gdb.Table("flashsale_pre_deductions").
				Select("status", "last_error", "purchase_slot", "rolled_back_at").
				Where("id = ?", purchase.PreDeductionID).Scan(&state).Error
			return err == nil && state.Status == "pending_rollback" && state.LastError == "message reached dead-letter queue"
		}, 10*time.Second, 100*time.Millisecond, "the real DLQ consumer must persist its rollback marker")
		require.Positive(t, state.PurchaseSlot)

		stockKey := fmt.Sprintf("flashsale:stock:%d", activity.ID)
		countKey := fmt.Sprintf("flashsale:count:%d:%d", activity.ID, userID)
		idemKey := fmt.Sprintf("flashsale:idem:%d:%d:%d", activity.ID, userID, state.PurchaseSlot)
		reservationKey := "flashsale:reservation:" + purchase.PreDeductionID
		value, err := cacheClient.Get(context.Background(), stockKey)
		require.NoError(t, err)
		require.Equal(t, "9", value)
		value, err = cacheClient.Get(context.Background(), countKey)
		require.NoError(t, err)
		require.Equal(t, "1", value)
		value, err = cacheClient.Get(context.Background(), idemKey)
		require.NoError(t, err)
		require.Equal(t, purchase.PreDeductionID, value)
		value, err = cacheClient.Get(context.Background(), reservationKey)
		require.NoError(t, err)
		require.Equal(t, purchase.PreDeductionID, value)

		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer recoveryCancel()
		require.NoError(t, registeredCronJob(t, background.cronJobs, "flashsale-recovery").Fn(recoveryCtx))

		state = preDeductionState{}
		require.NoError(t, gdb.Table("flashsale_pre_deductions").
			Select("status", "last_error", "purchase_slot", "rolled_back_at").
			Where("id = ?", purchase.PreDeductionID).Scan(&state).Error)
		require.Equal(t, "rolled_back", state.Status)
		require.NotNil(t, state.RolledBackAt)

		lifecycle, lifecycleFields := productionJSON[productionPurchaseLifecycleResponse](t, router, http.MethodGet,
			"/api/flashsales/purchases/"+purchase.PreDeductionID, userToken, nil, http.StatusOK)
		productionRequireKeys(t, lifecycleFields, "id", "status", "order_no", "created_at", "updated_at", "rolled_back_at")
		require.Equal(t, "rolled_back", lifecycle.Status)
		require.NotNil(t, lifecycle.RolledBackAt)

		var orderCount int64
		require.NoError(t, gdb.Table("orders").Where("order_no = ?", purchase.OrderNo).Count(&orderCount).Error)
		require.Zero(t, orderCount)
		var mysqlStock int
		require.NoError(t, gdb.Table("flashsale_activities").Select("stock").Where("id = ?", activity.ID).Scan(&mysqlStock).Error)
		require.Equal(t, 10, mysqlStock)
		value, err = cacheClient.Get(context.Background(), stockKey)
		require.NoError(t, err)
		require.Equal(t, "10", value)
		value, err = cacheClient.Get(context.Background(), countKey)
		require.NoError(t, err)
		require.Equal(t, "0", value)
		_, err = cacheClient.Get(context.Background(), idemKey)
		require.ErrorIs(t, err, cache.ErrMiss)
		_, err = cacheClient.Get(context.Background(), reservationKey)
		require.ErrorIs(t, err, cache.ErrMiss)
	})

	stopBackground()

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
	require.NoError(t, migrations.Steps(-2), "collation migration must downgrade cleanly without conflicting data")
	assertIdempotencyCollations("utf8mb4_unicode_ci")
	require.NoError(t, migrations.Steps(2), "collation and upload migrations must reapply cleanly")
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
	require.Error(t, conflictingDown.Steps(-2), "downgrade must reject identities that old collation would merge")
	assertIdempotencyCollations("utf8mb4_0900_bin")
	_, _ = conflictingDown.Close()
}

func routerEnvOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

type productionErrorResponse struct {
	Error string `json:"error"`
}

type productionUserResponse struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
	Role      string `json:"role"`
}

type productionLoginResponse struct {
	Token string                 `json:"token"`
	User  productionUserResponse `json:"user"`
}

type productionIDResponse struct {
	ID int64 `json:"id"`
}

type productionAddressResponse struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Receiver  string `json:"receiver"`
	IsDefault bool   `json:"is_default"`
}

type productionCartItemResponse struct {
	ID       int64 `json:"id"`
	UserID   int64 `json:"user_id"`
	SKUID    int64 `json:"sku_id"`
	Quantity int   `json:"quantity"`
}

type productionCartListResponse struct {
	Items []productionCartItemResponse `json:"items"`
}

type productionProductDetailResponse struct {
	ID   int64 `json:"id"`
	SKUs []struct {
		ID    int64 `json:"id"`
		Stock int   `json:"stock"`
	} `json:"skus"`
}

type productionOrderItemResponse struct {
	ID        int64             `json:"id"`
	OrderNo   string            `json:"order_no"`
	SKUID     int64             `json:"sku_id"`
	ProductID int64             `json:"product_id"`
	Title     string            `json:"title"`
	Specs     map[string]string `json:"specs"`
	Price     int64             `json:"price"`
	Quantity  int               `json:"quantity"`
	Subtotal  int64             `json:"subtotal"`
}

type productionOrderResponse struct {
	OrderNo        string                        `json:"order_no"`
	UserID         int64                         `json:"user_id"`
	OrderType      string                        `json:"order_type"`
	Status         string                        `json:"status"`
	ActivityID     *int64                        `json:"activity_id"`
	PurchaseSlot   string                        `json:"purchase_slot"`
	TotalAmount    int64                         `json:"total_amount"`
	DiscountAmount int64                         `json:"discount_amount"`
	PayAmount      int64                         `json:"pay_amount"`
	Receiver       string                        `json:"receiver"`
	PaidAt         *string                       `json:"paid_at"`
	ShippedAt      *string                       `json:"shipped_at"`
	CompletedAt    *string                       `json:"completed_at"`
	CancelledAt    *string                       `json:"cancelled_at"`
	Items          []productionOrderItemResponse `json:"items"`
}

type productionPaymentResponse struct {
	PaymentID string `json:"payment_id"`
	OrderNo   string `json:"order_no"`
	UserID    int64  `json:"user_id"`
	Amount    int64  `json:"amount"`
	Result    string `json:"result"`
}

type productionActivityResponse struct {
	ID           int64  `json:"id"`
	SKUID        int64  `json:"sku_id"`
	Stock        int    `json:"stock"`
	PerUserLimit int    `json:"per_user_limit"`
	Status       string `json:"status"`
}

type productionFlashSaleListResponse struct {
	ServerTime string                                `json:"server_time"`
	Items      []productionFlashSaleListItemResponse `json:"items"`
}

type productionFlashSaleListItemResponse struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Stock int    `json:"stock"`
}

type productionPurchaseResponse struct {
	PreDeductionID string `json:"pre_deduction_id"`
	Status         string `json:"status"`
	OrderNo        string `json:"order_no"`
}

type productionPurchaseLifecycleResponse struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	OrderNo      string  `json:"order_no"`
	OrderedAt    *string `json:"ordered_at"`
	RolledBackAt *string `json:"rolled_back_at"`
}

type productionHealthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

type productionUploadResponse struct {
	URL         string `json:"url"`
	Kind        string `json:"kind"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type productionFriendRequestResponse struct {
	ID         int64  `json:"id"`
	FromUserID int64  `json:"from_user_id"`
	ToUserID   int64  `json:"to_user_id"`
	Status     string `json:"status"`
}

type productionPostResponse struct {
	ID             int64  `json:"id"`
	UserID         int64  `json:"user_id"`
	SKUID          int64  `json:"sku_id"`
	Content        string `json:"content"`
	ImageURL       string `json:"image_url"`
	AuthorUsername string `json:"author_username"`
}

type productionMessageResponse struct {
	ID              int64  `json:"id"`
	ConversationKey string `json:"conversation_key"`
	SenderID        int64  `json:"sender_id"`
	RecipientID     int64  `json:"recipient_id"`
	Type            string `json:"type"`
	URL             string `json:"url"`
}

type productionConversationResponse struct {
	Items []struct {
		ConversationKey string                    `json:"conversation_key"`
		PeerUserID      int64                     `json:"peer_user_id"`
		PeerUsername    string                    `json:"peer_username"`
		LastMessage     productionMessageResponse `json:"last_message"`
		UnreadCount     int64                     `json:"unread_count"`
	} `json:"items"`
	HasMore bool `json:"has_more"`
}

type productionMessageListResponse struct {
	Items   []productionMessageResponse `json:"items"`
	HasMore bool                        `json:"has_more"`
}

var productionPNG1x1 = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x62, 0x60, 0x01, 0x00, 0x00,
	0x00, 0xFF, 0xFF, 0x03, 0x00, 0x00, 0x06, 0x00,
	0x05, 0x57, 0xBF, 0xAB, 0xD4, 0x00, 0x00, 0x00,
	0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

func productionJSON[T any](t *testing.T, router http.Handler, method, path, token string, requestBody any, expectedStatus int) (T, map[string]json.RawMessage) {
	t.Helper()
	response := productionRequest(t, router, method, path, token, requestBody)
	require.Equalf(t, expectedStatus, response.Code, "%s %s returned %d: %s", method, path, response.Code, response.Body.String())
	require.NotEmptyf(t, response.Body.Bytes(), "%s %s must return a JSON response", method, path)

	var decoded T
	require.NoErrorf(t, json.Unmarshal(response.Body.Bytes(), &decoded), "%s %s response: %s", method, path, response.Body.String())
	var fields map[string]json.RawMessage
	require.NoErrorf(t, json.Unmarshal(response.Body.Bytes(), &fields), "%s %s response must be a JSON object", method, path)
	return decoded, fields
}

func productionRequest(t *testing.T, router http.Handler, method, path, token string, requestBody any) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if requestBody == nil {
		body = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(requestBody)
		require.NoError(t, err)
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func productionUpload(t *testing.T, router http.Handler, token, requestID, kind, filename string, content []byte) productionUploadResponse {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("kind", kind))
	part, err := writer.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	request := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", requestID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())

	var uploaded productionUploadResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &uploaded))
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fields))
	productionRequireKeys(t, fields, "url", "kind", "filename", "content_type", "size")
	require.True(t, bytes.HasPrefix([]byte(uploaded.URL), []byte("/files/")))
	require.Equal(t, kind, uploaded.Kind)
	require.Equal(t, filename, uploaded.Filename)
	require.Equal(t, int64(len(content)), uploaded.Size)
	return uploaded
}

func productionRequireMediaRead(t *testing.T, router http.Handler, token, reference string, want []byte,
	wantContentType, wantDisposition, wantFilename string,
) {
	t.Helper()
	response := productionRequest(t, router, http.MethodGet, "/api"+reference, token, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, want, response.Body.Bytes())
	require.Equal(t, wantContentType, response.Header().Get("Content-Type"))
	require.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	disposition, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	require.NoError(t, err)
	require.Equal(t, wantDisposition, disposition)
	require.Equal(t, wantFilename, params["filename"])
}

func productionRequireAPIError(t *testing.T, response *httptest.ResponseRecorder, status int, message ...string) {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	testsupport.AssertAPIError(t, body, message...)
}

func productionJSONObjects(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &items))
	return items
}

func productionJSONObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &object))
	return object
}

func productionNoContent(t *testing.T, router http.Handler, method, path, token string) {
	t.Helper()
	response := productionRequest(t, router, method, path, token, nil)
	require.Equalf(t, http.StatusNoContent, response.Code, "%s %s returned %d: %s", method, path, response.Code, response.Body.String())
	require.Empty(t, response.Body.Bytes(), "%s %s must not return a response body", method, path)
}

func productionRequireKeys(t *testing.T, fields map[string]json.RawMessage, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(fields))
	for key := range fields {
		actual = append(actual, key)
	}
	require.ElementsMatch(t, expected, actual)
}

func productionWaitForHealth(t *testing.T, router http.Handler, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		response := productionRequest(t, router, http.MethodGet, "/healthz", "", nil)
		lastStatus, lastBody = response.Code, response.Body.String()
		if response.Code == http.StatusOK {
			var health productionHealthResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &health))
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fields))
			productionRequireKeys(t, fields, "status", "checks")
			require.Equal(t, "ok", health.Status)
			require.Equal(t, map[string]string{"mysql": "ok", "redis": "ok", "mq": "ok"}, health.Checks)
			return
		}
		<-ticker.C
	}
	require.FailNowf(t, "production router did not become healthy", "last status=%d body=%s", lastStatus, lastBody)
}

func productionRegisterAndLogin(t *testing.T, router http.Handler, username string) (int64, string) {
	t.Helper()
	const password = "secret123"
	registered, registeredFields := productionJSON[productionUserResponse](t, router, http.MethodPost, "/api/auth/register", "",
		map[string]any{"username": username, "password": password}, http.StatusCreated)
	productionRequireKeys(t, registeredFields, "id", "username", "nickname", "avatar_url", "role", "created_at", "updated_at")
	require.Positive(t, registered.ID)
	require.Equal(t, username, registered.Username)
	require.Equal(t, "user", registered.Role)
	return registered.ID, productionLogin(t, router, username, password, "user")
}

func productionLogin(t *testing.T, router http.Handler, username, password, role string) string {
	t.Helper()
	login, fields := productionJSON[productionLoginResponse](t, router, http.MethodPost, "/api/auth/login", "",
		map[string]any{"username": username, "password": password}, http.StatusOK)
	productionRequireKeys(t, fields, "token", "user")
	var userFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["user"], &userFields))
	productionRequireKeys(t, userFields, "id", "username", "nickname", "avatar_url", "role", "created_at", "updated_at")
	require.NotEmpty(t, login.Token)
	require.Equal(t, username, login.User.Username)
	require.Equal(t, role, login.User.Role)
	return login.Token
}

func productionExerciseMediaAuthorization(t *testing.T, router http.Handler, runID string,
	ownerID int64, ownerToken string, purchasedSKUID int64,
) {
	t.Helper()
	peerID, peerToken := productionRegisterAndLogin(t, router, "peer"+runID)
	_, outsiderToken := productionRegisterAndLogin(t, router, "outside"+runID)

	friendRequest, friendFields := productionJSON[productionFriendRequestResponse](t, router, http.MethodPost,
		"/api/friend-requests", ownerToken, map[string]any{"to_user_id": peerID}, http.StatusCreated)
	productionRequireKeys(t, friendFields, "id", "from_user_id", "to_user_id", "status", "created_at", "updated_at")
	require.Equal(t, ownerID, friendRequest.FromUserID)
	require.Equal(t, peerID, friendRequest.ToUserID)
	require.Equal(t, "pending", friendRequest.Status)
	productionNoContent(t, router, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", friendRequest.ID), peerToken)

	avatar := productionUpload(t, router, ownerToken, "router-avatar-"+runID, file.KindImage, "avatar.png", productionPNG1x1)
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+avatar.URL, outsiderToken, nil),
		http.StatusForbidden, "file access forbidden")
	profile, profileFields := productionJSON[productionUserResponse](t, router, http.MethodPatch, "/api/users/me", ownerToken,
		map[string]any{"avatar_url": avatar.URL}, http.StatusOK)
	productionRequireKeys(t, profileFields, "id", "username", "nickname", "avatar_url", "role", "created_at", "updated_at")
	require.Equal(t, avatar.URL, profile.AvatarURL)
	productionRequireMediaRead(t, router, outsiderToken, avatar.URL, productionPNG1x1, "image/png", "inline", "avatar.png")

	postImage := productionUpload(t, router, ownerToken, "router-post-"+runID, file.KindImage, "post.png", productionPNG1x1)
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+postImage.URL, peerToken, nil),
		http.StatusForbidden, "file access forbidden")
	post, postFields := productionJSON[productionPostResponse](t, router, http.MethodPost, "/api/posts", ownerToken,
		map[string]any{"sku_id": purchasedSKUID, "content": "production media post", "image_url": postImage.URL}, http.StatusCreated)
	productionRequireKeys(t, postFields, "id", "user_id", "sku_id", "content", "image_url", "created_at", "updated_at")
	require.Equal(t, ownerID, post.UserID)
	require.Equal(t, purchasedSKUID, post.SKUID)
	require.Equal(t, postImage.URL, post.ImageURL)

	feed, feedFields := productionJSON[struct {
		Items []productionPostResponse `json:"items"`
		Total int64                    `json:"total"`
	}](t, router, http.MethodGet, "/api/posts/feed?page=1&page_size=10", peerToken, nil, http.StatusOK)
	productionRequireKeys(t, feedFields, "items", "total")
	require.Len(t, feed.Items, 1)
	require.Equal(t, post.ID, feed.Items[0].ID)
	require.NotEmpty(t, feed.Items[0].AuthorUsername)
	feedItemFields := productionJSONObjects(t, feedFields["items"])
	require.Len(t, feedItemFields, 1)
	productionRequireKeys(t, feedItemFields[0],
		"id", "user_id", "sku_id", "content", "image_url", "created_at", "updated_at", "author_username")
	productionRequireMediaRead(t, router, peerToken, postImage.URL, productionPNG1x1, "image/png", "inline", "post.png")
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+postImage.URL, outsiderToken, nil),
		http.StatusForbidden, "file access forbidden")
	productionNoContent(t, router, http.MethodDelete, fmt.Sprintf("/api/posts/%d", post.ID), ownerToken)
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+postImage.URL, peerToken, nil),
		http.StatusForbidden, "file access forbidden")

	pdf := []byte("%PDF-1.7\nproduction router media\n")
	chatFile := productionUpload(t, router, ownerToken, "router-chat-file-"+runID, file.KindFile, "router-media.pdf", pdf)
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+chatFile.URL, peerToken, nil),
		http.StatusForbidden, "file access forbidden")
	messageBody := map[string]any{
		"to_user_id": peerID, "type": "file", "url": chatFile.URL, "client_request_id": "router-message-" + runID,
	}
	message, messageFields := productionJSON[productionMessageResponse](t, router, http.MethodPost, "/api/messages", ownerToken,
		messageBody, http.StatusCreated)
	productionRequireKeys(t, messageFields, "id", "conversation_key", "sender_id", "recipient_id", "type", "url", "created_at")
	require.Equal(t, ownerID, message.SenderID)
	require.Equal(t, peerID, message.RecipientID)
	require.Equal(t, "file", message.Type)
	require.Equal(t, chatFile.URL, message.URL)
	replayed, replayFields := productionJSON[productionMessageResponse](t, router, http.MethodPost, "/api/messages", ownerToken,
		messageBody, http.StatusOK)
	productionRequireKeys(t, replayFields, "id", "conversation_key", "sender_id", "recipient_id", "type", "url", "created_at")
	require.Equal(t, message.ID, replayed.ID)

	productionRequireMediaRead(t, router, peerToken, chatFile.URL, pdf, "application/pdf", "attachment", "router-media.pdf")
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+chatFile.URL, outsiderToken, nil),
		http.StatusForbidden, "file access forbidden")
	productionRequireAPIError(t, productionRequest(t, router, http.MethodGet, "/api"+chatFile.URL, "", nil),
		http.StatusUnauthorized, "missing token")

	conversations, conversationFields := productionJSON[productionConversationResponse](t, router, http.MethodGet,
		"/api/conversations", peerToken, nil, http.StatusOK)
	productionRequireKeys(t, conversationFields, "items", "has_more")
	require.Len(t, conversations.Items, 1)
	require.Equal(t, message.ConversationKey, conversations.Items[0].ConversationKey)
	require.Equal(t, ownerID, conversations.Items[0].PeerUserID)
	require.Equal(t, int64(1), conversations.Items[0].UnreadCount)
	conversationItems := productionJSONObjects(t, conversationFields["items"])
	require.Len(t, conversationItems, 1)
	productionRequireKeys(t, conversationItems[0],
		"conversation_key", "peer_user_id", "peer_username", "last_message", "unread_count")
	productionRequireKeys(t, productionJSONObject(t, conversationItems[0]["last_message"]),
		"id", "conversation_key", "sender_id", "recipient_id", "type", "url", "created_at")

	messages, messagesFields := productionJSON[productionMessageListResponse](t, router, http.MethodGet,
		"/api/conversations/"+message.ConversationKey+"/messages", peerToken, nil, http.StatusOK)
	productionRequireKeys(t, messagesFields, "items", "has_more")
	require.Len(t, messages.Items, 1)
	require.Equal(t, message.ID, messages.Items[0].ID)
	messageItems := productionJSONObjects(t, messagesFields["items"])
	require.Len(t, messageItems, 1)
	productionRequireKeys(t, messageItems[0],
		"id", "conversation_key", "sender_id", "recipient_id", "type", "url", "created_at")
}

func productionCreateAddress(t *testing.T, router http.Handler, token, receiver string) int64 {
	t.Helper()
	address, fields := productionJSON[productionAddressResponse](t, router, http.MethodPost, "/api/addresses", token,
		map[string]any{
			"receiver": receiver, "phone": "13800138000", "province": "Guangdong", "city": "Shenzhen",
			"district": "Nanshan", "detail": "Production Router Road 1", "is_default": true,
		}, http.StatusCreated)
	productionRequireKeys(t, fields,
		"id", "user_id", "receiver", "phone", "province", "city", "district", "detail", "is_default", "created_at", "updated_at")
	require.Positive(t, address.ID)
	require.Positive(t, address.UserID)
	require.Equal(t, receiver, address.Receiver)
	require.True(t, address.IsDefault)
	return address.ID
}

func productionCreateOnSaleSKU(t *testing.T, router http.Handler, adminToken, name string, price int64, stock int) (int64, int64) {
	t.Helper()
	category, categoryFields := productionJSON[productionIDResponse](t, router, http.MethodPost, "/api/admin/categories", adminToken,
		map[string]any{"name": "category-" + name}, http.StatusCreated)
	productionRequireKeys(t, categoryFields, "id", "name", "created_at", "updated_at")
	require.Positive(t, category.ID)

	product, productFields := productionJSON[productionIDResponse](t, router, http.MethodPost, "/api/admin/products", adminToken,
		map[string]any{"category_id": category.ID, "title": "product-" + name, "description": "production router tracer"}, http.StatusCreated)
	productionRequireKeys(t, productFields, "id", "category_id", "title", "description", "status", "created_at", "updated_at")
	require.Positive(t, product.ID)

	sku, skuFields := productionJSON[productionIDResponse](t, router, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/skus", product.ID), adminToken,
		map[string]any{"specs": map[string]string{"color": "router-blue"}, "price": price, "stock": stock}, http.StatusCreated)
	productionRequireKeys(t, skuFields, "id", "product_id", "specs", "price", "stock", "created_at", "updated_at")
	require.Positive(t, sku.ID)
	productionNoContent(t, router, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/publish", product.ID), adminToken)
	return product.ID, sku.ID
}

func productionRequireOrderItem(t *testing.T, item productionOrderItemResponse, orderNo string, productID, skuID, price int64, quantity int) {
	t.Helper()
	require.Positive(t, item.ID)
	require.Equal(t, orderNo, item.OrderNo)
	require.Equal(t, productID, item.ProductID)
	require.Equal(t, skuID, item.SKUID)
	require.NotEmpty(t, item.Title)
	require.Equal(t, "router-blue", item.Specs["color"])
	require.Equal(t, price, item.Price)
	require.Equal(t, quantity, item.Quantity)
	require.Equal(t, price*int64(quantity), item.Subtotal)
}

func productionSKUStock(t *testing.T, product productionProductDetailResponse, skuID int64) int {
	t.Helper()
	for _, sku := range product.SKUs {
		if sku.ID == skuID {
			return sku.Stock
		}
	}
	require.FailNowf(t, "SKU missing from product detail", "sku_id=%d product_id=%d", skuID, product.ID)
	return 0
}

func productionFlashSaleItem(t *testing.T, router http.Handler, token string, activityID int64) productionFlashSaleListItemResponse {
	t.Helper()
	list, fields := productionJSON[productionFlashSaleListResponse](t, router, http.MethodGet, "/api/flashsales", token, nil, http.StatusOK)
	productionRequireKeys(t, fields, "server_time", "items")
	itemFields := productionJSONObjects(t, fields["items"])
	require.Len(t, itemFields, len(list.Items))
	for index, item := range list.Items {
		if item.ID == activityID {
			productionRequireFlashSaleItemContract(t, itemFields[index])
			return item
		}
	}
	require.FailNowf(t, "flash sale missing from user list", "activity_id=%d", activityID)
	return productionFlashSaleListItemResponse{}
}

func productionRequireFlashSaleItemContract(t *testing.T, fields map[string]json.RawMessage) {
	t.Helper()
	productionRequireKeys(t, fields,
		"id", "sku_id", "title", "price", "stock", "per_user_limit", "status", "start_at", "end_at",
		"created_at", "updated_at", "state", "product_title", "sku")
	productionRequireKeys(t, productionJSONObject(t, fields["sku"]), "id", "product_id", "specs", "price")
}

func productionPurchaseAccepted(t *testing.T, router http.Handler, token string, activityID int64, requestID string, timeout time.Duration) (productionPurchaseResponse, map[string]json.RawMessage) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	path := fmt.Sprintf("/api/flashsales/%d/purchase", activityID)
	body := map[string]any{"client_request_id": requestID}
	for {
		response := productionRequest(t, router, http.MethodPost, path, token, body)
		if response.Code == http.StatusAccepted {
			var purchase productionPurchaseResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &purchase))
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fields))
			return purchase, fields
		}
		if response.Code != http.StatusServiceUnavailable || !bytes.Contains(response.Body.Bytes(), []byte("flashsale recovery incomplete")) {
			require.FailNowf(t, "purchase request failed", "POST %s returned %d: %s", path, response.Code, response.Body.String())
		}
		if time.Now().After(deadline) {
			require.FailNowf(t, "flash-sale recovery gate did not open", "last response: %s", response.Body.String())
		}
		<-ticker.C
	}
}

func productionPollPurchaseOrdered(t *testing.T, router http.Handler, token, preDeductionID string, timeout time.Duration) productionPurchaseLifecycleResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last productionPurchaseLifecycleResponse
	for time.Now().Before(deadline) {
		lifecycle, fields := productionJSON[productionPurchaseLifecycleResponse](t, router, http.MethodGet,
			"/api/flashsales/purchases/"+preDeductionID, token, nil, http.StatusOK)
		last = lifecycle
		if lifecycle.Status == "ordered" {
			productionRequireKeys(t, fields, "id", "status", "order_no", "created_at", "updated_at", "ordered_at")
			require.Equal(t, preDeductionID, lifecycle.ID)
			require.NotEmpty(t, lifecycle.OrderNo)
			require.NotNil(t, lifecycle.OrderedAt)
			return lifecycle
		}
		require.NotEqual(t, "rolled_back", lifecycle.Status, "purchase was rolled back before ordering")
		<-ticker.C
	}
	require.FailNowf(t, "flash-sale purchase did not reach ordered", "pre_deduction_id=%s last_status=%s", preDeductionID, last.Status)
	return productionPurchaseLifecycleResponse{}
}

func productionRequireSeckillOrder(t *testing.T, order productionOrderResponse, activityID int64, purchaseSlot string, skuID int64) {
	t.Helper()
	require.Equal(t, "seckill", order.OrderType)
	require.Equal(t, "pending_payment", order.Status)
	require.NotNil(t, order.ActivityID)
	require.Equal(t, activityID, *order.ActivityID)
	require.Equal(t, purchaseSlot, order.PurchaseSlot)
	require.Equal(t, int64(1900), order.PayAmount)
	require.Zero(t, order.DiscountAmount)
	require.Len(t, order.Items, 1)
	require.Equal(t, skuID, order.Items[0].SKUID)
	require.Equal(t, int64(1900), order.Items[0].Price)
	require.Equal(t, 1, order.Items[0].Quantity)
}
