// 集成测试（主 seam）：真实 MySQL + Redis（docker compose）+ httptest 起完整路由，
// 覆盖下单闭环（直购/购物车结算）、幂等、库存不足、券门槛、金额计算、
// 取消回补（库存+券）、状态机非法跃迁拒绝、发货/确认收货与跨用户越权。
package order_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	carthandler "github.com/xiangzhang-coding/go-single/internal/cart/handler"
	cartrepo "github.com/xiangzhang-coding/go-single/internal/cart/repository"
	cartsvc "github.com/xiangzhang-coding/go-single/internal/cart/service"
	couponhandler "github.com/xiangzhang-coding/go-single/internal/coupon/handler"
	couponmodel "github.com/xiangzhang-coding/go-single/internal/coupon/model"
	couponrepo "github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	orderhandler "github.com/xiangzhang-coding/go-single/internal/order/handler"
	"github.com/xiangzhang-coding/go-single/internal/order/model"
	orderrepo "github.com/xiangzhang-coding/go-single/internal/order/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	paymenthandler "github.com/xiangzhang-coding/go-single/internal/payment/handler"
	paymentrepo "github.com/xiangzhang-coding/go-single/internal/payment/repository"
	paymentsvc "github.com/xiangzhang-coding/go-single/internal/payment/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/snowflake"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
	producthandler "github.com/xiangzhang-coding/go-single/internal/product/handler"
	productrepo "github.com/xiangzhang-coding/go-single/internal/product/repository"
	productsvc "github.com/xiangzhang-coding/go-single/internal/product/service"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	testDBName    = "go_shop_test"
	testSecret    = "integration-test-secret"
	migrationsDir = "../../migrations"
	redisAddr     = "127.0.0.1:6379"
	// redisTestDB 各测试包独占一个 Redis DB（15-20），避免 go test ./... 并行时
	// 彼此 FlushDB 清掉对方的秒杀库存/幂等键等测试数据（跨包污染）。
	redisTestDB = 18
)

// noopActivity 秒杀活动库存端口替身：本包不触达秒杀落单，恒成功即可。
type noopActivity struct{}

func (noopActivity) DeductStock(context.Context, *transaction.Handle, int64, int) (bool, error) {
	return true, nil
}

func (noopActivity) RestoreStock(context.Context, *transaction.Handle, int64, int) error {
	return nil
}
func (noopActivity) RestoreRedis(context.Context, int64, int64, int) error { return nil }

// testEnv 每个测试包只构建一次；MySQL 或 Redis 不可达时本地跳过、CI 失败。
type testEnv struct {
	router   http.Handler
	verifier auth.TokenVerifier
	redis    *redis.Client
	gdb      *gorm.DB
	orderSvc ordersvc.Service
}

var (
	envOnce sync.Once
	env     *testEnv
	envErr  error
)

func requireEnv(t *testing.T) *testEnv {
	t.Helper()
	envOnce.Do(func() { env, envErr = buildEnv() })
	testsupport.RequireDependency(t, "MySQL/Redis", envErr)
	return env
}

func buildEnv() (*testEnv, error) {
	dsn := testDSN("")
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}
	rootDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/", envOr("GO_SINGLE_MYSQL_ROOT_USER", "root"), envOr("GO_SINGLE_MYSQL_ROOT_PASSWORD", "root123"), envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"), envOr("GO_SINGLE_MYSQL_PORT", "3306"))
	rootConn, err := sql.Open("mysql", rootDSN)
	if err != nil {
		return nil, err
	}
	defer rootConn.Close()
	if _, err := rootConn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+testDBName); err != nil {
		return nil, fmt.Errorf("创建测试库: %w", err)
	}
	if _, err := rootConn.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+testDBName+".* TO '"+envOr("GO_SINGLE_MYSQL_USER", "shop")+"'@'%'"); err != nil {
		return nil, fmt.Errorf("授权测试库: %w", err)
	}

	m, err := migrate.New("file://"+migrationsDir, "mysql://"+testDSN(testDBName))
	if err != nil {
		return nil, err
	}
	defer m.Close()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return nil, fmt.Errorf("执行迁移: %w", err)
	}

	gdb, err := gorm.Open(mysql.Open(testDSN(testDBName)), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, err
	}

	// Redis：测试专用 DB，先清空避免跨包污染。
	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DB: redisTestDB})
	redisCtx, redisCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer redisCancel()
	if err := rc.Ping(redisCtx).Err(); err != nil {
		return nil, fmt.Errorf("Redis 连接失败: %w", err)
	}
	if err := rc.FlushDB(redisCtx).Err(); err != nil {
		return nil, err
	}
	cacheClient, err := cache.NewRedis(redisAddr, "", redisTestDB)
	if err != nil {
		return nil, err
	}

	verifier := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})

	productStore := productrepo.Store{
		Category: productrepo.NewGORMCategory(gdb),
		Product:  productrepo.NewGORMProduct(gdb),
		SKU:      productrepo.NewGORMSKU(gdb),
	}
	productSvc := productsvc.New(productStore, cacheClient, zap.NewNop())

	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier)
	userHandler := userhandler.New(userSvc, verifier, testsupport.AllowAllAuthAttempts{})
	addressHandler := userhandler.NewAddress(userSvc, verifier)
	productHandler := producthandler.New(productSvc, verifier)
	cartSvc := cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(gdb)}, productSvc)
	cartHandler := carthandler.New(cartSvc, verifier)
	couponSvc := couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(gdb), UserCoupon: couponrepo.NewGORMUserCoupon(gdb)}, cacheClient, metrics.New().Business())
	couponHandler := couponhandler.New(couponSvc, verifier)

	orderNoGen, err := snowflake.New(1)
	if err != nil {
		return nil, err
	}
	orderStore := orderrepo.NewGORMOrder(gdb)
	orderSvc := ordersvc.New(orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(gdb), Tx: orderStore},
		cacheClient, orderNoGen, productSvc, couponSvc, cartSvc, userSvc, metrics.New().Business())
	orderHandler := orderhandler.New(orderSvc, verifier)
	paymentStore := paymentrepo.NewGORMPayment(gdb)
	paymentHandler := paymenthandler.New(
		paymentsvc.New(paymentrepo.Store{Payments: paymentStore, Tx: paymentStore}, orderSvc, metrics.New().Business()),
		verifier,
	)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	addressHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
	cartHandler.RegisterRoutes(api)
	couponHandler.RegisterRoutes(api)
	orderHandler.RegisterRoutes(api)
	paymentHandler.RegisterRoutes(api)
	return &testEnv{router: r, verifier: verifier, redis: rc, gdb: gdb, orderSvc: orderSvc}, nil
}

