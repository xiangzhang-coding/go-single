// 集成测试（主 seam）：真实 MySQL + Redis（docker compose）+ httptest 起完整路由，
// 覆盖 发券→领券→我的券 闭环、并发领券不超发（总量/每人限领）、过期券标识、权限。
package coupon_test

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

	couponhandler "github.com/xiangzhang-coding/go-single/internal/coupon/handler"
	couponrepo "github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	testDBName    = "go_shop_test"
	testSecret    = "integration-test-secret"
	migrationsDir = "../../migrations"
	redisAddr     = "127.0.0.1:6379"
	redisTestDB   = 15
)

// testEnv 每个测试包只构建一次；MySQL 或 Redis 不可达时整体跳过。
type testEnv struct {
	router   http.Handler
	verifier auth.TokenVerifier
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
	if envErr != nil {
		t.Skipf("MySQL/Redis 不可达，跳过集成测试（先 docker compose up -d）：%v", envErr)
	}
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
	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier)
	userHandler := userhandler.New(userSvc, verifier)
	couponHandler := couponhandler.New(
		couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(gdb), UserCoupon: couponrepo.NewGORMUserCoupon(gdb)}, cacheClient),
		verifier,
	)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	couponHandler.RegisterRoutes(api)
	return &testEnv{router: r, verifier: verifier, gdb: gdb}, nil
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
	require.Equal(t, http.StatusCreated, w.Code, "注册失败: %s", w.Body.String())
	_, login := doJSON(t, env, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	return login["token"].(string)
}

func uniqueName(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()) }

// templateBody 构造券模板请求体；now 偏移单位均为分钟。
// min_amount=0 时按直减券（direct）构造，否则为满减券（threshold）。
func templateBody(name string, total, perUserLimit int, value, minAmount int64, fromOffset, untilOffset time.Duration) string {
	typ := "direct"
	if minAmount > 0 {
		typ = "threshold"
	}
	from := time.Now().Add(fromOffset).Format(time.RFC3339)
	until := time.Now().Add(untilOffset).Format(time.RFC3339)
	return fmt.Sprintf(`{"name":%q,"type":%q,"value":%d,"min_amount":%d,"total":%d,"per_user_limit":%d,"valid_from":%q,"valid_until":%q}`,
		name, typ, value, minAmount, total, perUserLimit, from, until)
}

func createTemplate(t *testing.T, env *testEnv, token, name string, total, perUserLimit int, value, minAmount int64, fromOffset, untilOffset time.Duration) int64 {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/admin/coupons",
		templateBody(name, total, perUserLimit, value, minAmount, fromOffset, untilOffset), token)
	require.Equal(t, http.StatusCreated, w.Code, "发布券模板失败: %s", w.Body.String())
	id, ok := body["id"].(float64)
	require.True(t, ok)
	return int64(id)
}

// ---- 测试 ----

