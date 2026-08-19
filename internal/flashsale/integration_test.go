// 集成测试（主 seam）：真实 MySQL + Redis（docker compose）+ httptest 起完整路由，
// 覆盖 admin 活动管理闭环（创建/编辑/上架/下架）、上架预热一致性、
// 进行中编辑库存只减不增、权限与下架后抢购被拒（预扣经真实 Redis Lua）；
// T11 抢购接口：并发不超卖 / 幂等键拦截 / 全局与按用户限流 / 窗口与下架拒绝。
package flashsale_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

	flashsalehandler "github.com/xiangzhang-coding/go-single/internal/flashsale/handler"
	flashsalerepo "github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
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
	redisTestDB = 17
)

// testEnv 每个测试包只构建一次；MySQL 或 Redis 不可达时本地跳过、CI 失败。
type testEnv struct {
	router         http.Handler
	verifier       auth.TokenVerifier
	gdb            *gorm.DB
	redis          *redis.Client
	cacheClient    cache.Client
	productSvc     productsvc.Service
	userHandler    *userhandler.Handler
	productHandler *producthandler.Handler
	flashsaleSvc   flashsaleTestService
	publisher      *fakePublisher
}

type flashsaleTestService interface {
	flashsalesvc.Service
	PreDeduct(ctx context.Context, userID, activityID int64) error
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
	if err := rc.ConfigSet(redisCtx, "appendonly", "yes").Err(); err != nil {
		return nil, fmt.Errorf("Redis 开启 AOF: %w", err)
	}
	if err := rc.ConfigSet(redisCtx, "appendfsync", "always").Err(); err != nil {
		return nil, fmt.Errorf("Redis 设置测试 AOF fsync: %w", err)
	}
	if err := rc.FlushDB(redisCtx).Err(); err != nil {
		return nil, err
	}
	cacheClient, err := cache.NewRedis(redisAddr, "", redisTestDB)
	if err != nil {
		return nil, err
	}

	verifier := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})
	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier)
	userHandler := userhandler.New(userSvc, verifier, testsupport.AllowAllAuthAttempts{})

	productRepo := productrepo.NewGORMProduct(gdb)
	productSvc := productsvc.New(productrepo.Store{Category: productrepo.NewGORMCategory(gdb), Product: productRepo, SKU: productrepo.NewGORMSKU(gdb)}, cacheClient, zap.NewNop())
	productHandler := producthandler.New(productSvc, verifier)

	// 默认 env 的秒杀服务关闭按用户限流；限流专项测试经 newFlashsaleRouter 另起路由。
	// MQ 用 fake 发布端口（记录消息不投递）：T11 抢购路径测试无需真实 RabbitMQ；
	// 异步落单闭环（T12）走真实 MQ，见 seckill_order_integration_test.go。
	pub := &fakePublisher{}
	activityStore := flashsalerepo.NewGORMActivity(gdb)
	flashsaleSvc := flashsalesvc.New(
		flashsalerepo.Store{
			Activities:    activityStore,
			PreDeductions: flashsalerepo.NewGORMPreDeduction(gdb),
			Tx:            activityStore,
		},
		productSvc,
		cacheClient,
		limiter.RedisCounterConfig{},
		pub,
		&fakeNos{next: time.Now().UnixNano()},
		metrics.New().Business(),
	).(flashsaleTestService)
	flashsaleHandler := flashsalehandler.New(flashsaleSvc, verifier)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
	flashsaleHandler.RegisterRoutes(api, allowAll)
	return &testEnv{
		router:         r,
		verifier:       verifier,
		gdb:            gdb,
		redis:          rc,
		cacheClient:    cacheClient,
		productSvc:     productSvc,
		userHandler:    userHandler,
		productHandler: productHandler,
		flashsaleSvc:   flashsaleSvc,
		publisher:      pub,
	}, nil
}

// allowAll 恒放行中间件：默认 env 抢购接口不测全局限流（专项测试另起路由）。
func allowAll(_ *gin.Context) {}

// fakePublisher 记录发布消息的 MQ 替身：T11 抢购路径无需真实 RabbitMQ。
type fakePublisher struct {
	mu    sync.Mutex
	queue string
	body  []byte
	err   error
}

type failingDecreaseCache struct {
	cache.Client
}