func testDSN(dbName string) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		envOr("GO_SINGLE_MYSQL_USER", "shop"),
		envOr("GO_SINGLE_MYSQL_PASSWORD", "shop123"),
		envOr("GO_SINGLE_MYSQL_HOST", "127.0.0.1"),
		envOr("GO_SINGLE_MYSQL_PORT", "3306"),
		dbName,
	)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---- 请求助手 ----

func doJSON(t *testing.T, env *testEnv, method, path, body, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := performJSON(env, method, path, body, token)

	var parsed map[string]any
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &parsed))
	}
	return w, parsed
}

func performJSON(env *testEnv, method, path, body, token string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, r)
	return w
}

func adminToken(t *testing.T, env *testEnv) string {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/auth/login", `{"username":"admin","password":"admin123"}`, "")
	require.Equal(t, http.StatusOK, w.Code)
	tok, ok := body["token"].(string)
	require.True(t, ok)
	return tok
}

func registerAndToken(t *testing.T, env *testEnv, username string) string {
	t.Helper()
	w, _ := doJSON(t, env, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	require.Equal(t, http.StatusCreated, w.Code)
	_, login := doJSON(t, env, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	return login["token"].(string)
}

func uniqueName(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()) }

// onSaleSKU 组装一条可售 SKU 并返回 (productID, skuID)。
func onSaleSKU(t *testing.T, env *testEnv, price int64, stock int) (int64, int64) {
	t.Helper()
	token := adminToken(t, env)
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/categories", fmt.Sprintf(`{"name":%q}`, uniqueName("数码")), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建类目失败: %s", w.Body.String())
	catID := int64(body["id"].(float64))

	w, body = doJSON(t, env, http.MethodPost, "/api/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":%q,"description":"desc"}`, catID, uniqueName("手机")), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建商品失败: %s", w.Body.String())
	productID := int64(body["id"].(float64))

	w, body = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/skus", productID),
		fmt.Sprintf(`{"specs":{"color":"红"},"price":%d,"stock":%d}`, price, stock), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建 SKU 失败: %s", w.Body.String())
	skuID := int64(body["id"].(float64))

	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/publish", productID), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	return productID, skuID
}

// address 新建地址并返回 (addressID)。
func address(t *testing.T, env *testEnv, token string) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/addresses",
		`{"receiver":"张三","phone":"13800138000","province":"广东省","city":"深圳市","district":"南山区","detail":"科技园 1 号","is_default":true}`, token)
	require.Equal(t, http.StatusCreated, w.Code, "创建地址失败: %s", w.Body.String())
	return int64(body["id"].(float64))
}

// thresholdCoupon 发布并领取一张满减券，返回 userCouponID。
func thresholdCoupon(t *testing.T, env *testEnv, token string, value, minAmount int64) int64 {
	t.Helper()
	tmplID := createTemplate(t, env, "threshold", value, minAmount)
	w, body := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", token)
	require.Equal(t, http.StatusCreated, w.Code, "领券失败: %s", w.Body.String())
	return int64(body["id"].(float64))
}

func createTemplate(t *testing.T, env *testEnv, typ string, value, minAmount int64) int64 {
	t.Helper()
	token := adminToken(t, env)
	validFrom := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	validUntil := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/coupons",
		fmt.Sprintf(`{"name":%q,"type":%q,"value":%d,"min_amount":%d,"total":100,"per_user_limit":1,"valid_from":%q,"valid_until":%q}`,
			uniqueName("满减券"), typ, value, minAmount, validFrom, validUntil), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建券模板失败: %s", w.Body.String())
	return int64(body["id"].(float64))
}

// createOrder 直购下单，返回订单响应。
func createOrder(t *testing.T, env *testEnv, token, rid string, addrID int64, skuID int64, quantity int, couponID int64) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return doJSON(t, env, http.MethodPost, "/api/orders", orderRequestBody(rid, addrID, skuID, quantity, couponID), token)
}

func orderRequestBody(rid string, addrID int64, skuID int64, quantity int, couponID int64) string {
	couponPart := ""
	if couponID > 0 {
		couponPart = fmt.Sprintf(`,"coupon_id":%d`, couponID)
	}
	return fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"from_cart":false,"items":[{"sku_id":%d,"quantity":%d}]%s}`, rid, addrID, skuID, quantity, couponPart)
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}

func skuStock(t *testing.T, env *testEnv, skuID int64) int {
	t.Helper()
	// 直查 DB：绕过商品详情缓存。
	var stock int
	require.NoError(t, env.gdb.Raw("SELECT stock FROM skus WHERE id = ?", skuID).Scan(&stock).Error)
	return stock
}

func orderByNo(t *testing.T, env *testEnv, orderNo string) model.Order {
	t.Helper()
	var o model.Order
	require.NoError(t, env.gdb.First(&o, "order_no = ?", orderNo).Error)
	return o
}

// countOrdersByUser 该用户的订单数（测试库跨用例共享，须按用户统计）。
func countOrdersByUser(t *testing.T, env *testEnv, username string) int64 {
	t.Helper()
	var n int64
	err := env.gdb.Model(&model.Order{}).
		Where("user_id = (SELECT id FROM users WHERE username = ?)", username).
		Count(&n).Error
	require.NoError(t, err)
	return n
}

// ---- 测试 ----

