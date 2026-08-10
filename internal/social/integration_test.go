// 好友关系集成测试（主 seam）：真实 MySQL + httptest 起完整路由，
// 覆盖 申请→通过→好友（双向列表）、拒绝不建关系、重复申请/自加被拒、
// owner 校验（仅被申请人可处理申请）、鉴权与参数校验。
package social_test

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
	socialhandler "github.com/xiangzhang-coding/go-single/internal/social/handler"
	socialrepo "github.com/xiangzhang-coding/go-single/internal/social/repository"
	socialsvc "github.com/xiangzhang-coding/go-single/internal/social/service"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	testDBName    = "go_shop_test"
	testSecret    = "integration-test-secret"
	migrationsDir = "../../migrations"
)

// testEnv 每个测试包只构建一次；MySQL 不可达时测试整体跳过。
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
		t.Skipf("MySQL 不可达，跳过集成测试（先 docker compose up -d mysql）：%v", envErr)
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
	socialSvc := socialsvc.New(
		socialrepo.Store{Requests: socialrepo.NewGORMRequest(gdb), Friendships: socialrepo.NewGORMFriendship(gdb)},
		userSvc,
	)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userhandler.New(userSvc, verifier).RegisterRoutes(api)
	socialhandler.New(socialSvc, verifier).RegisterRoutes(api)
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

// register 注册并登录，返回 (userID, token)。
func register(t *testing.T, env *testEnv, prefix string) (int64, string) {
	t.Helper()
	username := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	w, body := doJSON(t, env, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	require.Equal(t, http.StatusCreated, w.Code, "注册失败: %s", w.Body.String())
	userID := int64(body["id"].(float64))

	w, body = doJSON(t, env, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, username), "")
	require.Equal(t, http.StatusOK, w.Code)
	token := body["token"].(string)
	require.NotEmpty(t, token)
	return userID, token
}

// sendRequest alice 向 bob 发起申请，返回 201 响应体。
func sendRequest(t *testing.T, env *testEnv, token string, toUserID int64) map[string]any {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/friend-requests",
		fmt.Sprintf(`{"to_user_id":%d}`, toUserID), token)
	require.Equal(t, http.StatusCreated, w.Code, "发起申请失败: %s", w.Body.String())
	return body
}

func friendsOf(t *testing.T, env *testEnv, token string) []any {
	t.Helper()
	w, body := doJSON(t, env, http.MethodGet, "/api/friends", "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items, _ := body["items"].([]any)
	return items
}

// ---- 申请→通过→好友（双向）----

func TestFriendRequestAcceptClosedLoop(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_fa")
	bobID, bobToken := register(t, env, "bob_fa")

	// alice 发起申请 → bob 的 incoming 列表出现 pending 申请。
	req := sendRequest(t, env, aliceToken, bobID)
	reqID := int64(req["id"].(float64))
	require.Equal(t, "pending", req["status"])

	w, list := doJSON(t, env, http.MethodGet, "/api/friend-requests?scope=incoming", "", bobToken)
	require.Equal(t, http.StatusOK, w.Code)
	items := list["items"].([]any)
	require.Len(t, items, 1)
	incoming := items[0].(map[string]any)
	require.Equal(t, float64(aliceID), incoming["from_user_id"])
	require.Equal(t, "pending", incoming["status"])

	// bob 通过 → 204。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", reqID), "", bobToken)
	require.Equal(t, http.StatusNoContent, w.Code)

	// 双向好友列表互相可见。
	aFriends := friendsOf(t, env, aliceToken)
	require.Len(t, aFriends, 1)
	require.Equal(t, float64(bobID), aFriends[0].(map[string]any)["user_id"])
	bFriends := friendsOf(t, env, bobToken)
	require.Len(t, bFriends, 1)
	require.Equal(t, float64(aliceID), bFriends[0].(map[string]any)["user_id"])

	// 申请状态置为 accepted。
	w, list = doJSON(t, env, http.MethodGet, "/api/friend-requests?scope=outgoing", "", aliceToken)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "accepted", list["items"].([]any)[0].(map[string]any)["status"])
}

// ---- 拒绝后不建立关系 ----