func (failingDecreaseCache) DecreaseFlashSaleStockDurably(context.Context, cache.FlashSaleDecreaseParams, time.Duration) error {
	return errors.New("injected Redis stock sync failure")
}

type blockingPauseCache struct {
	cache.Client
	paused  chan struct{}
	proceed chan struct{}
}

func (c blockingPauseCache) PauseFlashSaleStockDurably(ctx context.Context, p cache.FlashSalePauseParams, timeout time.Duration) (int, error) {
	stock, err := c.Client.PauseFlashSaleStockDurably(ctx, p, timeout)
	if err != nil {
		return 0, err
	}
	close(c.paused)
	select {
	case <-c.proceed:
		return stock, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *fakePublisher) Publish(_ context.Context, queue string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = queue
	f.body = body
	return f.err
}

// fakeNos 雪花订单号替身（确定性序列，避免依赖真实实现）。
type fakeNos struct {
	mu   sync.Mutex
	next int64
}

func (f *fakeNos) Next() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return f.next, nil
}

// newFlashsaleRouter 构建独立秒杀路由（按需注入限流配置），复用 user/product
// handler（共享同一 gdb/verifier），供限流与抢购接口专项测试使用。
func (e *testEnv) newFlashsaleRouter(t *testing.T, limitCfg limiter.TokenBucketConfig, rlCfg limiter.RedisCounterConfig) http.Handler {
	t.Helper()
	activityStore := flashsalerepo.NewGORMActivity(e.gdb)
	svc := flashsalesvc.New(
		flashsalerepo.Store{
			Activities:    activityStore,
			PreDeductions: flashsalerepo.NewGORMPreDeduction(e.gdb),
			Tx:            activityStore,
		},
		e.productSvc,
		e.cacheClient,
		rlCfg,
		&fakePublisher{},
		&fakeNos{next: time.Now().UnixNano()},
		metrics.New().Business(),
	)
	h := flashsalehandler.New(svc, e.verifier)
	limitMW, err := limiter.NewTokenBucket(limitCfg)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	e.userHandler.RegisterRoutes(api)
	e.productHandler.RegisterRoutes(api)
	h.RegisterRoutes(api, limitMW)
	return r
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
	return doJSONOn(t, env.router, method, path, body, token)
}

func doJSONOn(t *testing.T, router http.Handler, method, path, body, token string) (*httptest.ResponseRecorder, map[string]any) {
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
	router.ServeHTTP(w, r)

	var parsed map[string]any
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &parsed))
	}
	return w, parsed
}

// purchase 在指定路由上发起抢购请求。
func purchase(t *testing.T, router http.Handler, activityID int64, token string, requestIDs ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	requestID := uniqueName("purchase")
	if len(requestIDs) > 0 {
		requestID = requestIDs[0]
	}
	return doJSONOn(t, router, http.MethodPost, fmt.Sprintf("/api/flashsales/%d/purchase", activityID),
		fmt.Sprintf(`{"client_request_id":%q}`, requestID), token)
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
	require.Equal(t, http.StatusCreated, w.Code, "注册失败: %s", w.Body.String())
	_, login := doJSON(t, env, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	return login["token"].(string)
}

func uniqueName(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()) }

// seedSKU admin 建类目→商品→SKU，返回 SKU id（秒杀活动绑定目标）。
func seedSKU(t *testing.T, env *testEnv, admin string) int64 {
	t.Helper()
	w, cat := doJSON(t, env, http.MethodPost, "/api/admin/categories", fmt.Sprintf(`{"name":%q}`, uniqueName("类目")), admin)
	require.Equal(t, http.StatusCreated, w.Code, "建类目失败: %s", w.Body.String())

	w, prod := doJSON(t, env, http.MethodPost, "/api/admin/products",
		fmt.Sprintf(`{"category_id":%d,"title":%q,"description":"demo"}`, int64(cat["id"].(float64)), uniqueName("商品")), admin)
	require.Equal(t, http.StatusCreated, w.Code, "建商品失败: %s", w.Body.String())

	w, sku := doJSON(t, env, http.MethodPost,
		fmt.Sprintf("/api/admin/products/%d/skus", int64(prod["id"].(float64))),
		`{"specs":{"color":"红"},"price":19900,"stock":500}`, admin)
	require.Equal(t, http.StatusCreated, w.Code, "建 SKU 失败: %s", w.Body.String())
	return int64(sku["id"].(float64))
}