// 鉴权：未登录访问订单接口一律 401。
func TestOrderRequiresAuth(t *testing.T) {
	env := requireEnv(t)

	w, _ := doJSON(t, env, http.MethodPost, "/api/orders", `{"client_request_id":"x","address_id":1,"items":[{"sku_id":1,"quantity":1}]}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/1", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/1/cancel", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/1/confirm", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/orders/1/ship", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// 直购闭环：库存/订单项/地址快照/金额全部正确；详情与列表可查。
func TestOrderDirectBuyHappyPath(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("buyer"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2, 0)
	require.Equal(t, http.StatusCreated, w.Code, "下单失败: %s", w.Body.String())
	require.ElementsMatch(t, []string{
		"order_no", "user_id", "order_type", "status", "total_amount", "discount_amount", "pay_amount",
		"receiver", "phone", "province", "city", "district", "detail", "expire_at", "created_at", "updated_at", "items",
	}, mapKeys(body))
	orderNo := body["order_no"].(string)
	require.Equal(t, "pending_payment", body["status"])
	require.Equal(t, "normal", body["order_type"])
	require.Equal(t, float64(19800), body["total_amount"])
	require.Equal(t, float64(0), body["discount_amount"])
	require.Equal(t, float64(19800), body["pay_amount"])
	// 地址快照。
	require.Equal(t, "张三", body["receiver"])
	require.Equal(t, "13800138000", body["phone"])
	require.Equal(t, "南山区", body["district"])
	require.NotEmpty(t, body["expire_at"], "应有超时时间")

	items := body["items"].([]any)
	require.Len(t, items, 1)
	it := items[0].(map[string]any)
	require.ElementsMatch(t, []string{
		"id", "order_no", "sku_id", "product_id", "title", "specs", "price", "quantity", "subtotal", "created_at", "updated_at",
	}, mapKeys(it))
	require.Equal(t, float64(skuID), it["sku_id"])
	require.Equal(t, float64(9900), it["price"])
	require.Equal(t, float64(2), it["quantity"])
	require.Equal(t, float64(19800), it["subtotal"])
	require.NotEmpty(t, it["title"])

	// 库存扣减：10 - 2 = 8。
	require.Equal(t, 8, skuStock(t, env, skuID))

	// 详情（owner 可查）。
	w, detail := doJSON(t, env, http.MethodGet, "/api/orders/"+orderNo, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, orderNo, detail["order_no"])
	require.Len(t, detail["items"].([]any), 1)

	// 列表含该订单。
	w, list := doJSON(t, env, http.MethodGet, "/api/orders?page=1&page_size=20", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), list["total"])
	require.Equal(t, orderNo, list["orders"].([]any)[0].(map[string]any)["order_no"])
}

func TestOrderMaximumPriceAndQuantityBoundary(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("maxamount"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 100_000_000, 99)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 99, 0)
	require.Equal(t, http.StatusCreated, w.Code, "最大金额边界下单失败: %s", w.Body.String())
	require.Equal(t, float64(9_900_000_000), body["total_amount"])
	require.Equal(t, float64(9_900_000_000), body["pay_amount"])
	item := body["items"].([]any)[0].(map[string]any)
	require.Equal(t, float64(9_900_000_000), item["subtotal"])
	require.Zero(t, skuStock(t, env, skuID))
}

// 幂等：同一 client_request_id 重复提交只生成一单并返回同一订单号。
func TestOrderIdempotency(t *testing.T) {
	env := requireEnv(t)
	username := uniqueName("retryer")
	token := registerAndToken(t, env, username)
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	rid := uniqueName("req")

	w, body := createOrder(t, env, token, rid, addrID, skuID, 2, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)

	w, body = createOrder(t, env, token, rid, addrID, skuID, 2, 0)
	require.Equal(t, http.StatusOK, w.Code, "重复提交应返回 200 与同一订单号")
	require.Equal(t, orderNo, body["order_no"])

	require.Equal(t, int64(1), countOrdersByUser(t, env, username), "重复提交只生成一单")
	require.Equal(t, 8, skuStock(t, env, skuID), "库存只扣一次")

	// 不同用户可用相同 client_request_id（幂等键按用户隔离）。
	other := registerAndToken(t, env, uniqueName("other"))
	addr2 := address(t, env, other)
	_, sku2 := onSaleSKU(t, env, 100, 5)
	w, _ = createOrder(t, env, other, rid, addr2, sku2, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code, "不同用户相同 client_request_id 不应冲突")
}

func TestOrderIdempotencySurvivesRedisLoss(t *testing.T) {
	env := requireEnv(t)
	username := uniqueName("durable-idem")
	token := registerAndToken(t, env, username)
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	rid := uniqueName("durable-request")

	w, first := createOrder(t, env, token, rid, addrID, skuID, 2, 0)
	require.Equal(t, http.StatusCreated, w.Code, "首次下单失败: %s", w.Body.String())
	require.NoError(t, env.redis.FlushDB(context.Background()).Err())

	w, replay := createOrder(t, env, token, rid, addrID, skuID, 2, 0)
	require.Equal(t, http.StatusOK, w.Code, "Redis 丢失后应由 MySQL 返回原订单: %s", w.Body.String())
	require.Equal(t, first["order_no"], replay["order_no"])
	require.Equal(t, int64(1), countOrdersByUser(t, env, username))
	require.Equal(t, 8, skuStock(t, env, skuID), "库存只应扣一次")

	duplicate := orderByNo(t, env, first["order_no"].(string))
	duplicate.OrderNo = fmt.Sprintf("%d", time.Now().UnixNano())
	err := env.gdb.Create(&duplicate).Error
	require.Error(t, err, "数据库唯一约束必须拒绝同用户和 client_request_id 的第二单")
	var mysqlErr *drivermysql.MySQLError
	require.ErrorAs(t, err, &mysqlErr)
	require.Equal(t, uint16(1062), mysqlErr.Number)
}

// 库存不足：下单失败 409，库存不变，不生成订单。
func TestOrderInsufficientStock(t *testing.T) {
	env := requireEnv(t)
	username := uniqueName("stockless")
	token := registerAndToken(t, env, username)
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 1)

	w, errorBody := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2, 0)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, map[string]any{"error": "insufficient stock"}, errorBody)
	require.Equal(t, 1, skuStock(t, env, skuID))

	require.Equal(t, int64(0), countOrdersByUser(t, env, username), "库存不足不得生成订单")

	// 幂等键已释放：修正数量后可重试成功。
	w, body := createOrder(t, env, token, uniqueName("req2"), addrID, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, float64(9900), body["pay_amount"])
}

// 购物车结算：订单包含全部条目、购物车清空、库存扣减。
func TestOrderFromCartCheckout(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("carter"))
	addrID := address(t, env, token)
	_, skuA := onSaleSKU(t, env, 1000, 10)
	_, skuB := onSaleSKU(t, env, 2000, 5)

	w, _ := doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":2}`, skuA), token)
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, skuB), token)
	require.Equal(t, http.StatusCreated, w.Code)

	w, body := doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"from_cart":true}`, uniqueName("req"), addrID), token)
	require.Equal(t, http.StatusCreated, w.Code, "结算失败: %s", w.Body.String())
	require.Equal(t, float64(4000), body["total_amount"], "1000*2 + 2000*1")
	require.Len(t, body["items"].([]any), 2)

	// 购物车已清空。
	w, list := doJSON(t, env, http.MethodGet, "/api/cart", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, list["items"].([]any))
	require.Equal(t, 8, skuStock(t, env, skuA))
	require.Equal(t, 4, skuStock(t, env, skuB))
}

// 购物车结算读取必须在同一事务内加锁：先由另一事务改量并持锁，
// 结算应等待提交后读取最新数量，而不是使用旧快照再删除当前行。
func TestOrderCartCheckoutUsesLockedCurrentQuantity(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("cart-lock"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	w, _ := doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, skuID), token)
	require.Equal(t, http.StatusCreated, w.Code)

	lockTx := env.gdb.Begin()
	require.NoError(t, lockTx.Error)
	defer lockTx.Rollback()
	// 当前测试创建了唯一 SKU，按 SKU 更新该条目并持有行锁。
	require.NoError(t, lockTx.Exec("UPDATE cart_items SET quantity = 3 WHERE sku_id = ?", skuID).Error)

	type response struct {
		w    *httptest.ResponseRecorder
		body map[string]any
	}
	result := make(chan response, 1)
	go func() {
		w, body := doJSON(t, env, http.MethodPost, "/api/orders",
			fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"from_cart":true}`, uniqueName("req"), addrID), token)
		result <- response{w: w, body: body}
	}()

	select {
	case got := <-result:
		t.Fatalf("购物车行尚未解锁，结算不应提前完成，状态码=%d body=%s", got.w.Code, got.w.Body.String())
	case <-time.After(200 * time.Millisecond):
	}

	require.NoError(t, lockTx.Commit().Error)
	got := <-result
	require.Equal(t, http.StatusCreated, got.w.Code, "解锁后结算应成功: %s", got.w.Body.String())
	require.Equal(t, float64(29700), got.body["total_amount"], "应使用锁释放后的最新数量 3")
	require.Equal(t, 7, skuStock(t, env, skuID))
}

