// 集成测试（主 seam）：真实 MySQL + Redis（docker compose）+ httptest 起完整路由，
// 覆盖加购闭环：SKU 存在/上架校验、重复加购合并、改量/删除、跨用户越权拒绝。
package cart_test

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
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	carthandler "github.com/xiangzhang-coding/go-single/internal/cart/handler"
	cartrepo "github.com/xiangzhang-coding/go-single/internal/cart/repository"
	cartsvc "github.com/xiangzhang-coding/go-single/internal/cart/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
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
	redisTestDB = 15
)

// testEnv 每个测试包只构建一次；MySQL 或 Redis 不可达时本地跳过、CI 失败。
type testEnv struct {
	router   http.Handler
	verifier auth.TokenVerifier
	redis    *redis.Client
	gdb      *gorm.DB
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
	if err := rc.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("Redis 连接失败: %w", err)
	}
	if err := rc.FlushDB(ctx).Err(); err != nil {
		return nil, err
	}
	cacheClient, err := cache.NewRedis(redisAddr, "", redisTestDB)
	if err != nil {
		return nil, err
	}

	verifier := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})

	// product 服务：cart 跨模块校验 SKU 存在/上架所依赖。
	productStore := productrepo.Store{
		Category: productrepo.NewGORMCategory(gdb),
		Product:  productrepo.NewGORMProduct(gdb),
		SKU:      productrepo.NewGORMSKU(gdb),
	}
	productSvc := productsvc.New(productStore, cacheClient)

	userHandler := userhandler.New(usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier), verifier, testsupport.AllowAllAuthAttempts{})
	cartHandler := carthandler.New(cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(gdb)}, productSvc), verifier)
	productHandler := producthandler.New(productSvc, verifier)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
	cartHandler.RegisterRoutes(api)
	return &testEnv{router: r, verifier: verifier, redis: rc, gdb: gdb}, nil
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

// onSaleSKU 组装一条可售 SKU 并返回 (productID, skuID)。
func onSaleSKU(t *testing.T, env *testEnv) (int64, int64) {
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
		`{"specs":{"color":"红"},"price":9900,"stock":10}`, token)
	require.Equal(t, http.StatusCreated, w.Code, "创建 SKU 失败: %s", w.Body.String())
	skuID := int64(body["id"].(float64))

	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/publish", productID), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	return productID, skuID
}

