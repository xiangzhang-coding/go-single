// 集成测试（主 seam）：真实 MySQL + Redis（docker compose）+ httptest 起完整路由，
// 覆盖模拟支付闭环：成功驱动 待支付→已支付 并随后可发货、失败停留待支付可重付、
// 重复回调拒绝（流水唯一 + 状态机）、金额核对（含券订单应付）、跨用户越权。
package payment_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
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
	couponrepo "github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	orderhandler "github.com/xiangzhang-coding/go-single/internal/order/handler"
	"github.com/xiangzhang-coding/go-single/internal/order/model"
	orderrepo "github.com/xiangzhang-coding/go-single/internal/order/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	paymenthandler "github.com/xiangzhang-coding/go-single/internal/payment/handler"
	paymentmodel "github.com/xiangzhang-coding/go-single/internal/payment/model"
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
	redisTestDB = 19
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

type testEnv struct {
	router http.Handler
	gdb    *gorm.DB
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

	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DB: redisTestDB})
	defer rc.Close()
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

	productSvc := productsvc.New(productrepo.Store{
		Category: productrepo.NewGORMCategory(gdb),
		Product:  productrepo.NewGORMProduct(gdb),
		SKU:      productrepo.NewGORMSKU(gdb),
	}, cacheClient, zap.NewNop())
	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier)
	userHandler := userhandler.New(userSvc, verifier, testsupport.AllowAllAuthAttempts{})
	addressHandler := userhandler.NewAddress(userSvc, verifier)
	productHandler := producthandler.New(productSvc, verifier)
	cartSvc := cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(gdb)}, productSvc)
	cartHandler := carthandler.New(cartSvc, verifier)
	couponSvc := couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(gdb), UserCoupon: couponrepo.NewGORMUserCoupon(gdb)}, cacheClient, metrics.New().Business())
	couponHandler := couponhandler.New(couponSvc, verifier)

	orderNoGen, err := snowflake.New(2)
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
	return &testEnv{router: r, gdb: gdb}, nil
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

	var parsed map[string]any
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &parsed))
	}
	return w, parsed
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

func address(t *testing.T, env *testEnv, token string) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/addresses",
		`{"receiver":"张三","phone":"13800138000","province":"广东省","city":"深圳市","district":"南山区","detail":"科技园 1 号","is_default":true}`, token)
	require.Equal(t, http.StatusCreated, w.Code, "创建地址失败: %s", w.Body.String())
	return int64(body["id"].(float64))
}

// createOrder 直购下单，返回订单响应。
func createOrder(t *testing.T, env *testEnv, token, rid string, addrID int64, skuID int64, quantity int) map[string]any {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"items":[{"sku_id":%d,"quantity":%d}]}`, rid, addrID, skuID, quantity), token)
	require.Equal(t, http.StatusCreated, w.Code, "下单失败: %s", w.Body.String())
	return body
}

// mockPay 发起模拟支付回调。
func mockPay(t *testing.T, env *testEnv, token, orderNo, paymentID string, amount int64, result string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"amount":%d,"result":%q}`, orderNo, paymentID, amount, result), token)
}

func orderByNo(t *testing.T, env *testEnv, orderNo string) model.Order {
	t.Helper()
	var o model.Order
	require.NoError(t, env.gdb.First(&o, "order_no = ?", orderNo).Error)
	return o
}

// ---- 测试 ----

// 支付成功：订单进入已支付（paid_at 落库），随后 admin 可发货。
func TestMockPaySuccessThenShip(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("payer"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 2)
	orderNo := body["order_no"].(string)
	require.Equal(t, "pending_payment", body["status"])
	payAmount := int64(body["pay_amount"].(float64))

	paymentID := uniqueName("pay_ok")
	w, resp := mockPay(t, env, token, orderNo, paymentID, payAmount, "success")
	require.Equal(t, http.StatusCreated, w.Code, "支付失败: %s", w.Body.String())
	require.Equal(t, "success", resp["result"])
	require.Equal(t, orderNo, resp["order_no"])
	require.Equal(t, paymentID, resp["payment_id"])

	o := orderByNo(t, env, orderNo)
	require.Equal(t, model.OrderStatusPaid, o.Status, "支付成功订单应进入已支付")
	require.NotNil(t, o.PaidAt, "应记录支付时间")

	// 支付流水落库。
	var n int64
	require.NoError(t, env.gdb.Model(&paymentmodel.Payment{}).Where("order_no = ? AND payment_id = ?", orderNo, paymentID).Count(&n).Error)
	require.Equal(t, int64(1), n)

	// 已支付 → 已发货（admin）。
	admin := adminToken(t, env)
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/orders/"+orderNo+"/ship", "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, model.OrderStatusShipped, orderByNo(t, env, orderNo).Status)
}