// activityBody 构造活动请求体；偏移单位均为分钟。
func activityBody(skuID int64, title string, price int64, stock, perUserLimit int, fromOffset, untilOffset time.Duration) string {
	return activityBodyAt(skuID, title, price, stock, perUserLimit, time.Now().Add(fromOffset), time.Now().Add(untilOffset))
}

func activityBodyAt(skuID int64, title string, price int64, stock, perUserLimit int, startAt, endAt time.Time) string {
	from := startAt.Format(time.RFC3339Nano)
	until := endAt.Format(time.RFC3339Nano)
	return fmt.Sprintf(`{"sku_id":%d,"title":%q,"price":%d,"stock":%d,"per_user_limit":%d,"start_at":%q,"end_at":%q}`,
		skuID, title, price, stock, perUserLimit, from, until)
}

// createActivity admin 创建活动（默认进行中窗口），返回活动 id。
func createActivity(t *testing.T, env *testEnv, admin string, skuID int64, stock int, perUserLimit int, fromOffset, untilOffset time.Duration) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/flashsales",
		activityBody(skuID, uniqueName("秒杀"), 9900, stock, perUserLimit, fromOffset, untilOffset), admin)
	require.Equal(t, http.StatusCreated, w.Code, "创建活动失败: %s", w.Body.String())
	id, ok := body["id"].(float64)
	require.True(t, ok)
	return int64(id)
}

// stockKey / countKey / idemKey 与 service 保持一致（DESIGN.md key 约定）。
func stockKey(id int64) string { return fmt.Sprintf("flashsale:stock:%d", id) }
func countKey(id, userID int64) string {
	return fmt.Sprintf("flashsale:count:%d:%d", id, userID)
}
func slotIdemKey(id, userID, purchaseSlot int64) string {
	return fmt.Sprintf("flashsale:idem:%d:%d:%d", id, userID, purchaseSlot)
}
func rlKey(userID int64) string { return fmt.Sprintf("flashsale:rl:%d", userID) }

func redisStock(t *testing.T, env *testEnv, id int64) int {
	t.Helper()
	v, err := env.redis.Get(context.Background(), stockKey(id)).Int()
	require.NoError(t, err, "预热库存 key 应存在")
	return v
}

// ---- 测试 ----

// 权限：游客/普通用户不能管理秒杀活动；admin 可。
func TestAdminFlashSaleRequireAdmin(t *testing.T) {
	env := requireEnv(t)
	body := activityBody(1, "x", 100, 10, 1, -time.Minute, time.Hour)

	w, _ := doJSON(t, env, http.MethodPost, "/api/admin/flashsales", body, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	userToken := registerAndToken(t, env, uniqueName("mallory"))
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/flashsales", body, userToken)
	require.Equal(t, http.StatusForbidden, w.Code)
}

// 闭环：创建（默认限购 1）→ 列表可见 → 编辑 → 上架预热（Redis 与配置一致）
// → 预扣成功（真实 Lua）→ 下架（清除预热 + 抢购被拒）→ 重新上架（重新预热）。
func TestFlashSaleLifecycleClosedLoop(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)

	// 创建：per_user_limit 缺省 → 默认 1。
	w, created := doJSON(t, env, http.MethodPost, "/api/admin/flashsales",
		activityBody(skuID, uniqueName("限时秒杀"), 9900, 100, 0, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusCreated, w.Code, "创建活动失败: %s", w.Body.String())
	id := int64(created["id"].(float64))
	require.Equal(t, float64(1), created["per_user_limit"], "per_user_limit 应默认 1")
	require.Equal(t, "off_sale", created["status"])

	// 列表可见。
	w, list := doJSON(t, env, http.MethodGet, "/api/admin/flashsales", "", admin)
	require.Equal(t, http.StatusOK, w.Code)
	found := false
	for _, it := range list["items"].([]any) {
		if int64(it.(map[string]any)["id"].(float64)) == id {
			found = true
		}
	}
	require.True(t, found, "活动应在 admin 列表")

	// 编辑（库存 120，限购 2）→ 204。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBody(skuID, uniqueName("改后秒杀"), 8800, 120, 2, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 上架：预热库存与配置一致。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 120, redisStock(t, env, id))

	// 预扣：成功一次，Redis 库存减一、用户计数加一。
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 7, id))
	require.Equal(t, 119, redisStock(t, env, id))
	cnt, err := env.redis.Get(context.Background(), countKey(id, 7)).Int()
	require.NoError(t, err)
	require.Equal(t, 1, cnt)

	// 下架：状态翻转、预热清除、抢购被拒。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/unpublish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	_, err = env.redis.Get(context.Background(), stockKey(id)).Result()
	require.ErrorIs(t, err, redis.Nil, "下架应清除预热库存")
	require.ErrorIs(t, env.flashsaleSvc.PreDeduct(context.Background(), 7, id), flashsalesvc.ErrOffline, "下架后抢购应被拒")

	// 重新上架：重新预热为配置库存。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 120, redisStock(t, env, id))

	// 编辑已上架活动 → 204（覆盖已上架场景）。
	current, err := flashsalerepo.NewGORMActivity(env.gdb).GetByID(context.Background(), id)
	require.NoError(t, err)
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBodyAt(skuID, uniqueName("再编辑"), 8800, 100, 2, current.StartAt, current.EndAt), admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 100, redisStock(t, env, id))
}