// offSaleSKU 组装一条下架商品的 SKU（校验加购被拒）。
func offSaleSKU(t *testing.T, env *testEnv) int64 {
	t.Helper()
	token := adminToken(t, env)
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/categories", fmt.Sprintf(`{"name":%q}`, uniqueName("草稿")), token)
	require.Equal(t, http.StatusCreated, w.Code)
	catID := int64(body["id"].(float64))
	w, body = doJSON(t, env, http.MethodPost, "/api/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":%q}`, catID, uniqueName("未上架商品")), token)
	require.Equal(t, http.StatusCreated, w.Code)
	productID := int64(body["id"].(float64))
	w, body = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/skus", productID),
		`{"specs":{},"price":100,"stock":1}`, token)
	require.Equal(t, http.StatusCreated, w.Code)
	return int64(body["id"].(float64))
}

// ---- 测试 ----

// 鉴权：未登录访问购物车一律 401。
func TestCartRequiresAuth(t *testing.T) {
	env := requireEnv(t)

	w, _ := doJSON(t, env, http.MethodGet, "/api/cart", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", `{"sku_id":1,"quantity":1}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodPut, "/api/cart/items/1", `{"quantity":2}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodDelete, "/api/cart/items/1", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// 加购闭环：加购 → 重复加购合并 → 列表快照 → 改量 → 删除。
func TestCartHappyPath(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("buyer"))
	_, skuID := onSaleSKU(t, env)

	// 加购数量 2 → 201。
	w, body := doJSON(t, env, http.MethodPost, "/api/cart",
		fmt.Sprintf(`{"sku_id":%d,"quantity":2}`, skuID), token)
	require.Equal(t, http.StatusCreated, w.Code, "加购失败: %s", w.Body.String())
	itemID := int64(body["id"].(float64))
	require.Equal(t, float64(2), body["quantity"])

	// 重复加购同一 SKU（数量 3）→ 合并为 5，复用原条目。
	w, body = doJSON(t, env, http.MethodPost, "/api/cart",
		fmt.Sprintf(`{"sku_id":%d,"quantity":3}`, skuID), token)
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, float64(itemID), body["id"], "重复加购应复用原条目")
	require.Equal(t, float64(5), body["quantity"])

	// 列表：1 条，快照含标题/规格/价格。
	w, list := doJSON(t, env, http.MethodGet, "/api/cart", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items := list["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	require.Equal(t, float64(skuID), item["sku_id"])
	require.Equal(t, float64(5), item["quantity"])
	require.Equal(t, float64(9900), item["price"])
	require.Equal(t, "红", item["specs"].(map[string]any)["color"])
	require.NotEmpty(t, item["title"])
	require.NotEmpty(t, item["product_id"])

	// 改量 → 204，列表生效。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/cart/items/%d", itemID), `{"quantity":7}`, token)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, list = doJSON(t, env, http.MethodGet, "/api/cart", "", token)
	require.Equal(t, float64(7), list["items"].([]any)[0].(map[string]any)["quantity"])

	// 删除 → 204，列表为空。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/cart/items/%d", itemID), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, list = doJSON(t, env, http.MethodGet, "/api/cart", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, list["items"].([]any))
}

// 校验：SKU 不存在 404、商品下架 409、非法数量/请求 400。
func TestCartAddRejects(t *testing.T) {
	env := requireEnv(t)
	token := registerAndToken(t, env, uniqueName("picker"))
	_, skuID := onSaleSKU(t, env)
	offSKU := offSaleSKU(t, env)

	// 不存在的 SKU → 404。
	w, _ := doJSON(t, env, http.MethodPost, "/api/cart", `{"sku_id":999999,"quantity":1}`, token)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 下架商品的 SKU → 409。
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, offSKU), token)
	require.Equal(t, http.StatusConflict, w.Code)

	// 数量 0 / 负数 / 超上限 → 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":0}`, skuID), token)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":-1}`, skuID), token)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":100}`, skuID), token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 缺 sku_id → 400（binding required）。
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", `{"quantity":1}`, token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 不存在的条目改量/删除 → 404。
	w, _ = doJSON(t, env, http.MethodPut, "/api/cart/items/999999", `{"quantity":1}`, token)
	require.Equal(t, http.StatusNotFound, w.Code)
	w, _ = doJSON(t, env, http.MethodDelete, "/api/cart/items/999999", "", token)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// 对象级授权（防 IDOR）：他人条目的改量/删除被拒 403，购物车互不可见。
func TestCartCrossUserForbidden(t *testing.T) {
	env := requireEnv(t)
	alice := registerAndToken(t, env, uniqueName("alice"))
	bob := registerAndToken(t, env, uniqueName("bob"))
	productID, skuID := onSaleSKU(t, env)

	w, body := doJSON(t, env, http.MethodPost, "/api/cart",
		fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, skuID), alice)
	require.Equal(t, http.StatusCreated, w.Code)
	itemID := int64(body["id"].(float64))

	// bob 改/删 alice 的条目 → 403。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/cart/items/%d", itemID), `{"quantity":3}`, bob)
	require.Equal(t, http.StatusForbidden, w.Code)
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/cart/items/%d", itemID), "", bob)
	require.Equal(t, http.StatusForbidden, w.Code)

	// bob 的列表看不到 alice 的条目；alice 的条目仍完好。
	w, list := doJSON(t, env, http.MethodGet, "/api/cart", "", bob)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, list["items"].([]any))
	w, list = doJSON(t, env, http.MethodGet, "/api/cart", "", alice)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, list["items"].([]any), 1)

	// 下架商品后：已加购条目仍可见（可管理），但不可再加购。
	token := adminToken(t, env)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/unpublish", productID), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, list = doJSON(t, env, http.MethodGet, "/api/cart", "", alice)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, list["items"].([]any), 1, "下架不应清除已加购条目")
	w, _ = doJSON(t, env, http.MethodPost, "/api/cart", fmt.Sprintf(`{"sku_id":%d,"quantity":1}`, skuID), alice)
	require.Equal(t, http.StatusConflict, w.Code, "下架商品不可再加购")
}