// 支付失败：订单停留待支付，仅记失败流水；以新 payment_id 重试成功。
func TestMockPayFailKeepsPendingThenRetrySucceeds(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("retryer"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1)
	orderNo := body["order_no"].(string)
	payAmount := int64(body["pay_amount"].(float64))

	w, resp := mockPay(t, env, token, orderNo, uniqueName("pay_fail1"), payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code, "失败回调应记录流水: %s", w.Body.String())
	require.Equal(t, "fail", resp["result"])
	require.Equal(t, model.OrderStatusPendingPayment, orderByNo(t, env, orderNo).Status, "支付失败订单应停留待支付")

	// 再次失败（新流水号）仍停留待支付，可重复发起。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_fail2"), payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code)

	// 重试支付成功：待支付 → 已支付。
	w, resp = mockPay(t, env, token, orderNo, uniqueName("pay_ok2"), payAmount, "success")
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, "success", resp["result"])
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)
}

func TestPaymentIDUsesExactUnicodeText(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("payment-case"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)
	body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1)
	orderNo := body["order_no"].(string)
	payAmount := int64(body["pay_amount"].(float64))
	paymentID := fmt.Sprintf("Case-Key-%d", time.Now().UnixNano())

	w, _ := mockPay(t, env, token, orderNo, paymentID, payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = mockPay(t, env, token, orderNo, strings.ToLower(paymentID), payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = mockPay(t, env, token, orderNo, paymentID+"-space", payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = mockPay(t, env, token, orderNo, paymentID+"-space ", payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code)
	unicodeSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	unicodePaymentID := strings.Repeat("键", 64-len(unicodeSuffix)) + unicodeSuffix
	w, _ = mockPay(t, env, token, orderNo, unicodePaymentID, payAmount, "fail")
	require.Equal(t, http.StatusCreated, w.Code)

	var count int64
	require.NoError(t, env.gdb.Model(&paymentmodel.Payment{}).Where("order_no = ?", orderNo).Count(&count).Error)
	require.Equal(t, int64(5), count)
}

// 重复回调拒绝：同一 payment_id 流水唯一拒绝；新 payment_id 状态机拒绝。
func TestMockPayDuplicateCallbackRejected(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("dup"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1)
	orderNo := body["order_no"].(string)
	payAmount := int64(body["pay_amount"].(float64))

	w, _ := mockPay(t, env, token, orderNo, uniqueName("pay_dup1"), payAmount, "success")
	require.Equal(t, http.StatusCreated, w.Code)

	// 同一 payment_id 重放：流水唯一约束拒绝（409）。
	w, errorBody := mockPay(t, env, token, orderNo, uniqueName("pay_dup1"), payAmount, "success")
	require.Equal(t, http.StatusConflict, w.Code, "重复流水号应被拒: %s", w.Body.String())
	require.Equal(t, map[string]any{"error": "illegal order status transition"}, errorBody)

	// 新 payment_id 但订单已支付：状态机校验拒绝（409）。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_dup2"), payAmount, "success")
	require.Equal(t, http.StatusConflict, w.Code)

	// 已支付订单的失败回调同样拒绝（状态机一致，不得污染已流转订单）。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_dup3"), payAmount, "fail")
	require.Equal(t, http.StatusConflict, w.Code)

	// 被拒回调不得落流水（仍只有一条）。
	var n int64
	require.NoError(t, env.gdb.Model(&paymentmodel.Payment{}).Where("order_no = ?", orderNo).Count(&n).Error)
	require.Equal(t, int64(1), n)
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)
}

// 金额不符：回调金额 ≠ 应付金额 → 409，订单不流转、不落流水。
func TestMockPayAmountMismatchRejected(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("mismatch"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1)
	orderNo := body["order_no"].(string)

	w, _ := mockPay(t, env, token, orderNo, uniqueName("pay_mis1"), 9901, "success")
	require.Equal(t, http.StatusConflict, w.Code, "金额不符应被拒: %s", w.Body.String())
	require.Equal(t, model.OrderStatusPendingPayment, orderByNo(t, env, orderNo).Status)

	// 少付同样被拒。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_mis2"), 9899, "success")
	require.Equal(t, http.StatusConflict, w.Code)

	var n int64
	require.NoError(t, env.gdb.Model(&paymentmodel.Payment{}).Where("order_no = ?", orderNo).Count(&n).Error)
	require.Equal(t, int64(0), n)

	// 修正金额后支付成功（错误流水号已被拒，不占唯一键）。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_mis3"), 9900, "success")
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)
}

