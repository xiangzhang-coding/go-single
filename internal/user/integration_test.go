// 集成测试（主 seam）：真实 MySQL（docker compose）+ httptest 起完整路由，
// 覆盖注册 / 登录 / 鉴权 / 对象级授权（防 IDOR）。
package user_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/requestbody"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	testDBName    = "go_shop_test_user"
	testSecret    = "integration-test-secret"
	migrationsDir = "../../migrations"
)

// testEnv 每个测试包只构建一次；MySQL 不可达时本地跳过、CI 失败。
type testEnv struct {
	router   http.Handler
	verifier auth.TokenVerifier
	userSvc  usersvc.Service
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
	testsupport.RequireDependency(t, "MySQL", envErr)
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
	// 用 root 建测试库并授权（compose 的 shop 用户仅有 go_shop.* 权限）。
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
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

	verifier := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})
	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, verifier)
	handler := userhandler.New(userSvc, verifier, testsupport.AllowAllAuthAttempts{})
	addressHandler := userhandler.NewAddress(userSvc, verifier)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	jsonLimit, err := requestbody.LimitJSON(64 << 10)
	if err != nil {
		return nil, err
	}
	api.Use(jsonLimit)
	handler.RegisterRoutes(api)
	addressHandler.RegisterRoutes(api)
	return &testEnv{router: r, verifier: verifier, userSvc: userSvc, gdb: gdb}, nil
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

func doJSON(t *testing.T, env *testEnv, method, path, body string, token string) (*httptest.ResponseRecorder, map[string]any) {
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

func registerUser(t *testing.T, env *testEnv, username, password string) map[string]any {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password), "")
	require.Equal(t, http.StatusCreated, w.Code, "注册失败: %s", w.Body.String())
	return body
}

func login(t *testing.T, env *testEnv, username, password string) (int, map[string]any) {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password), "")
	return w.Code, body
}

func tokenOf(t *testing.T, body map[string]any) string {
	t.Helper()
	tok, ok := body["token"].(string)
	require.True(t, ok, "响应缺少 token: %v", body)
	require.NotEmpty(t, tok)
	return tok
}

func requireUserResponseShape(t *testing.T, body map[string]any) {
	t.Helper()
	testsupport.AssertExactJSONKeys(t, body,
		"id", "username", "nickname", "avatar_url", "role", "created_at", "updated_at")
	require.IsType(t, float64(0), body["id"])
	for _, field := range []string{"username", "nickname", "avatar_url", "role", "created_at", "updated_at"} {
		require.IsType(t, "", body[field], "field %q", field)
	}
}

// 注册 → 登录 → 携带 token 访问受保护接口 全链路。
func TestRegisterLoginAndMe(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("alice_%d", time.Now().UnixNano())

	reg := registerUser(t, env, username, "secret123")
	requireUserResponseShape(t, reg)
	require.Equal(t, username, reg["username"])
	require.Equal(t, "user", reg["role"])

	code, loginBody := login(t, env, username, "secret123")
	require.Equal(t, http.StatusOK, code)
	testsupport.AssertExactJSONKeys(t, loginBody, "token", "user")
	loginUser, ok := loginBody["user"].(map[string]any)
	require.True(t, ok, "login user = %#v, want JSON object", loginBody["user"])
	requireUserResponseShape(t, loginUser)
	require.Equal(t, username, loginUser["username"])
	require.Equal(t, "user", loginUser["role"])
	token := tokenOf(t, loginBody)

	w, me := doJSON(t, env, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	requireUserResponseShape(t, me)
	require.Equal(t, username, me["username"])
	require.Equal(t, reg["id"], me["id"])
}

// 密码错误 / 未知用户 → 401。
func TestLoginRejected(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("bob_%d", time.Now().UnixNano())
	registerUser(t, env, username, "secret123")

	code, body := login(t, env, username, "wrong-pass")
	require.Equal(t, http.StatusUnauthorized, code)
	testsupport.AssertAPIError(t, body, "invalid username or password")

	code, body = login(t, env, "ghost_"+username, "secret123")
	require.Equal(t, http.StatusUnauthorized, code)
	testsupport.AssertAPIError(t, body, "invalid username or password")
}

// 未带 / 伪造 / 过期 token 访问受保护接口 → 401。
func TestProtectedEndpointRequiresToken(t *testing.T) {
	env := requireEnv(t)

	w, body := doJSON(t, env, http.MethodGet, "/api/users/me", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	testsupport.AssertAPIError(t, body)

	w, body = doJSON(t, env, http.MethodGet, "/api/users/me", "", "garbage.token.here")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	testsupport.AssertAPIError(t, body)

	// 过期 token：用负 TTL 签发已过期令牌。
	expired := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: -time.Hour})
	stale, err := expired.Issue(1, "user")
	require.NoError(t, err)
	w, body = doJSON(t, env, http.MethodGet, "/api/users/me", "", stale)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	testsupport.AssertAPIError(t, body)
}