// 上架预热一致性：上架后 Redis 库存 == 配置库存；未开始的活动上架可覆盖存量。
func TestPublishPrewarmConsistency(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)

	// 未开始的活动：上架后预热为配置库存。
	id := createActivity(t, env, admin, skuID, 50, 1, time.Hour, 2*time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 50, redisStock(t, env, id))

	// 编辑未开始活动（配置库存变化）→ 预热库存覆盖为新值（DEL+SET）。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBody(skuID, "改", 9900, 80, 1, time.Hour, 2*time.Hour), admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 80, redisStock(t, env, id))
}

// 进行中编辑库存只减不增：调高被拒（409，DB 与 Redis 均不变）；调低生效（Redis 同步降低）。
func TestInProgressStockOnlyDecreases(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)

	id := createActivity(t, env, admin, skuID, 100, 1, -time.Minute, time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 100, redisStock(t, env, id))

	// 部分预扣后：Redis 存量 95。
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 1, id))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 2, id))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 3, id))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 4, id))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 5, id))
	require.Equal(t, 95, redisStock(t, env, id))
	current, err := flashsalerepo.NewGORMActivity(env.gdb).GetByID(context.Background(), id)
	require.NoError(t, err)

	// 调高库存 → 409；DB 与 Redis 均不变。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBodyAt(skuID, "调高", 9900, 200, 1, current.StartAt, current.EndAt), admin)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, 95, redisStock(t, env, id))
	var dbStock int
	require.NoError(t, env.gdb.Table("flashsale_activities").Select("stock").Where("id = ?", id).Scan(&dbStock).Error)
	require.Equal(t, 100, dbStock, "进行中调高库存不应写入 DB")

	// 调低库存 → 204；DB 与 Redis 同步降低。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBodyAt(skuID, "调低", 9900, 90, 1, current.StartAt, current.EndAt), admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 85, redisStock(t, env, id), "Redis preserves the five accepted pre-deductions")
	require.NoError(t, env.gdb.Table("flashsale_activities").Select("stock").Where("id = ?", id).Scan(&dbStock).Error)
	require.Equal(t, 90, dbStock)
}

func TestInProgressEditSyncFailureFailsClosed(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)
	id := createActivity(t, env, admin, skuID, 100, 1, -time.Minute, time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)

	activities := flashsalerepo.NewGORMActivity(env.gdb)
	current, err := activities.GetByID(context.Background(), id)
	require.NoError(t, err)
	svc := flashsalesvc.New(
		flashsalerepo.Store{Activities: activities, PreDeductions: flashsalerepo.NewGORMPreDeduction(env.gdb), Tx: activities},
		env.productSvc, failingDecreaseCache{Client: env.cacheClient}, limiter.RedisCounterConfig{},
		&fakePublisher{}, &fakeNos{next: time.Now().UnixNano()}, metrics.New().Business(),
	)
	h := flashsalehandler.New(svc, env.verifier)
	router := gin.New()
	h.RegisterRoutes(router.Group("/api"), allowAll)

	w, _ = doJSONOn(t, router, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBodyAt(skuID, "同步失败", 9900, 90, 1, current.StartAt, current.EndAt), admin)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	var stored struct {
		Stock  int
		Status string
	}
	require.NoError(t, env.gdb.Table("flashsale_activities").Select("stock", "status").Where("id = ?", id).Scan(&stored).Error)
	require.Equal(t, 90, stored.Stock, "MySQL stock commits offline before Redis synchronization")
	require.Equal(t, "off_sale", stored.Status, "sync failure must persistently fail closed")
	_, err = env.redis.Get(context.Background(), stockKey(id)).Result()
	require.ErrorIs(t, err, redis.Nil, "stale sellable stock must be removed")

	user := registerAndToken(t, env, uniqueName("syncfail"))
	w, _ = purchase(t, router, id, user)
	require.Equal(t, http.StatusConflict, w.Code, "failed synchronization must not continue selling")
}