// 券不可用：不存在 404 / 已用 409 / 已过期 409（跨模块错误正确映射 HTTP 状态码）。
func TestOrderCouponUnusableHTTP(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("couponbad"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	// 不存在的券 → 404。
	w, _ := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 999999)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 已核销的券 → 409。
	couponID := thresholdCoupon(t, env, token, 5000, 5000)
	w, _ = createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, couponID)
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = createOrder(t, env, token, uniqueName("req2"), addrID, skuID, 1, couponID)
	require.Equal(t, http.StatusConflict, w.Code)

	// 已过期的券 → 409（领券后把模板有效期改到过去；时间经 Go 传入与存储墙钟同源）。
	couponID2 := thresholdCoupon(t, env, token, 5000, 5000)
	require.NoError(t, env.gdb.Exec(
		"UPDATE coupon_templates SET valid_until = ? WHERE id = "+
			"(SELECT template_id FROM user_coupons WHERE id = ?)", time.Now().Add(-time.Minute), couponID2).Error)
	w, _ = createOrder(t, env, token, uniqueName("req3"), addrID, skuID, 1, couponID2)
	require.Equal(t, http.StatusConflict, w.Code)
}

func TestOrderConcurrentCouponUseOnlyOnce(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("coupon-race"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	couponID := thresholdCoupon(t, env, token, 5000, 5000)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := orderRequestBody(uniqueName(fmt.Sprintf("coupon-use-%d", i)), addrID, skuID, 1, couponID)
			w := performJSON(env, http.MethodPost, "/api/orders", body, token)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	sort.Ints(codes)
	require.Equal(t, []int{http.StatusCreated, http.StatusConflict}, codes)
	var orderCount int64
	require.NoError(t, env.gdb.Table("orders").Where("coupon_id = ?", couponID).Count(&orderCount).Error)
	require.Equal(t, int64(1), orderCount, "同一张优惠券并发核销只能创建一个订单")
}

// 券归属校验：用户不能在订单中使用他人的券（防止跨用户越权）。
func TestOrderCouponOwnership(t *testing.T) {
	env := requireEnv(t)
	alice := registerAndToken(t, env, uniqueName("coupon-owner"))
	bob := registerAndToken(t, env, uniqueName("coupon-thief"))
	addrID := address(t, env, bob)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	couponID := thresholdCoupon(t, env, alice, 5000, 5000)

	w, _ := createOrder(t, env, bob, uniqueName("req"), addrID, skuID, 1, couponID)
	require.Equal(t, http.StatusNotFound, w.Code, "他人券应按不存在处理")
}

// 满减券：门槛不足 409；满足后金额正确、券核销。
func TestOrderCouponThresholdAndAmount(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("couponer"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	couponID := thresholdCoupon(t, env, token, 5000, 20000)

	// 总额 9900 < 20000：门槛不足 409。
	w, _ := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, couponID)
	require.Equal(t, http.StatusConflict, w.Code)

	// 总额 29700 >= 20000：应付 = 29700 - 5000。
	w, body := createOrder(t, env, token, uniqueName("req2"), addrID, skuID, 3, couponID)
	require.Equal(t, http.StatusCreated, w.Code, "下单失败: %s", w.Body.String())
	require.Equal(t, float64(29700), body["total_amount"])
	require.Equal(t, float64(5000), body["discount_amount"])
	require.Equal(t, float64(24700), body["pay_amount"])
	require.Equal(t, float64(couponID), body["coupon_id"])

	// 券已核销（used）。
	w, mine := doJSON(t, env, http.MethodGet, "/api/coupons/mine?status=used", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	found := false
	for _, c := range mine["items"].([]any) {
		if int64(c.(map[string]any)["id"].(float64)) == couponID {
			found = true
		}
	}
	require.True(t, found, "下单后券应为 used")
}

func TestOrderCouponSettlementSerializesWithTemplateUpdate(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("cpnlock"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 10000, 2)
	couponID := thresholdCoupon(t, env, token, 1000, 5000)

	var templateID int64
	require.NoError(t, env.gdb.Table("user_coupons").
		Where("id = ?", couponID).Select("template_id").Scan(&templateID).Error)

	adminTx := env.gdb.Begin()
	require.NoError(t, adminTx.Error)
	t.Cleanup(func() { _ = adminTx.Rollback().Error })
	var lockedID int64
	require.NoError(t, adminTx.Raw("SELECT id FROM coupon_templates WHERE id = ? FOR UPDATE", templateID).Scan(&lockedID).Error)
	require.Equal(t, templateID, lockedID)
	require.NoError(t, adminTx.Model(&couponmodel.CouponTemplate{}).
		Where("id = ?", templateID).Update("value", 2500).Error)

	type response struct {
		w    *httptest.ResponseRecorder
		body map[string]any
	}
	result := make(chan response, 1)
	go func() {
		w, body := createOrder(t, env, token, uniqueName("coupon-template-update"), addrID, skuID, 1, couponID)
		result <- response{w: w, body: body}
	}()

	select {
	case got := <-result:
		t.Fatalf("模板更新事务尚未提交，订单结算不应使用事务外模板快照: status=%d body=%s", got.w.Code, got.w.Body.String())
	case <-time.After(200 * time.Millisecond):
	}

	require.NoError(t, adminTx.Commit().Error)
	got := <-result
	require.Equal(t, http.StatusCreated, got.w.Code, got.w.Body.String())
	require.Equal(t, float64(2500), got.body["discount_amount"], "应使用先提交的管理员模板值")
	require.Equal(t, float64(7500), got.body["pay_amount"])
}

// 取消待支付订单：回补库存 + 回退券；确认收货前状态机拦截；非 owner 拒绝。
func TestOrderCancelRestoresStockAndCoupon(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("canceller"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	couponID := thresholdCoupon(t, env, token, 5000, 5000)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2, couponID)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)

	// 待支付订单不可确认收货（非法跃迁 409）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/confirm", "", token)
	require.Equal(t, http.StatusConflict, w.Code)

	// 取消：库存回补 8→10、券回退 unused。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 10, skuStock(t, env, skuID), "取消后库存应回补")
	require.Equal(t, model.OrderStatusCancelled, orderByNo(t, env, orderNo).Status)

	w, mine := doJSON(t, env, http.MethodGet, "/api/coupons/mine?status=unused", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	found := false
	for _, c := range mine["items"].([]any) {
		if int64(c.(map[string]any)["id"].(float64)) == couponID {
			found = true
		}
	}
	require.True(t, found, "取消后券应回退为 unused")

	// 重复取消：非法跃迁 409；库存不得重复回补。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", token)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, 10, skuStock(t, env, skuID))
}

// 状态机全链路：支付（直接置库模拟）→ 发货 → 确认收货；各非法跃迁被拒。
func TestOrderShipAndConfirmLifecycle(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("lifecycle"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	admin := adminToken(t, env)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)

	// 待支付不可发货（非法跃迁 409）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/orders/"+orderNo+"/ship", "", admin)
	require.Equal(t, http.StatusConflict, w.Code)

	// 非 admin 发货 403。
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/orders/"+orderNo+"/ship", "", token)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 模拟支付回调：待支付 → 已支付（T08 支付模块驱动此跃迁）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"amount":9900,"result":"success"}`, orderNo, uniqueName("pay")), token)
	require.Equal(t, http.StatusCreated, w.Code, "支付失败: %s", w.Body.String())
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)
	require.NotNil(t, orderByNo(t, env, orderNo).PaidAt)

	// 已支付 → 已发货（admin）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/orders/"+orderNo+"/ship", "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, model.OrderStatusShipped, orderByNo(t, env, orderNo).Status)
	require.NotNil(t, orderByNo(t, env, orderNo).ShippedAt)

	// 已发货 → 已完成（用户确认收货）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/confirm", "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	o := orderByNo(t, env, orderNo)
	require.Equal(t, model.OrderStatusCompleted, o.Status)
	require.NotNil(t, o.CompletedAt)

	// 已完成后再取消：非法跃迁 409。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", token)
	require.Equal(t, http.StatusConflict, w.Code)
}

// 后台订单列表（T25）：跨用户可见 + status 筛选 + 权限（游客 401 / 普通用户 403）。
func TestAdminOrderListAll(t *testing.T) {
	env := requireEnv(t)
	alice := registerAndToken(t, env, uniqueName("admin_alice"))
	bob := registerAndToken(t, env, uniqueName("admin_bob"))
	addrA := address(t, env, alice)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	admin := adminToken(t, env)

	// alice 两单（第二单取消）；bob 一单。
	_, bodyA := createOrder(t, env, alice, uniqueName("req"), addrA, skuID, 1, 0)
	orderA := bodyA["order_no"].(string)
	_, bodyA2 := createOrder(t, env, alice, uniqueName("req"), addrA, skuID, 1, 0)
	orderA2 := bodyA2["order_no"].(string)
	w, _ := doJSON(t, env, http.MethodPost, "/api/orders/"+orderA2+"/cancel", "", alice)
	require.Equal(t, http.StatusNoContent, w.Code)
	addrB := address(t, env, bob)
	_, bodyB := createOrder(t, env, bob, uniqueName("req"), addrB, skuID, 1, 0)
	orderB := bodyB["order_no"].(string)

	// 权限：游客 401，普通用户（bob）403。
	w, _ = doJSON(t, env, http.MethodGet, "/api/admin/orders", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/admin/orders", "", bob)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 全量列表（跨用户，最多翻 5 页找齐 3 单）。
	found := map[string]bool{}
	for page := 1; page <= 5 && len(found) < 3; page++ {
		w, list := doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/admin/orders?page=%d&page_size=50", page), "", admin)
		require.Equal(t, http.StatusOK, w.Code)
		for _, item := range list["orders"].([]any) {
			no := item.(map[string]any)["order_no"].(string)
			if no == orderA || no == orderA2 || no == orderB {
				found[no] = true
			}
			require.Contains(t, item.(map[string]any), "items", "后台列表项应随附订单项")
		}
	}
	require.Equal(t, 3, len(found), "后台列表应跨用户看到 alice/bob 全部订单")

	// 状态筛选 cancelled：alice 的取消单可见、bob 的待支付单不可见。
	w, list := doJSON(t, env, http.MethodGet, "/api/admin/orders?status=cancelled&page_size=50", "", admin)
	require.Equal(t, http.StatusOK, w.Code)
	seenCancel, seenPending := false, false
	for _, item := range list["orders"].([]any) {
		switch item.(map[string]any)["order_no"].(string) {
		case orderA2:
			seenCancel = true
		case orderB:
			seenPending = true
		}
	}
	require.True(t, seenCancel, "已取消订单应出现在 cancelled 筛选中")
	require.False(t, seenPending, "待支付订单不应出现在 cancelled 筛选中")

	// 非法状态 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/admin/orders?status=bogus", "", admin)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// 对象级授权（防 IDOR）：他人订单详情 403、取消 403、列表互不可见。
func TestOrderCrossUserForbidden(t *testing.T) {
	env := requireEnv(t)
	alice := registerAndToken(t, env, uniqueName("alice"))
	bob := registerAndToken(t, env, uniqueName("bob"))
	addrA := address(t, env, alice)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	w, body := createOrder(t, env, alice, uniqueName("req"), addrA, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)

	// bob 看/取消 alice 的订单 → 403。
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/"+orderNo, "", bob)
	require.Equal(t, http.StatusForbidden, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", bob)
	require.Equal(t, http.StatusForbidden, w.Code)

	// bob 的列表看不到 alice 的订单。
	w, list := doJSON(t, env, http.MethodGet, "/api/orders", "", bob)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(0), list["total"])
	require.Empty(t, list["orders"].([]any))

	// 不存在的订单 404。
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/999999999999", "", alice)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// 状态筛选 + 分页。
func TestOrderListStatusFilterAndPagination(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("paginator"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	orders := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 0)
		require.Equal(t, http.StatusCreated, w.Code)
		orders = append(orders, body["order_no"].(string))
	}
	// 取消第 1 单。
	w, _ := doJSON(t, env, http.MethodPost, "/api/orders/"+orders[0]+"/cancel", "", token)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 全部 3 单。
	w, list := doJSON(t, env, http.MethodGet, "/api/orders", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(3), list["total"])

	// 状态筛选 cancelled 1 单。
	w, list = doJSON(t, env, http.MethodGet, "/api/orders?status=cancelled", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), list["total"])
	require.Equal(t, orders[0], list["orders"].([]any)[0].(map[string]any)["order_no"])

	// 分页 page_size=2：第 2 页 1 单。
	w, list = doJSON(t, env, http.MethodGet, "/api/orders?page=2&page_size=2", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(3), list["total"])
	require.Len(t, list["orders"].([]any), 1)

	// 非法状态 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders?status=whatever", "", token)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// 并发幂等：同一 client_request_id 并发提交 10 次，只生成一单、返回同一订单号。
func TestOrderConcurrentIdempotency(t *testing.T) {
	env := requireEnv(t)
	username := uniqueName("race")
	token := registerAndToken(t, env, username)
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	rid := uniqueName("req")

	const n = 10
	orderNos := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, body := createOrder(t, env, token, rid, addrID, skuID, 1, 0)
			require.True(t, w.Code == http.StatusCreated || w.Code == http.StatusOK || w.Code == http.StatusAccepted,
				"并发提交应返回 201/200/202，实际 %d: %s", w.Code, w.Body.String())
			if w.Code == http.StatusAccepted {
				require.Equal(t, "processing", body["state"])
				require.Len(t, body, 2, "202 响应只暴露判别状态和订单号")
			}
			orderNos <- body["order_no"].(string)
		}()
	}
	wg.Wait()
	close(orderNos)

	seen := make(map[string]bool)
	for no := range orderNos {
		seen[no] = true
	}
	require.Len(t, seen, 1, "并发重复提交必须只生成一个订单号")
	require.Equal(t, int64(1), countOrdersByUser(t, env, username))
	require.Equal(t, 9, skuStock(t, env, skuID), "库存只扣一次")
}

func TestOrderInFlightReplayReturnsProcessingContract(t *testing.T) {
	env := requireEnv(t)
	username := uniqueName("processing")
	token := registerAndToken(t, env, username)
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	rid := uniqueName("processing-request")
	var userID int64
	require.NoError(t, env.gdb.Table("users").Select("id").Where("username = ?", username).Scan(&userID).Error)
	require.Positive(t, userID)

	lockTx := env.gdb.Begin()
	require.NoError(t, lockTx.Error)
	t.Cleanup(func() { _ = lockTx.Rollback().Error })
	require.NoError(t, lockTx.Exec("SELECT id FROM skus WHERE id = ? FOR UPDATE", skuID).Error)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- performJSON(env, http.MethodPost, "/api/orders", orderRequestBody(rid, addrID, skuID, 1, 0), token)
	}()
	idempotencyKey := fmt.Sprintf("order:idem:%d:%s", userID, rid)
	require.Eventually(t, func() bool {
		return env.redis.Exists(context.Background(), idempotencyKey).Val() == 1
	}, 2*time.Second, 10*time.Millisecond)

	replay := performJSON(env, http.MethodPost, "/api/orders", orderRequestBody(rid, addrID, skuID, 1, 0), token)
	require.Equal(t, http.StatusAccepted, replay.Code, replay.Body.String())
	require.JSONEq(t, fmt.Sprintf(`{"state":"processing","order_no":%q}`, jsonField(t, replay, "order_no")), replay.Body.String())

	require.NoError(t, lockTx.Rollback().Error)
	first := <-firstDone
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
}