// admin 种子账号（migration 种入）可登录且 role=admin。
func TestAdminSeedLogin(t *testing.T) {
	env := requireEnv(t)

	code, body := login(t, env, "admin", "admin123")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "admin", body["user"].(map[string]any)["role"])
	token := tokenOf(t, body)

	w, me := doJSON(t, env, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "admin", me["role"])
	require.Equal(t, "admin", me["username"])
}

// 对象级授权：普通用户访问他人资料 → 403；admin 可见；不存在 → 404。
func TestCrossUserDenied(t *testing.T) {
	env := requireEnv(t)
	uid := time.Now().UnixNano()

	userA := fmt.Sprintf("ava_%d", uid)
	userB := fmt.Sprintf("bob_%d", uid)
	regA := registerUser(t, env, userA, "secret123")
	regB := registerUser(t, env, userB, "secret123")
	_, loginA := login(t, env, userA, "secret123")
	tokenA := tokenOf(t, loginA)

	// A 访问 B 的资料 → 403。
	w, body := doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/users/%v", regB["id"]), "", tokenA)
	require.Equal(t, http.StatusForbidden, w.Code)
	testsupport.AssertAPIError(t, body)

	// A 访问自己 → 200。
	w, _ = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/users/%v", regA["id"]), "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)

	// admin 访问任意用户 → 200。
	_, adminBody := login(t, env, "admin", "admin123")
	adminToken := tokenOf(t, adminBody)
	w, _ = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/users/%v", regB["id"]), "", adminToken)
	require.Equal(t, http.StatusOK, w.Code)

	// 不存在用户 → 404。
	w, body = doJSON(t, env, http.MethodGet, "/api/users/999999", "", adminToken)
	require.Equal(t, http.StatusNotFound, w.Code)
	testsupport.AssertAPIError(t, body)
}