// 权限：游客/普通用户不能发布券模板；admin 可。
func TestAdminCouponRequireAdmin(t *testing.T) {
	env := requireEnv(t)

	w, _ := doJSON(t, env, http.MethodPost, "/api/admin/coupons", templateBody("x", 10, 1, 100, 0, -time.Minute, time.Hour), "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	userToken := registerAndToken(t, env, uniqueName("mallory"))
	w, _ = doJSON(t, env, http.MethodPost, "/api/admin/coupons", templateBody("x", 10, 1, 100, 0, -time.Minute, time.Hour), userToken)
	require.Equal(t, http.StatusForbidden, w.Code)
}

// 发券→领券→我的券列表 闭环：admin 发布模板，用户浏览可领、领取、
// 我的券可见（含模板信息与未用状态），可领券列表状态随之变化。
func TestCouponLifecycleClosedLoop(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	user := registerAndToken(t, env, uniqueName("alice"))

	// admin 发布券模板（满减：满100减20，总量100，每人限领1）。
	tmplID := createTemplate(t, env, admin, uniqueName("满100减20"), 100, 1, 2000, 10000, -time.Minute, time.Hour)

	// 用户浏览可领券列表：该模板可领。
	w, list := doJSON(t, env, http.MethodGet, "/api/coupons", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	item := findItem(t, list, tmplID, "可领券列表应包含新模板")
	require.Equal(t, "claimable", item["state"])
	require.Equal(t, float64(2000), item["value"])
	require.Equal(t, float64(10000), item["min_amount"])

	// 领取 → 201。
	w, claimed := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", user)
	require.Equal(t, http.StatusCreated, w.Code, "领券失败: %s", w.Body.String())
	require.Equal(t, float64(tmplID), claimed["template_id"])
	require.Equal(t, "unused", claimed["status"])

	// 我的券列表：1 张，含模板信息。
	w, mine := doJSON(t, env, http.MethodGet, "/api/coupons/mine", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), mine["total"])
	coupon := mine["items"].([]any)[0].(map[string]any)
	require.Equal(t, float64(tmplID), coupon["template_id"])
	require.Equal(t, "unused", coupon["status"])
	require.Equal(t, float64(2000), coupon["value"])
	require.Equal(t, float64(10000), coupon["min_amount"])
	require.NotEmpty(t, coupon["name"])

	// 用户已达每人限领 → 可领券列表状态变为 limit_reached。
	w, list = doJSON(t, env, http.MethodGet, "/api/coupons", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	item = findItem(t, list, tmplID, "模板应仍在可领券列表")
	require.Equal(t, "limit_reached", item["state"])
}

// findItem 在 items 中按 id 查找条目。
func findItem(t *testing.T, body map[string]any, id int64, msg string) map[string]any {
	t.Helper()
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["id"].(float64)) == id {
			return m
		}
	}
	require.Fail(t, msg)
	return nil
}

// 并发领券不超发：20 个用户抢 total=5，恰好 5 人成功，其余 409，DB 落库 5 张。
func TestClaimConcurrentNoOversell(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)

	const (
		workers = 20
		total   = 5
	)
	tmplID := createTemplate(t, env, admin, uniqueName("限量券"), total, 1, 1000, 0, -time.Minute, time.Hour)

	tokens := make([]string, workers)
	for i := 0; i < workers; i++ {
		tokens[i] = registerAndToken(t, env, uniqueName(fmt.Sprintf("racer_%d", i)))
	}

	var wg sync.WaitGroup
	codes := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", tokens[i])
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, code := range codes {
		if code == http.StatusCreated {
			ok++
		} else {
			require.Equal(t, http.StatusConflict, code, "抢光后应 409")
		}
	}
	require.Equal(t, total, ok, "并发领券不得超发")

	// DB 落库数 = 成功数；Redis 总量计数 = 成功数。
	var dbCount int64
	require.NoError(t, env.gdb.Table("user_coupons").Where("template_id = ?", tmplID).Count(&dbCount).Error)
	require.Equal(t, int64(total), dbCount)
	ctx := context.Background()
	rc := redis.NewClient(&redis.Options{Addr: redisAddr, DB: redisTestDB})
	defer rc.Close()
	claimed, err := rc.Get(ctx, fmt.Sprintf("coupon:claimed:%d", tmplID)).Int()
	require.NoError(t, err)
	require.Equal(t, total, claimed)
}

// 每人限领原子强制：同一用户并发领取 per_user_limit=2，恰好 2 张。
func TestClaimConcurrentPerUserLimit(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	user := registerAndToken(t, env, uniqueName("greedy"))

	const workers = 10
	tmplID := createTemplate(t, env, admin, uniqueName("限领券"), 100, 2, 500, 0, -time.Minute, time.Hour)

	var wg sync.WaitGroup
	codes := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", user)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, code := range codes {
		if code == http.StatusCreated {
			ok++
		} else {
			require.Equal(t, http.StatusConflict, code)
		}
	}
	require.Equal(t, 2, ok, "同一用户并发领取不得超过每人限领")

	var dbCount int64
	require.NoError(t, env.gdb.Table("user_coupons").Where("template_id = ?", tmplID).Count(&dbCount).Error)
	require.Equal(t, int64(2), dbCount)
}