func TestInProgressEditRecomputesDeltaAfterConcurrentConsumer(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)
	id := createActivity(t, env, admin, skuID, 100, 1, -time.Minute, time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 987654, id))
	require.Equal(t, 99, redisStock(t, env, id))

	activities := flashsalerepo.NewGORMActivity(env.gdb)
	paused := make(chan struct{})
	proceed := make(chan struct{})
	svc := flashsalesvc.New(
		flashsalerepo.Store{Activities: activities, PreDeductions: flashsalerepo.NewGORMPreDeduction(env.gdb), Tx: activities},
		env.productSvc, blockingPauseCache{Client: env.cacheClient, paused: paused, proceed: proceed},
		limiter.RedisCounterConfig{}, &fakePublisher{}, &fakeNos{next: time.Now().UnixNano()}, metrics.New().Business(),
	)
	current, err := activities.GetByID(context.Background(), id)
	require.NoError(t, err)
	editDone := make(chan error, 1)
	go func() {
		editDone <- svc.UpdateActivity(context.Background(), id, flashsalesvc.ActivityParams{
			SKUID: current.SKUID, Title: current.Title, Price: current.Price, Stock: 90,
			PerUserLimit: current.PerUserLimit, StartAt: current.StartAt, EndAt: current.EndAt,
		})
	}()
	<-paused
	require.NoError(t, activities.WithinTx(context.Background(), func(tx *gorm.DB) error {
		ok, deductErr := activities.DeductStock(context.Background(), tx, id, 1)
		require.True(t, ok)
		return deductErr
	}))
	close(proceed)
	require.NoError(t, <-editDone)
	var mysqlRemaining int
	require.NoError(t, env.gdb.Table("flashsale_activities").Select("stock").Where("id = ?", id).Scan(&mysqlRemaining).Error)
	require.Equal(t, 90, mysqlRemaining)
	require.Equal(t, 90, redisStock(t, env, id), "delta must be recomputed from the row-locked MySQL stock")
}

// 预扣边界（真实 Redis Lua）：抢光 / 超限购 / 窗口外 / 不存在的活动。
func TestPreDeductBoundaries(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)

	// 库存 1：第一个用户成功，第二个用户抢光。
	single := createActivity(t, env, admin, skuID, 1, 1, -time.Minute, time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", single), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 1, single))
	require.ErrorIs(t, env.flashsaleSvc.PreDeduct(context.Background(), 2, single), flashsalesvc.ErrSoldOut)

	// 限购 2：同一用户第三次被拒（用户计数落在 Redis）。
	limited := createActivity(t, env, admin, skuID, 10, 2, -time.Minute, time.Hour)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", limited), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 3, limited))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 3, limited))
	require.ErrorIs(t, env.flashsaleSvc.PreDeduct(context.Background(), 3, limited), flashsalesvc.ErrLimitReached)
	require.Equal(t, 8, redisStock(t, env, limited), "超限购拒绝不应扣减库存")

	// 未开始：窗口外被拒。
	notStarted := createActivity(t, env, admin, skuID, 10, 1, time.Hour, 2*time.Hour)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", notStarted), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.ErrorIs(t, env.flashsaleSvc.PreDeduct(context.Background(), 4, notStarted), flashsalesvc.ErrNotInWindow)

	// 活动不存在。
	require.ErrorIs(t, env.flashsaleSvc.PreDeduct(context.Background(), 4, 999999), flashsalesvc.ErrActivityNotFound)
}