func jsonField(t *testing.T, response *httptest.ResponseRecorder, field string) string {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	value, ok := body[field].(string)
	require.True(t, ok)
	return value
}

// 并发抢购：库存 5，并发 6 单各买 1 → 恰好 5 成功、1 失败，库存归零不超卖。
func TestOrderConcurrentStockRace(t *testing.T) {
	env := requireEnv(t)
	username := uniqueName("seckiller")
	token := registerAndToken(t, env, username)
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 5)

	const n = 6
	results := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, _ := createOrder(t, env, token, fmt.Sprintf("race-%d-%d", i, time.Now().UnixNano()), addrID, skuID, 1, 0)
			results <- w.Code
		}(i)
	}
	wg.Wait()
	close(results)

	ok, conflict := 0, 0
	for code := range results {
		switch code {
		case http.StatusCreated:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("意外状态码 %d", code)
		}
	}
	require.Equal(t, 5, ok, "库存 5 应恰好 5 单成功")
	require.Equal(t, 1, conflict, "恰好 1 单库存不足")
	require.Equal(t, 0, skuStock(t, env, skuID), "不得超卖")
}

// 快照不可变：下单后删地址、改 SKU 价格，订单详情不受影响。
func TestOrderSnapshotImmutability(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("snapshot"))
	addrID := address(t, env, token)
	productID, skuID := onSaleSKU(t, env, 9900, 10)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)

	// 删除地址 + 修改 SKU 价格。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/addresses/%d", addrID), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	admin := adminToken(t, env)
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/skus/%d", skuID),
		`{"specs":{"color":"红"},"price":100,"stock":10}`, admin)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 订单详情仍为下单时快照。
	w, detail := doJSON(t, env, http.MethodGet, "/api/orders/"+orderNo, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "张三", detail["receiver"], "地址快照不受地址簿删除影响")
	it := detail["items"].([]any)[0].(map[string]any)
	require.Equal(t, float64(9900), it["price"], "价格快照不受 SKU 改价影响")
	require.Equal(t, float64(2), it["quantity"])

	// 商品页展示新价格，订单页展示旧价格（快照语义）。
	w, prod := doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products/%d", productID), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	skus := prod["skus"].([]any)
	require.Equal(t, float64(100), skus[0].(map[string]any)["price"])
}