// 过期券正确标识：领取后有效期流逝，我的券派生为 expired，unused 筛选不含它。
func TestExpiredCouponMarked(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	user := registerAndToken(t, env, uniqueName("carol"))

	tmplID := createTemplate(t, env, admin, uniqueName("短期券"), 10, 1, 500, 0, -time.Minute, time.Hour)

	w, claimed := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", user)
	require.Equal(t, http.StatusCreated, w.Code)
	couponID := int64(claimed["id"].(float64))

	// 直接改 DB 让模板过期（模拟时间流逝；读取路径以当前时间派生状态）。
	require.NoError(t, env.gdb.Exec("UPDATE coupon_templates SET valid_until = NOW(3) - INTERVAL 1 MINUTE WHERE id = ?", tmplID).Error)

	w, mine := doJSON(t, env, http.MethodGet, "/api/coupons/mine", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	items := mine["items"].([]any)
	require.Len(t, items, 1)
	item := items[0].(map[string]any)
	require.Equal(t, float64(couponID), item["id"])
	require.Equal(t, "expired", item["status"], "未用且过期的券应标识为 expired")

	// expired 筛选命中；unused 筛选不含过期券。
	w, expired := doJSON(t, env, http.MethodGet, "/api/coupons/mine?status=expired", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(1), expired["total"])
	w, unused := doJSON(t, env, http.MethodGet, "/api/coupons/mine?status=unused", "", user)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, float64(0), unused["total"])

	// 领券请求在有效期外 → 409。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", tmplID), "", registerAndToken(t, env, uniqueName("dave")))
	require.Equal(t, http.StatusConflict, w.Code)
}

// 领券边界：模板不存在 404；未开始/已抢光/超限领 409；非法 status 400。
func TestClaimEdgeCases(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	user := registerAndToken(t, env, uniqueName("bob"))

	// 不存在的模板 → 404。
	w, _ := doJSON(t, env, http.MethodPost, "/api/coupons/999999/claim", "", user)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 未开始 → 409。
	notStarted := createTemplate(t, env, admin, uniqueName("未开始券"), 10, 1, 500, 0, time.Hour, 2*time.Hour)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", notStarted), "", user)
	require.Equal(t, http.StatusConflict, w.Code)

	// 已抢光 → 409（另一用户领掉唯一一张）。
	single := createTemplate(t, env, admin, uniqueName("唯一券"), 1, 1, 500, 0, -time.Minute, time.Hour)
	other := registerAndToken(t, env, uniqueName("eve"))
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", single), "", other)
	require.Equal(t, http.StatusCreated, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/coupons/%d/claim", single), "", user)
	require.Equal(t, http.StatusConflict, w.Code)

	// 非法 status 参数 → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/coupons/mine?status=bogus", "", user)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 游客访问受保护接口 → 401。
	w, _ = doJSON(t, env, http.MethodGet, "/api/coupons/mine", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// 模板编辑：修改总量/有效期后生效，非法参数 400。
func TestTemplateUpdate(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)

	tmplID := createTemplate(t, env, admin, uniqueName("可编辑券"), 10, 1, 1000, 5000, -time.Minute, time.Hour)

	// 编辑 → 204。
	w, _ := doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/coupons/%d", tmplID),
		templateBody(uniqueName("编辑后券"), 20, 2, 1500, 8000, -time.Minute, 2*time.Hour), admin)
	require.Equal(t, http.StatusNoContent, w.Code)

	// admin 列表可见新值。
	w, list := doJSON(t, env, http.MethodGet, "/api/admin/coupons", "", admin)
	require.Equal(t, http.StatusOK, w.Code)
	found := false
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["id"].(float64)) == tmplID {
			found = true
			require.Equal(t, float64(20), m["total"])
			require.Equal(t, float64(2), m["per_user_limit"])
		}
	}
	require.True(t, found)

	// 非法参数（门槛低于面额）→ 400；不存在的模板 → 404。
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/coupons/%d", tmplID),
		templateBody("bad", 10, 1, 1000, 500, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodPut, "/api/admin/coupons/999999",
		templateBody("nope", 10, 1, 1000, 5000, -time.Minute, time.Hour), admin)
	require.Equal(t, http.StatusNotFound, w.Code)
}