// 重复注册 → 409；非法输入 → 400。
func TestRegisterValidation(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("carol_%d", time.Now().UnixNano())

	registerUser(t, env, username, "secret123")

	w, body := doJSON(t, env, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret456"}`, username), "")
	require.Equal(t, http.StatusConflict, w.Code)
	testsupport.AssertAPIError(t, body)

	w, body = doJSON(t, env, http.MethodPost, "/api/auth/register", `{"username":"x","password":"12345"}`, "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	testsupport.AssertAPIError(t, body)

	w, body = doJSON(t, env, http.MethodPost, "/api/auth/register", `{"username":""}`, "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	testsupport.AssertAPIError(t, body)
}

func TestConcurrentLoginHasAccountBudget(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("ll_%d", time.Now().UnixNano())
	registerUser(t, env, username, "secret123")
	router := authLimitedRouter(t, env, limiter.AuthAttemptsConfig{
		Login:    limiter.AuthAttemptConfig{PerIPMax: 100, PerAccountMax: 3, Window: time.Minute},
		Register: limiter.AuthAttemptConfig{PerIPMax: 100, PerAccountMax: 3, Window: time.Minute},
	})

	statuses := concurrentAuthRequests(router, "/api/auth/login", username, "wrong-pass", 10)
	require.Equal(t, 3, statuses[http.StatusUnauthorized])
	require.Equal(t, 7, statuses[http.StatusTooManyRequests])
}

func TestLoginAccountBudgetMatchesMySQLUsernameCollation(t *testing.T) {
	env := requireEnv(t)
	for _, tc := range []struct {
		name       string
		username   func(string) string
		equivalent func(string) string
	}{
		{name: "ligature", username: func(s string) string { return "oe_" + s }, equivalent: func(s string) string { return "œ_" + s }},
		{name: "trailing_space", username: func(s string) string { return "space_" + s }, equivalent: func(s string) string { return "space_" + s + " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := fmt.Sprint(time.Now().UnixNano())
			username := tc.username(suffix)
			registerUser(t, env, username, "secret123")
			router := authLimitedRouter(t, env, limiter.AuthAttemptsConfig{
				Login:    limiter.AuthAttemptConfig{PerIPMax: 100, PerAccountMax: 1, Window: time.Minute},
				Register: limiter.AuthAttemptConfig{PerIPMax: 100, PerAccountMax: 3, Window: time.Minute},
			})

			require.Equal(t, http.StatusUnauthorized,
				authRequestStatus(router, "/api/auth/login", username, "wrong-pass", "192.0.2.1:1234"))
			require.Equal(t, http.StatusTooManyRequests,
				authRequestStatus(router, "/api/auth/login", tc.equivalent(suffix), "wrong-pass", "192.0.2.2:1234"))
		})
	}
}

func TestConcurrentDuplicateRegistrationIsBounded(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("lr_%d", time.Now().UnixNano())
	router := authLimitedRouter(t, env, limiter.AuthAttemptsConfig{
		Login:    limiter.AuthAttemptConfig{PerIPMax: 100, PerAccountMax: 3, Window: time.Minute},
		Register: limiter.AuthAttemptConfig{PerIPMax: 100, PerAccountMax: 3, Window: time.Minute},
	})

	statuses := concurrentAuthRequests(router, "/api/auth/register", username, "secret123", 10)
	require.Equal(t, 1, statuses[http.StatusCreated])
	require.Equal(t, 2, statuses[http.StatusConflict])
	require.Equal(t, 7, statuses[http.StatusTooManyRequests])
}

func TestOversizedJSONRejectedBeforeRegistration(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("oversize_%d", time.Now().UnixNano())
	body := fmt.Sprintf(`{"username":%q,"password":"secret123","padding":%q}`, username, strings.Repeat("x", 64<<10))
	w, response := doJSON(t, env, http.MethodPost, "/api/auth/register", body, "")
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	testsupport.AssertAPIError(t, response)

	code, response := login(t, env, username, "secret123")
	require.Equal(t, http.StatusUnauthorized, code)
	testsupport.AssertAPIError(t, response)
}

func authLimitedRouter(t *testing.T, env *testEnv, cfg limiter.AuthAttemptsConfig) http.Handler {
	t.Helper()
	cacheClient, err := cache.NewRedis(envOr("GO_SINGLE_REDIS_ADDR", "127.0.0.1:6379"),
		envOr("GO_SINGLE_REDIS_PASSWORD", ""), 13)
	testsupport.RequireDependency(t, "Redis", err)
	t.Cleanup(func() { _ = cacheClient.Close() })
	authLimits, err := limiter.NewAuthAttempts(cacheClient, env.userSvc, cfg)
	require.NoError(t, err)
	jsonLimit, err := requestbody.LimitJSON(64 << 10)
	require.NoError(t, err)

	r := gin.New()
	api := r.Group("/api")
	api.Use(jsonLimit)
	userhandler.New(env.userSvc, env.verifier, authLimits).RegisterRoutes(api)
	return r
}

func concurrentAuthRequests(router http.Handler, path, username, password string, count int) map[int]int {
	statuses := make(chan int, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses <- authRequestStatus(router, path, username, password, fmt.Sprintf("192.0.2.%d:1234", i+1))
		}(i)
	}
	wg.Wait()
	close(statuses)

	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	return counts
}

func authRequestStatus(router http.Handler, path, username, password, remoteAddr string) int {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Code
}

// 用户名前缀搜索：命中、排除自己、鉴权与校验。
func TestUserSearchByPrefix(t *testing.T) {
	env := requireEnv(t)
	uid := time.Now().UnixNano()

	prefix := fmt.Sprintf("sea_%d", uid)
	userA := prefix + "_a"
	userB := prefix + "_b"
	other := "zz_" + fmt.Sprint(uid)
	registerUser(t, env, userA, "secret123")
	registerUser(t, env, userB, "secret123")
	registerUser(t, env, other, "secret123")

	_, loginA := login(t, env, userA, "secret123")
	tokenA := tokenOf(t, loginA)

	// 前缀命中：返回 a 与 b，且不含自己（登录账号自身）。
	w, body := doJSON(t, env, http.MethodGet, "/api/users?username="+prefix, "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)
	items, ok := body["items"].([]any)
	require.True(t, ok)
	usernames := make([]string, 0, len(items))
	for _, it := range items {
		m := it.(map[string]any)
		usernames = append(usernames, m["username"].(string))
	}
	require.Contains(t, usernames, userB)
	require.NotContains(t, usernames, userA)
	w, body = doJSON(t, env, http.MethodGet, "/api/users?username="+prefix+"&limit=1", "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)
	limited := body["items"].([]any)
	require.Len(t, limited, 1, "排除自己必须发生在 SQL LIMIT 前")
	require.Equal(t, userB, limited[0].(map[string]any)["username"])

	// 无匹配：空列表。
	w, body = doJSON(t, env, http.MethodGet, "/api/users?username=nomatch_zz_"+fmt.Sprint(uid), "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, body["items"])

	// 未携带 token → 401。
	w, body = doJSON(t, env, http.MethodGet, "/api/users?username="+prefix, "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	testsupport.AssertAPIError(t, body)

	// 空前缀 → 400。
	w, body = doJSON(t, env, http.MethodGet, "/api/users?username=", "", tokenA)
	require.Equal(t, http.StatusBadRequest, w.Code)
	testsupport.AssertAPIError(t, body)
}

func TestUserSearchTreatsWildcardsLiterallyAndReturnsPublicFields(t *testing.T) {
	env := requireEnv(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	seeker := "find" + suffix
	registerUser(t, env, seeker, "secret123")
	_, loginBody := login(t, env, seeker, "secret123")
	token := tokenOf(t, loginBody)

	base := "lit" + suffix
	underscoreMatch := base + "_one"
	underscoreDecoy := base + "xone"
	percentMatch := base + "%two"
	percentDecoy := base + "ytwo"
	for _, username := range []string{underscoreMatch, underscoreDecoy, percentMatch, percentDecoy} {
		registerUser(t, env, username, "secret123")
	}

	for _, tc := range []struct {
		prefix string
		want   string
		not    string
	}{
		{prefix: base + "_", want: underscoreMatch, not: underscoreDecoy},
		{prefix: base + "%", want: percentMatch, not: percentDecoy},
	} {
		w, body := doJSON(t, env, http.MethodGet, "/api/users?username="+url.QueryEscape(tc.prefix), "", token)
		require.Equal(t, http.StatusOK, w.Code)
		items := body["items"].([]any)
		require.Len(t, items, 1)
		item := items[0].(map[string]any)
		require.Equal(t, tc.want, item["username"])
		require.NotEqual(t, tc.not, item["username"])
		require.ElementsMatch(t, []string{"id", "username"}, mapKeys(item))
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