// 购物车结算 + 券 → 取消：多条目库存回补、券回退、购物车不恢复。
func TestOrderCancelFromCartWithCoupon(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("cartcoupon"))
	addrID := address(t, env, token)
	_, skuA := onSaleSKU(t, env, 1000, 10)
	_, skuB := onSaleSKU(t, env, 2000, 5)
	couponID := thresholdCoupon(t, env, token, 3000, 3000)

	w, _ := doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":2}`, skuA), token)
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, skuB), token)
	require.Equal(t, http.StatusCreated, w.Code)

	w, body := doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"from_cart":true,"coupon_id":%d}`, uniqueName("req"), addrID, couponID), token)
	require.Equal(t, http.StatusCreated, w.Code, "结算失败: %s", w.Body.String())
	require.Equal(t, float64(4000), body["total_amount"])
	require.Equal(t, float64(3000), body["discount_amount"])
	require.Equal(t, float64(1000), body["pay_amount"])
	orderNo := body["order_no"].(string)
	require.Equal(t, 8, skuStock(t, env, skuA))
	require.Equal(t, 4, skuStock(t, env, skuB))

	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 10, skuStock(t, env, skuA), "多条目订单取消应全部回补")
	require.Equal(t, 5, skuStock(t, env, skuB))

	w, mine := doJSON(t, env, http.MethodGet, "/api/coupons/mine?status=unused", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	found := false
	for _, c := range mine["items"].([]any) {
		if int64(c.(map[string]any)["id"].(float64)) == couponID {
			found = true
		}
	}
	require.True(t, found, "取消后券应回退")
}

// 取消时券已被外部改动（非 used）：取消失败 409，订单保持待支付、库存不回补。
func TestOrderCancelRollbackFailure(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("rollbackfail"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	couponID := thresholdCoupon(t, env, token, 5000, 5000)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2, couponID)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)
	require.Equal(t, 8, skuStock(t, env, skuID))

	// 外部把券改回 unused（模拟数据扰动）：回退条件更新失败 → 取消整体失败。
	require.NoError(t, env.gdb.Exec("UPDATE user_coupons SET status = 'unused', used_at = NULL WHERE id = ?", couponID).Error)

	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", token)
	require.Equal(t, http.StatusConflict, w.Code)

	o := orderByNo(t, env, orderNo)
	require.Equal(t, model.OrderStatusPendingPayment, o.Status, "回退失败应整体回滚，订单保持待支付")
	require.Equal(t, 8, skuStock(t, env, skuID), "回退失败不得回补库存")
}

// 非法请求：from_cart 与 items 互斥、空 items、缺 client_request_id、非法订单号参数。
func TestOrderInvalidRequests(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("invalid"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 100, 5)

	// from_cart 与 items 互斥 → 400。
	w, _ := doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"from_cart":true,"items":[{"sku_id":%d,"quantity":1}]}`, uniqueName("req"), addrID, skuID), token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 空 items → 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"items":[]}`, uniqueName("req"), addrID), token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 缺 client_request_id → 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"address_id":%d,"items":[{"sku_id":%d,"quantity":1}]}`, addrID, skuID), token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 非法订单号参数 → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/abc", "", token)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/0", "", token)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/-1", "", token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 不存在的订单：详情/取消/确认/发货 → 404。
	admin := adminToken(t, env)
	w, _ = doJSON(t, env, http.MethodGet, "/api/orders/1234567890123456789", "", token)
	require.Equal(t, http.StatusNotFound, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/1234567890123456789/cancel", "", token)
	require.Equal(t, http.StatusNotFound, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/orders/1234567890123456789/confirm", "", token)
	require.Equal(t, http.StatusNotFound, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/orders/1234567890123456789/ship", "", admin)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// 直购多 SKU：金额累计、订单项逐条、库存逐项扣减。
func TestOrderDirectBuyMultiSKU(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("multisku"))
	addrID := address(t, env, token)
	_, skuA := onSaleSKU(t, env, 1000, 10)
	_, skuB := onSaleSKU(t, env, 2000, 5)

	w, body := doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"items":[{"sku_id":%d,"quantity":2},{"sku_id":%d,"quantity":1}]}`,
			uniqueName("req"), addrID, skuA, skuB), token)
	require.Equal(t, http.StatusCreated, w.Code, "下单失败: %s", w.Body.String())
	require.Equal(t, float64(4000), body["total_amount"])
	require.Len(t, body["items"].([]any), 2)
	require.Equal(t, 8, skuStock(t, env, skuA))
	require.Equal(t, 4, skuStock(t, env, skuB))
}

// T09 超时取消：制造已过 expire_at 的待支付订单，经 cron 任务回调（服务层
// CancelExpired 等价调用）自动取消——状态置 cancelled、库存回补、券回退；
// 未超时订单不受影响；已支付订单不参与扫描。
func TestOrderTimeoutCancel(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("timeout"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	// 直减券：满 0 减 1000（threshold 要求 min_amount >= value）。
	tmplID := createTemplate(t, env, "direct", 1000, 0)
	w, body := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", token)
	require.Equal(t, http.StatusCreated, w.Code, "领券失败: %s", w.Body.String())
	couponID := int64(body["id"].(float64))

	// 订单 A：带券 2 件（将超时）；订单 B：无券 1 件（保持未超时）。
	w, body = createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2, couponID)
	require.Equal(t, http.StatusCreated, w.Code)
	orderA := body["order_no"].(string)
	w, body = createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderB := body["order_no"].(string)
	require.Equal(t, 7, skuStock(t, env, skuID))

	var couponStatus string
	require.NoError(t, env.gdb.Raw("SELECT status FROM user_coupons WHERE id = ?", couponID).Scan(&couponStatus).Error)
	require.Equal(t, "used", couponStatus, "下单后券应已核销")

	// 订单 A 拨回已超时（expire_at 为 1 分钟前）。
	require.NoError(t, env.gdb.Exec("UPDATE orders SET expire_at = ? WHERE order_no = ?", time.Now().Add(-time.Minute), orderA).Error)

	// 共享测试库可能还有历史残留的已超时订单（它们被一并取消属正确行为），
	// 取消数只做下限断言；重点断言本测试订单的状态与补偿结果。
	n, _, err := env.orderSvc.CancelExpired(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "超时订单应被取消")

	// 订单 A：已取消 + 回补库存 + 回退券 + cancelled_at 落库。
	o := orderByNo(t, env, orderA)
	require.Equal(t, model.OrderStatusCancelled, o.Status)
	require.NotNil(t, o.CancelledAt)
	require.Equal(t, 9, skuStock(t, env, skuID), "库存回补 2 件（订单 B 仍占用 1 件）")
	require.NoError(t, env.gdb.Raw("SELECT status FROM user_coupons WHERE id = ?", couponID).Scan(&couponStatus).Error)
	require.Equal(t, "unused", couponStatus, "超时取消应回退券")

	// 订单 B：未超时不受影响。
	o = orderByNo(t, env, orderB)
	require.Equal(t, model.OrderStatusPendingPayment, o.Status)
	require.Nil(t, o.CancelledAt)

	// 再次执行（无超时订单）：取消数为 0，幂等不重复处理。
	n, _, err = env.orderSvc.CancelExpired(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)

	// 用户视角：列表 cancelled 含订单 A；详情状态一致。
	w, list := doJSON(t, env, http.MethodGet, "/api/orders?status=cancelled", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), list["total"])
	require.Equal(t, orderA, list["orders"].([]any)[0].(map[string]any)["order_no"])
	w, detail := doJSON(t, env, http.MethodGet, "/api/orders/"+orderA, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "cancelled", detail["status"])
}

// T09 已支付订单不受超时扫描影响（支付后状态已迁移，不再命中待支付扫描）。
func TestOrderTimeoutCancelSkipsPaidOrder(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("paid-timeout"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)

	// 模拟支付成功 → 待支付 → 已支付。
	w, _ = doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"amount":9900,"result":"success"}`, orderNo, uniqueName("pay")), token)
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)

	// 即使 expire_at 已过，已支付订单不参与超时扫描。
	require.NoError(t, env.gdb.Exec("UPDATE orders SET expire_at = ? WHERE order_no = ?", time.Now().Add(-time.Minute), orderNo).Error)
	n, _, err := env.orderSvc.CancelExpired(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)
}

