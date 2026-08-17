// 集成测试（主 seam）：真实 MySQL + Redis（docker compose）+ httptest 起完整路由，
// 覆盖 admin 类目/商品/SKU 管理、游客列表筛选分页、详情缓存命中/降级、下架不可见。
package product_test

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
	redisTestDB = 20
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
	store := productrepo.Store{
		Category: productrepo.NewGORMCategory(gdb),
		Product:  productrepo.NewGORMProduct(gdb),
		SKU:      productrepo.NewGORMSKU(gdb),
	}
	productHandler := producthandler.New(productsvc.New(store, cacheClient), verifier)
	userHandler := userhandler.New(usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier), verifier)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
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

func createCategory(t *testing.T, env *testEnv, token, name string) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/categories",
		fmt.Sprintf(`{"name":%q}`, name), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建类目失败: %s", w.Body.String())
	id, ok := body["id"].(float64)
	require.True(t, ok)
	return int64(id)
}

func createProduct(t *testing.T, env *testEnv, token string, categoryID int64, title string) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":%q,"description":"desc"}`, categoryID, title), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建商品失败: %s", w.Body.String())
	return int64(body["id"].(float64))
}

func createSKU(t *testing.T, env *testEnv, token string, productID int64, price int64, stock int) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/skus", productID),
		fmt.Sprintf(`{"specs":{"color":"红"},"price":%d,"stock":%d}`, price, stock), token)
	require.Equal(t, http.StatusCreated, w.Code, "创建 SKU 失败: %s", w.Body.String())
	return int64(body["id"].(float64))
}

