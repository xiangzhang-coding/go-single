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
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	testDBName    = "go_shop_test"
	testSecret    = "integration-test-secret"
	migrationsDir = "../../migrations"
)

// testEnv 每个测试包只构建一次；MySQL 不可达时本地跳过、CI 失败。
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
	handler := userhandler.New(userSvc, verifier)
	addressHandler := userhandler.NewAddress(userSvc, verifier)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	handler.RegisterRoutes(api)
	addressHandler.RegisterRoutes(api)
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

// 注册 → 登录 → 携带 token 访问受保护接口 全链路。
func TestRegisterLoginAndMe(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("alice_%d", time.Now().UnixNano())

	reg := registerUser(t, env, username, "secret123")
	require.Equal(t, "user", reg["role"])

	code, loginBody := login(t, env, username, "secret123")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "user", loginBody["user"].(map[string]any)["role"])
	token := tokenOf(t, loginBody)

	w, me := doJSON(t, env, http.MethodGet, "/api/users/me", "", token)
	require.Equal(t, http.StatusOK, w.Code)
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
	require.NotEmpty(t, body["error"])

	code, _ = login(t, env, "ghost_"+username, "secret123")
	require.Equal(t, http.StatusUnauthorized, code)
}

// 未带 / 伪造 / 过期 token 访问受保护接口 → 401。
func TestProtectedEndpointRequiresToken(t *testing.T) {
	env := requireEnv(t)

	w, _ := doJSON(t, env, http.MethodGet, "/api/users/me", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w, _ = doJSON(t, env, http.MethodGet, "/api/users/me", "", "garbage.token.here")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 过期 token：用负 TTL 签发已过期令牌。
	expired := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: -time.Hour})
	stale, err := expired.Issue(1, "user")
	require.NoError(t, err)
	w, _ = doJSON(t, env, http.MethodGet, "/api/users/me", "", stale)
	require.Equal(t, http.StatusUnauthorized, w.Code)
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
	require.NotEmpty(t, body["error"])

	// A 访问自己 → 200。
	w, _ = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/users/%v", regA["id"]), "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)

	// admin 访问任意用户 → 200。
	_, adminBody := login(t, env, "admin", "admin123")
	adminToken := tokenOf(t, adminBody)
	w, _ = doJSON(t, env, http.MethodGet, fmt.Sprintf("/api/users/%v", regB["id"]), "", adminToken)
	require.Equal(t, http.StatusOK, w.Code)

	// 不存在用户 → 404。
	w, _ = doJSON(t, env, http.MethodGet, "/api/users/999999", "", adminToken)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// 重复注册 → 409；非法输入 → 400。
func TestRegisterValidation(t *testing.T) {
	env := requireEnv(t)
	username := fmt.Sprintf("carol_%d", time.Now().UnixNano())

	registerUser(t, env, username, "secret123")

	w, _ := doJSON(t, env, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret456"}`, username), "")
	require.Equal(t, http.StatusConflict, w.Code)

	w, _ = doJSON(t, env, http.MethodPost, "/api/auth/register", `{"username":"x","password":"12345"}`, "")
	require.Equal(t, http.StatusBadRequest, w.Code)

	w, _ = doJSON(t, env, http.MethodPost, "/api/auth/register", `{"username":""}`, "")
	require.Equal(t, http.StatusBadRequest, w.Code)
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

	// 无匹配：空列表。
	w, body = doJSON(t, env, http.MethodGet, "/api/users?username=nomatch_zz_"+fmt.Sprint(uid), "", tokenA)
	require.Equal(t, http.StatusOK, w.Code)
	require.Empty(t, body["items"])

	// 未携带 token → 401。
	w, _ = doJSON(t, env, http.MethodGet, "/api/users?username="+prefix, "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 空前缀 → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/users?username=", "", tokenA)
	require.Equal(t, http.StatusBadRequest, w.Code)
}