func TestFriendRequestRejectNoFriendship(t *testing.T) {
	env := requireEnv(t)
	_, aliceToken := register(t, env, "ava_fr")
	bobID, bobToken := register(t, env, "bob_fr")

	req := sendRequest(t, env, aliceToken, bobID)
	reqID := int64(req["id"].(float64))

	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/reject", reqID), "", bobToken)
	require.Equal(t, http.StatusNoContent, w.Code)

	require.Empty(t, friendsOf(t, env, aliceToken))
	require.Empty(t, friendsOf(t, env, bobToken))

	// 申请状态为 rejected；拒绝后 alice 可重新申请（复用原行，id 不变）。
	w, list := doJSON(t, env, http.MethodGet, "/api/friend-requests?scope=outgoing", "", aliceToken)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "rejected", list["items"].([]any)[0].(map[string]any)["status"])

	w, body := doJSON(t, env, http.MethodPost, "/api/friend-requests",
		fmt.Sprintf(`{"to_user_id":%d}`, bobID), aliceToken)
	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, float64(reqID), body["id"], "被拒后重新申请应复用原行")
	require.Equal(t, "pending", body["status"])
}

// ---- 重复申请与自加被拒 ----

func TestFriendRequestDuplicateAndSelf(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_fd")
	bobID, _ := register(t, env, "bob_fd")

	// 自加好友 → 400。
	w, _ := doJSON(t, env, http.MethodPost, "/api/friend-requests",
		fmt.Sprintf(`{"to_user_id":%d}`, aliceID), aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)

	sendRequest(t, env, aliceToken, bobID)
	// 重复申请（pending 未处理）→ 409。
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests",
		fmt.Sprintf(`{"to_user_id":%d}`, bobID), aliceToken)
	require.Equal(t, http.StatusConflict, w.Code)

	// 目标用户不存在 → 404。
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests", `{"to_user_id":999999}`, aliceToken)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// ---- owner 校验（防 IDOR）----

func TestFriendRequestOwnerCheck(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_fo")
	bobID, bobToken := register(t, env, "bob_fo")
	_, carolToken := register(t, env, "carol_fo")

	req := sendRequest(t, env, aliceToken, bobID)
	reqID := int64(req["id"].(float64))

	// 申请人 / 无关第三人 处理申请 → 403。
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", reqID), "", aliceToken)
	require.Equal(t, http.StatusForbidden, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/reject", reqID), "", carolToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 不存在的申请 → 404。
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests/999999/accept", "", bobToken)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 拒绝后不能重复处理（非待处理）→ 409。
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/reject", reqID), "", bobToken)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", reqID), "", bobToken)
	require.Equal(t, http.StatusConflict, w.Code)

	// 已是好友后再申请 → 409。
	bobAccepts := func() {
		r := sendRequest(t, env, bobToken, aliceID)
		w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", int64(r["id"].(float64))), "", aliceToken)
		require.Equal(t, http.StatusNoContent, w.Code)
	}
	bobAccepts()
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests",
		fmt.Sprintf(`{"to_user_id":%d}`, aliceID), bobToken)
	require.Equal(t, http.StatusConflict, w.Code)
}

// ---- 鉴权与参数校验 ----

func TestFriendRequestAuthAndValidation(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_fv")

	// 未带 token → 401。
	w, _ := doJSON(t, env, http.MethodGet, "/api/friends", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests", `{"to_user_id":1}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 缺少 to_user_id / 非法 id → 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests", `{}`, aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodPost, "/api/friend-requests/abc/accept", "", aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 非法 scope / status 筛选 → 400。
	w, _ = doJSON(t, env, http.MethodGet, "/api/friend-requests?scope=sideways", "", aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)
	w, _ = doJSON(t, env, http.MethodGet, "/api/friend-requests?scope=incoming&status=weird", "", aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 未产生任何关系。
	require.Empty(t, friendsOf(t, env, aliceToken))
	var cnt int64
	require.NoError(t, env.gdb.Table("friend_requests").
		Where("from_user_id = ? OR to_user_id = ?", aliceID, aliceID).Count(&cnt).Error)
	require.Zero(t, cnt, "校验失败不应落库")
}