func TestOrderExpiredPaymentIsRejected(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("expiredpay"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)
	require.NoError(t, env.gdb.Exec("UPDATE orders SET expire_at = ? WHERE order_no = ?", time.Now().Add(-time.Second), orderNo).Error)

	w, _ = doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"amount":9900,"result":"success"}`, orderNo, uniqueName("pay")), token)
	require.Equal(t, http.StatusConflict, w.Code, "已过期的待支付订单不能支付成功: %s", w.Body.String())
	require.Equal(t, model.OrderStatusPendingPayment, orderByNo(t, env, orderNo).Status)

	var paymentCount int64
	require.NoError(t, env.gdb.Table("payments").Where("order_no = ?", orderNo).Count(&paymentCount).Error)
	require.Zero(t, paymentCount, "被拒支付的流水必须随事务回滚")
}

func TestOrderExpiredPaymentRacesTimeoutCancellation(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("payrace"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	w, body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1, 0)
	require.Equal(t, http.StatusCreated, w.Code)
	orderNo := body["order_no"].(string)
	require.NoError(t, env.gdb.Exec("UPDATE orders SET expire_at = ? WHERE order_no = ?", time.Now().Add(-time.Second), orderNo).Error)

	start := make(chan struct{})
	paymentResult := make(chan *httptest.ResponseRecorder, 1)
	cancelResult := make(chan error, 1)
	go func() {
		<-start
		paymentResult <- performJSON(env, http.MethodPost, "/api/payments/mock",
			fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"amount":9900,"result":"success"}`, orderNo, uniqueName("pay")), token)
	}()
	go func() {
		<-start
		_, _, err := env.orderSvc.CancelExpired(context.Background())
		cancelResult <- err
	}()
	close(start)

	payResponse := <-paymentResult
	require.Equal(t, http.StatusConflict, payResponse.Code, "过期支付不得在超时取消竞态中成功: %s", payResponse.Body.String())
	require.NoError(t, <-cancelResult)

	if orderByNo(t, env, orderNo).Status == model.OrderStatusPendingPayment {
		_, _, err := env.orderSvc.CancelExpired(context.Background())
		require.NoError(t, err)
	}
	require.Equal(t, model.OrderStatusCancelled, orderByNo(t, env, orderNo).Status)
	require.Equal(t, 10, skuStock(t, env, skuID), "取消仅回补一次库存")

	var paymentCount int64
	require.NoError(t, env.gdb.Table("payments").Where("order_no = ?", orderNo).Count(&paymentCount).Error)
	require.Zero(t, paymentCount, "被拒支付的流水必须随事务回滚")
}