// 券订单：应付 = 总额 − 券额，按应付金额支付成功。
func TestMockPayWithCouponUsesPayAmount(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("coupon_payer"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 1)

	// 满 5000 减 5000 券：应付 9900 - 5000 = 4900。
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/coupons",
		fmt.Sprintf(`{"name":%q,"type":"threshold","value":5000,"min_amount":5000,"total":100,"per_user_limit":1,"valid_from":%q,"valid_until":%q}`,
			uniqueName("满减券"), time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339)), adminToken(t, env))
	require.Equal(t, http.StatusCreated, w.Code, "创建券模板失败: %s", w.Body.String())
	tmplID := int64(body["id"].(float64))
	w, body = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", token)
	require.Equal(t, http.StatusCreated, w.Code, "领券失败: %s", w.Body.String())
	userCouponID := int64(body["id"].(float64))

	w, body = doJSON(t, env, http.MethodPost, "/api/orders",
		fmt.Sprintf(`{"client_request_id":%q,"address_id":%d,"coupon_id":%d,"items":[{"sku_id":%d,"quantity":1}]}`,
			uniqueName("req"), addrID, userCouponID, skuID), token)
	require.Equal(t, http.StatusCreated, w.Code, "下单失败: %s", w.Body.String())
	orderNo := body["order_no"].(string)
	require.Equal(t, float64(4900), body["pay_amount"])

	// 按总额支付：金额不符拒绝。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_cpn1"), 9900, "success")
	require.Equal(t, http.StatusConflict, w.Code, "券订单应按应付金额支付: %s", w.Body.String())
	// 按应付支付：成功。
	w, _ = mockPay(t, env, token, orderNo, uniqueName("pay_cpn2"), 4900, "success")
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, model.OrderStatusPaid, orderByNo(t, env, orderNo).Status)
}

// 对象级授权：他人订单支付 403；未登录 401。
func TestMockPayAuthAndOwnerEnforced(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("owner"))
	other := registerAndToken(t, env, uniqueName("intruder"))
	addrID := address(t, env, token)
	_, skuID := onSaleSKU(t, env, 9900, 10)

	body := createOrder(t, env, token, uniqueName("req"), addrID, skuID, 1)
	orderNo := body["order_no"].(string)
	payAmount := int64(body["pay_amount"].(float64))

	// 未登录：401。
	w, _ := mockPay(t, env, "", orderNo, uniqueName("pay_auth1"), payAmount, "success")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 他人支付：403（防 IDOR）。
	w, _ = mockPay(t, env, other, orderNo, uniqueName("pay_auth2"), payAmount, "success")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, model.OrderStatusPendingPayment, orderByNo(t, env, orderNo).Status)

	// 参数非法：400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":"","amount":1,"result":"success"}`, orderNo), token)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"amount":1,"result":"pending"}`, orderNo, uniqueName("pay_auth3")), token)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/payments/mock",
		fmt.Sprintf(`{"order_id":%q,"payment_id":%q,"result":"success"}`, orderNo, uniqueName("pay_auth4")), token)
	require.Equal(t, http.StatusBadRequest, w.Code, "缺少 amount 应 400")
}