func publish(t *testing.T, env *testEnv, token string, productID int64, publish_ bool) {
	t.Helper()
	action := "publish"
	if !publish_ {
		action = "unpublish"
	}
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/%s", productID, action), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func uniqueName(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()) }

// ---- 测试 ----

// 权限：游客/普通用户不能管理；admin 可。
func TestAdminEndpointsRequireAdmin(t *testing.T) {
	env := requireEnv(t)

	// 无 token → 401。
	w, _ := doJSON(t, env, http.MethodPost, "/api/admin/categories", `{"name":"x"}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 普通用户 → 403。
	userToken := registerAndToken(t, env, uniqueName("mallory"))
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/categories", `{"name":"x"}`, userToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/products", `{"category_id":1,"title":"x"}`, userToken)
	require.Equal(t, http.StatusForbidden, w.Code)
}

// 类目 CRUD：创建/编辑/删除 + 重名 400 + 占用 409。
func TestAdminCategoryCRUD(t *testing.T) {
	env := requireEnv(t)
	token := adminToken(t, env)

	name := uniqueName("数码")
	id := createCategory(t, env, token, name)

	// 重名 → 400。
	w, _ := doJSON(t, env, http.MethodPost, "/api/admin/categories", fmt.Sprintf(`{"name":%q}`, name), token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 列表可见。
	w, list := doJSON(t, env, http.MethodGet, "/api/categories", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	found := false
	for _, item := range list["items"].([]any) {
		if int64(item.(map[string]any)["id"].(float64)) == id {
			found = true
		}
	}
	require.True(t, found, "类目列表应包含新建类目")

	// 编辑 → 204，名称更新。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/categories/%d", id), fmt.Sprintf(`{"name":%q}`, uniqueName("改名")), token)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 类目下有商品 → 409。
	createProduct(t, env, token, id, uniqueName("手机"))
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/admin/categories/%d", id), "", token)
	require.Equal(t, http.StatusConflict, w.Code)

	// 空类目可删 → 204。
	emptyCat := createCategory(t, env, token, uniqueName("空类目"))
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/admin/categories/%d", emptyCat), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
}

// 商品/SKU 管理：默认下架、上/下架、SKU 维护、非法输入。
func TestAdminProductSKUCRUD(t *testing.T) {
	env := requireEnv(t)
	token := adminToken(t, env)

	catID := createCategory(t, env, token, uniqueName("服装"))
	pID := createProduct(t, env, token, catID, "T恤")

	// 不存在类目 → 404。
	w, _ := doJSON(t, env, http.MethodPost, "/api/admin/products", `{"category_id":999999,"title":"x"}`, token)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 非法 specs（非 JSON）→ 400。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/skus", pID), `{"specs":{"color":"红","price":100}`, token)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 不存在的商品建 SKU → 404。
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/products/999999/skus", `{"specs":{},"price":1}`, token)
	require.Equal(t, http.StatusNotFound, w.Code)

	skuID := createSKU(t, env, token, pID, 3900, 10)

	// 编辑 SKU（价格/库存）→ 204。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/skus/%d", skuID), `{"specs":{"color":"蓝"},"price":4900,"stock":5}`, token)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 编辑不存在的 SKU → 404。
	w, _ = doJSON(t, env, http.MethodPut, "/api/admin/skus/999999", `{"specs":{},"price":1,"stock":1}`, token)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 编辑商品 → 204；不存在的商品 → 404。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/products/%d", pID),
		fmt.Sprintf(`{"category_id":%d,"title":"T恤 Pro"}`, catID), token)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, _ = doJSON(t, env, http.MethodPut, "/api/admin/products/999999",
		fmt.Sprintf(`{"category_id":%d,"title":"x"}`, catID), token)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 上架 → 204；下架 → 204；不存在的商品 → 404。
	publish(t, env, token, pID, true)
	publish(t, env, token, pID, false)
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/products/999999/publish", "", token)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 删除 SKU → 204；重复删除 → 404。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/admin/skus/%d", skuID), "", token)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/admin/skus/%d", skuID), "", token)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// 后台商品列表：含草稿/下架（游客列表不可见），status 筛选 + 分页 + 权限。
func TestAdminProductListIncludesDrafts(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)

	catID := createCategory(t, env, admin, uniqueName("后台列表"))
	p1 := createProduct(t, env, admin, catID, uniqueName("已上架"))
	p2 := createProduct(t, env, admin, catID, uniqueName("草稿"))
	publish(t, env, admin, p1, true)

	// 全部状态：本类目 2 件可见（含草稿）。
	path := fmt.Sprintf("/api/admin/products?category_id=%d", catID)
	w, list := doJSON(t, env, http.MethodGet, path, "", admin)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(2), list["total"])
	require.Equal(t, 2, len(list["items"].([]any)))

	// 按状态筛选：仅下架 1 件；仅上架 1 件。
	w, list = doJSON(t, env, http.MethodGet, path+"&status=off_sale", "", admin)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), list["total"])
	require.Equal(t, float64(p2), list["items"].([]any)[0].(map[string]any)["id"])

	w, list = doJSON(t, env, http.MethodGet, path+"&status=on_sale", "", admin)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), list["total"])
	require.Equal(t, float64(p1), list["items"].([]any)[0].(map[string]any)["id"])

	// 非法状态 → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/admin/products?status=bogus", "", admin)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 权限：游客 401，普通用户 403。
	w, _ = doJSON(t, env, http.MethodGet, "/api/admin/products", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	userToken := registerAndToken(t, env, uniqueName("mallory"))
	w, _ = doJSON(t, env, http.MethodGet, "/api/admin/products", "", userToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 同一商品在游客列表不可见（草稿通道隔离）。
	w, visitorList := doJSON(t, env, http.MethodGet, "/api/products", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	for _, item := range visitorList["items"].([]any) {
		require.NotEqual(t, float64(p2), item.(map[string]any)["id"])
	}
}

// 游客列表：按类目筛选 + 分页 + 下架不可见。
func TestVisitorListFilterPagination(t *testing.T) {
	env := requireEnv(t)
	token := adminToken(t, env)

	digital := createCategory(t, env, token, uniqueName("数码"))
	home := createCategory(t, env, token, uniqueName("家电"))

	p1 := createProduct(t, env, token, digital, uniqueName("手机"))
	p2 := createProduct(t, env, token, digital, uniqueName("平板"))
	offline := createProduct(t, env, token, digital, uniqueName("下架品"))
	homeProduct := createProduct(t, env, token, home, uniqueName("冰箱"))

	publish(t, env, token, p1, true)
	publish(t, env, token, p2, true)
	publish(t, env, token, homeProduct, true)
	_ = offline // 保持下架

	// 按类目筛选：数码类 2 件（下架的不计）。
	w, list := doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products?category_id=%d", digital), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(2), list["total"])
	require.Equal(t, 2, len(list["items"].([]any)))

	// 分页：page=1, page_size=1 → 1 条，total 仍为 2。
	w, list = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products?category_id=%d&page=1&page_size=1", digital), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(2), list["total"])
	require.Equal(t, 1, len(list["items"].([]any)))

	// 分页越界 → 空列表。
	w, list = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products?category_id=%d&page=99", digital), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(2), list["total"])
	require.Equal(t, 0, len(list["items"].([]any)))

	// 非法 category_id → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/products?category_id=abc", "", "")
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 家电类 1 件。
	w, list = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products?category_id=%d", home), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), list["total"])
}

// 详情：完整 SPU+SKU 信息；第二次命中缓存；清缓存后降级直查 DB；下架 404。
func TestVisitorDetailCacheLifecycle(t *testing.T) {
	env := requireEnv(t)
	token := adminToken(t, env)

	catID := createCategory(t, env, token, uniqueName("数码"))
	pID := createProduct(t, env, token, catID, uniqueName("旗舰手机"))
	skuID := createSKU(t, env, token, pID, 9900, 10)
	publish(t, env, token, pID, true)

	// 首次访问：直查 DB 并回填缓存。
	w, d := doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products/%d", pID), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(pID), d["id"])
	require.Equal(t, float64(catID), d["category_id"])
	skus := d["skus"].([]any)
	require.Len(t, skus, 1)
	sku := skus[0].(map[string]any)
	require.Equal(t, float64(skuID), sku["id"])
	require.Equal(t, float64(9900), sku["price"])
	require.Equal(t, float64(10), sku["stock"])
	require.Equal(t, "红", sku["specs"].(map[string]any)["color"], "specs 应内嵌为对象返回")

	// 缓存已写入，TTL ≈ 5min。
	ctx := context.Background()
	key := fmt.Sprintf("product:detail:%d", pID)
	raw, err := env.redis.Get(ctx, key).Result()
	require.NoError(t, err, "首次访问后详情应已回填缓存")
	require.NotEmpty(t, raw)
	ttl, err := env.redis.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 4*time.Minute)
	require.LessOrEqual(t, ttl, 5*time.Minute)

	// 第二次访问命中缓存：绕过接口直接改 DB（涨价），再访问仍返回缓存旧价。
	require.NoError(t, env.gdb.Exec("UPDATE skus SET price = 1 WHERE id = ?", skuID).Error)
	w, d = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products/%d", pID), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(9900), d["skus"].([]any)[0].(map[string]any)["price"], "命中缓存应返回旧价格")

	// 清空缓存（模拟缓存被清）：降级直查 DB，读到新价格并重新回填。
	require.NoError(t, env.redis.Del(ctx, key).Err())
	w, d = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products/%d", pID), "", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), d["skus"].([]any)[0].(map[string]any)["price"], "缓存清空后应直查 DB 读到新价格")
	require.NoError(t, env.redis.Get(ctx, key).Err(), "降级直查后应重新回填缓存")

	// 下架：游客 404，且缓存被清除（防止旧缓存复活）。
	publish(t, env, token, pID, false)
	w, _ = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products/%d", pID), "", "")
	require.Equal(t, http.StatusNotFound, w.Code)
	_, err = env.redis.Get(ctx, key).Result()
	require.ErrorIs(t, err, redis.Nil, "下架后缓存应被清除")

	// 不存在的商品 → 404。
	w, _ = doJSON(t, env, http.MethodGet, "/api/products/999999", "", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

// 下架商品对游客不可见（列表与详情双通道）。
func TestOffSaleInvisibleEverywhere(t *testing.T) {
	env := requireEnv(t)
	token := adminToken(t, env)

	catID := createCategory(t, env, token, uniqueName("数码"))
	pID := createProduct(t, env, token, catID, uniqueName("草稿"))

	w, _ := doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/products/%d", pID), "", "")
	require.Equal(t, http.StatusNotFound, w.Code)

	w, list := doJSON(t, env, http.MethodGet, "/api/products", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	for _, item := range list["items"].([]any) {
		require.NotEqual(t, float64(pID), item.(map[string]any)["id"])
	}
}