// 活动边界：不存在的活动 404；非法参数 400；已结束的活动不可上架；SKU 不存在 400。
func TestActivityEdgeCases(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)

	w, _ := doJSON(t, env, http.MethodPut, "/api/admin/flashsales/999999",
		activityBody(skuID, "x", 100, 10, 1, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusNotFound, w.Code)

	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/flashsales",
		activityBody(999999, "x", 100, 10, 1, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusBadRequest, w.Code, "SKU 不存在应 400")

	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/flashsales",
		activityBody(skuID, "x", 0, 10, 1, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusBadRequest, w.Code, "秒杀价为 0 应 400")

	// 已结束的活动不可上架。
	ended := createActivity(t, env, admin, skuID, 10, 1, -2*time.Hour, -time.Hour)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", ended), "", admin)
	require.Equal(t, http.StatusBadRequest, w.Code, "已结束的活动不可上架")

	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/flashsales/999999/publish", "", admin)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// T23 秒杀页活动列表（用户视角）：仅返回已上架且未结束的活动，
// 携带 server_time（倒计时对齐）、剩余库存（Redis 预扣余量）、
// 派生状态与 SKU/商品摘要。
func TestFlashSaleUserActivityList(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	skuID := seedSKU(t, env, admin)
	user := registerAndToken(t, env, uniqueName("shopper"))

	// 进行中 + 即将开始各一个，上架；下架一个；已结束一个（上架后改窗口到过去）。
	inProgress := createActivity(t, env, admin, skuID, 100, 2, -time.Minute, time.Hour)
	notStarted := createActivity(t, env, admin, skuID, 50, 1, time.Hour, 2*time.Hour)
	offSale := createActivity(t, env, admin, skuID, 30, 1, -time.Minute, time.Hour)
	ended := createActivity(t, env, admin, skuID, 20, 1, -time.Minute, time.Hour)
	for _, id := range []int64{inProgress, notStarted, ended} {
		w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
		require.Equal(t, http.StatusNoContent, w.Code)
	}
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/unpublish", offSale), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.NoError(t, env.gdb.Exec("UPDATE flashsale_activities SET start_at = DATE_SUB(NOW(), INTERVAL 2 HOUR), end_at = DATE_SUB(NOW(), INTERVAL 1 HOUR) WHERE id = ?", ended).Error)

	// 游客无权访问；普通用户可访问。
	w, _ = doJSON(t, env, http.MethodGet, "/api/flashsales", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w, body := doJSON(t, env, http.MethodGet, "/api/flashsales", "", user)
	require.Equal(t, http.StatusOK, w.Code, "秒杀页列表失败: %s", w.Body.String())

	serverTime, ok := body["server_time"].(string)
	require.True(t, ok, "列表应携带 server_time")
	_, parseErr := time.Parse(time.RFC3339, serverTime)
	require.NoError(t, parseErr, "server_time 应为 RFC3339")

	items := body["items"].([]any)
	byID := map[int64]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		require.NotEqual(t, float64(offSale), m["id"], "下架活动不应出现")
		require.NotEqual(t, float64(ended), m["id"], "已结束活动不应出现")
		byID[int64(m["id"].(float64))] = m
	}
	ip, hasInProgress := byID[inProgress]
	require.True(t, hasInProgress)
	require.Equal(t, "in_progress", ip["state"])
	require.Equal(t, float64(2), ip["per_user_limit"])
	require.Equal(t, float64(100), ip["stock"], "剩余库存与预热一致")
	require.Equal(t, float64(19900), ip["sku"].(map[string]any)["price"], "SKU 原价")
	require.NotEmpty(t, ip["product_title"])

	ns, hasNotStarted := byID[notStarted]
	require.True(t, hasNotStarted)
	require.Equal(t, "not_started", ns["state"])

	// 预扣后列表反映 Redis 余量（95），而非配置库存。
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 7, inProgress))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 8, inProgress))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 9, inProgress))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 10, inProgress))
	require.NoError(t, env.flashsaleSvc.PreDeduct(context.Background(), 11, inProgress))
	w, body = doJSON(t, env, http.MethodGet, "/api/flashsales", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["id"].(float64)) == inProgress {
			require.Equal(t, float64(95), m["stock"], "列表剩余库存应取 Redis 预扣余量")
		}
	}
}
