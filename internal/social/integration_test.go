// 好友关系与好友圈集成测试（主 seam）：真实 MySQL + httptest 起完整路由，
// 覆盖 申请→通过→好友（双向列表）、拒绝不建关系、重复申请/自加被拒、
// owner 校验（仅被申请人可处理申请）、鉴权与参数校验；
// 好友圈：分享购买校验（未购买 403）、仅好友可见（非好友不可见）、
// 时间线分页、删除自己的动态（他人 403）。
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

	cartmodel "github.com/xiangzhang-coding/go-single/internal/cart/model"
	couponmodel "github.com/xiangzhang-coding/go-single/internal/coupon/model"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	orderrepo "github.com/xiangzhang-coding/go-single/internal/order/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	productmodel "github.com/xiangzhang-coding/go-single/internal/product/model"
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

	// order 服务：好友圈分享的购买校验端口（HasPurchasedSKU）走真实订单仓储；
	// 其余跨模块依赖（下单/支付路径本包不触达）用替身，避免拉起 Redis 与全部模块。
	orderStore := orderrepo.NewGORMOrder(gdb)
	orderSvc := ordersvc.New(
		orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(gdb), Tx: orderStore},
		noopCache{}, stubOrderNoGen{}, stubProducts{}, stubCoupons{}, stubCart{}, userSvc, stubActivity{}, stubActivity{},
		metrics.New().Business())

	socialStore := socialrepo.Store{
		Requests:    socialrepo.NewGORMRequest(gdb),
		Friendships: socialrepo.NewGORMFriendship(gdb),
		Posts:       socialrepo.NewGORMPost(gdb),
	}
	socialSvc := socialsvc.New(socialStore, userSvc)
	postSvc := socialsvc.NewPosts(socialStore, userSvc, orderSvc)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userhandler.New(userSvc, verifier).RegisterRoutes(api)
	socialhandler.New(socialSvc, postSvc, verifier).RegisterRoutes(api)
	return &testEnv{router: r, verifier: verifier, gdb: gdb}, nil
}

// ---- order 服务跨模块替身（本包只触达 HasPurchasedSKU） ----

type noopCache struct{}

func (noopCache) Ping(context.Context) error                                    { return nil }
func (noopCache) Close() error                                                  { return nil }
func (noopCache) Get(context.Context, string) (string, error)                   { return "", cache.ErrMiss }
func (noopCache) Set(context.Context, string, string, time.Duration) error      { return nil }
func (noopCache) Del(context.Context, string) error                             { return nil }
func (noopCache) Eval(context.Context, string, []string, ...any) (int64, error) { return 0, nil }

type stubOrderNoGen struct{}

func (stubOrderNoGen) Next() (int64, error) { return 0, nil }

type stubProducts struct{}

func (stubProducts) GetSKU(context.Context, int64) (*productmodel.SKU, error) { return nil, nil }
func (stubProducts) GetSKUForUpdate(context.Context, *gorm.DB, int64) (*productmodel.SKU, error) {
	return nil, nil
}
func (stubProducts) GetDetail(context.Context, int64) (*productmodel.ProductDetail, error) {
	return nil, nil
}
func (stubProducts) DeductStock(context.Context, *gorm.DB, int64, int) (bool, error) {
	return true, nil
}
func (stubProducts) RestoreStock(context.Context, *gorm.DB, int64, int) error { return nil }

type stubCoupons struct{}

func (stubCoupons) GetUsable(context.Context, int64, int64) (*couponmodel.UserCouponView, error) {
	return nil, nil
}
func (stubCoupons) UseCoupon(context.Context, *gorm.DB, int64, int64) error      { return nil }
func (stubCoupons) RollbackCoupon(context.Context, *gorm.DB, int64, int64) error { return nil }

type stubCart struct{}

func (stubCart) LockItems(context.Context, *gorm.DB, int64) ([]cartmodel.CartItem, error) {
	return nil, nil
}
func (stubCart) DeletePurchased(context.Context, *gorm.DB, int64, []int64) error { return nil }

type stubActivity struct{}

func (stubActivity) DeductStock(context.Context, *gorm.DB, int64, int) (bool, error) {
	return true, nil
}

func (stubActivity) RestoreStock(context.Context, *gorm.DB, int64, int) error { return nil }
func (stubActivity) RestoreRedis(context.Context, int64, int64, int) error    { return nil }

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

// ---- 好友圈：种子数据助手 ----

// seedProductSKU 直接落库一个商品与 SKU（好友圈测试不走完整下单闭环，
// 已购状态由 seedPurchase 直插订单模拟），返回 (productID, skuID)。
func seedProductSKU(t *testing.T, env *testEnv, prefix string) (int64, int64) {
	t.Helper()
	catName := fmt.Sprintf("cat_%s_%d", prefix, time.Now().UnixNano())
	require.NoError(t, env.gdb.Exec("INSERT INTO categories (name) VALUES (?)", catName).Error)
	var catID int64
	require.NoError(t, env.gdb.Table("categories").Select("id").Where("name = ?", catName).Scan(&catID).Error)

	require.NoError(t, env.gdb.Exec(
		"INSERT INTO products (category_id, title, status) VALUES (?, ?, 'on_sale')", catID, "测试商品").Error)
	var productID int64
	require.NoError(t, env.gdb.Table("products").Select("id").Where("category_id = ?", catID).Scan(&productID).Error)

	require.NoError(t, env.gdb.Exec(
		"INSERT INTO skus (product_id, specs, price, stock) VALUES (?, '{}', 1000, 100)", productID).Error)
	var skuID int64
	require.NoError(t, env.gdb.Table("skus").Select("id").Where("product_id = ?", productID).Scan(&skuID).Error)
	return productID, skuID
}

// seedPurchase 直插一条指定状态的订单与订单项，模拟该用户的已购/未购历史。
// 时间经 Go 时钟写入（与全项目时钟约定一致，不依赖 MySQL NOW()）。
func seedPurchase(t *testing.T, env *testEnv, userID, productID, skuID int64, status string) {
	t.Helper()
	orderNo := fmt.Sprintf("%d", time.Now().UnixNano())
	expireAt := time.Now().Add(15 * time.Minute)
	require.NoError(t, env.gdb.Exec(`
		INSERT INTO orders (order_no, user_id, order_type, status, total_amount, discount_amount, pay_amount,
			receiver, phone, province, city, district, detail, expire_at)
		VALUES (?, ?, 'normal', ?, 1000, 0, 1000, '张三', '13800000000', '省', '市', '区', '地址', ?)`,
		orderNo, userID, status, expireAt).Error)
	require.NoError(t, env.gdb.Exec(`
		INSERT INTO order_items (order_no, sku_id, product_id, title, specs, price, quantity, subtotal)
		VALUES (?, ?, ?, '测试商品', '{}', 1000, 1, 1000)`,
		orderNo, skuID, productID).Error)
}

// befriend alice 发起申请 → bob 通过，返回好友关系。
func befriend(t *testing.T, env *testEnv, aliceToken string, bobID int64, bobToken string) {
	t.Helper()
	req := sendRequest(t, env, aliceToken, bobID)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/friend-requests/%d/accept", int64(req["id"].(float64))), "", bobToken)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func sharePost(t *testing.T, env *testEnv, token string, skuID int64, content string) map[string]any {
	t.Helper()
	w, body := doJSON(t, env, http.MethodPost, "/api/posts",
		fmt.Sprintf(`{"sku_id":%d,"content":%q}`, skuID, content), token)
	require.Equal(t, http.StatusCreated, w.Code, "分享失败: %s", w.Body.String())
	return body
}

func feedOf(t *testing.T, env *testEnv, token, query string) ([]any, int64) {
	t.Helper()
	w, body := doJSON(t, env, http.MethodGet, "/api/posts/feed"+query, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	items, _ := body["items"].([]any)
	total, _ := body["total"].(float64)
	return items, int64(total)
}

// ---- 分享：购买校验（未购买不可分享） ----

func TestPostSharePurchaseValidation(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_ps")
	productID, skuID := seedProductSKU(t, env, "ps")

	// 未购买 → 403；待支付/已取消不算已购 → 403。
	w, _ := doJSON(t, env, http.MethodPost, "/api/posts",
		fmt.Sprintf(`{"sku_id":%d,"content":"种草"}`, skuID), aliceToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	seedPurchase(t, env, aliceID, productID, skuID, ordermodel.OrderStatusPendingPayment)
	w, _ = doJSON(t, env, http.MethodPost, "/api/posts",
		fmt.Sprintf(`{"sku_id":%d}`, skuID), aliceToken)
	require.Equal(t, http.StatusForbidden, w.Code, "待支付订单不算已购")

	// 已支付 → 201。
	seedPurchase(t, env, aliceID, productID, skuID, ordermodel.OrderStatusPaid)
	w, body := doJSON(t, env, http.MethodPost, "/api/posts",
		fmt.Sprintf(`{"sku_id":%d,"content":"好好用","image_url":"https://minio.example/x.png"}`, skuID), aliceToken)
	require.Equal(t, http.StatusCreated, w.Code, "已购分享应成功: %s", w.Body.String())
	require.Equal(t, float64(skuID), body["sku_id"])
	require.Equal(t, "好好用", body["content"])

	// 参数校验：缺 sku_id → 400。
	w, _ = doJSON(t, env, http.MethodPost, "/api/posts", `{"content":"x"}`, aliceToken)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 未带 token → 401。
	w, _ = doJSON(t, env, http.MethodPost, "/api/posts", `{"sku_id":1}`, "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---- 时间线：仅好友可见 ----

func TestPostFeedFriendsOnly(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_pf")
	bobID, bobToken := register(t, env, "bob_pf")
	carolID, carolToken := register(t, env, "carol_pf")

	// alice ↔ bob 好友；carol 与两人均非好友。
	befriend(t, env, aliceToken, bobID, bobToken)

	bobProductID, bobSKU := seedProductSKU(t, env, "pf")
	carolProductID, carolSKU := seedProductSKU(t, env, "pf")
	seedPurchase(t, env, bobID, bobProductID, bobSKU, ordermodel.OrderStatusPaid)
	seedPurchase(t, env, carolID, carolProductID, carolSKU, ordermodel.OrderStatusPaid)
	seedPurchase(t, env, aliceID, bobProductID, bobSKU, ordermodel.OrderStatusPaid)

	sharePost(t, env, bobToken, bobSKU, "bob 的分享")
	sharePost(t, env, carolToken, carolSKU, "carol 的分享")
	sharePost(t, env, aliceToken, bobSKU, "alice 自己的")

	// alice 的时间线只含好友 bob 的动态，不含自己与非好友 carol 的。
	items, total := feedOf(t, env, aliceToken, "")
	require.Equal(t, int64(1), total)
	require.Len(t, items, 1)
	post := items[0].(map[string]any)
	require.Equal(t, float64(bobID), post["user_id"])
	require.Equal(t, "bob 的分享", post["content"])
	require.True(t, strings.HasPrefix(post["author_username"].(string), "bob_"), "作者用户名经跨模块补齐")

	// carol（非好友）看不到任何人的动态。
	carolItems, carolTotal := feedOf(t, env, carolToken, "")
	require.Zero(t, carolTotal, "非好友动态不可见")
	require.Empty(t, carolItems)

	// bob 的时间线只有好友 alice 的动态，看不到非好友 carol 的。
	bobItems, bobTotal := feedOf(t, env, bobToken, "")
	require.Equal(t, int64(1), bobTotal)
	require.Len(t, bobItems, 1)
	require.Equal(t, float64(aliceID), bobItems[0].(map[string]any)["user_id"])
}

// ---- 时间线：分页与删除 ----

func TestPostFeedPagination(t *testing.T) {
	env := requireEnv(t)
	_, aliceToken := register(t, env, "ava_pg")
	bobID, bobToken := register(t, env, "bob_pg")
	befriend(t, env, aliceToken, bobID, bobToken)

	productID, skuID := seedProductSKU(t, env, "pg")
	seedPurchase(t, env, bobID, productID, skuID, ordermodel.OrderStatusShipped)
	for i := 0; i < 5; i++ {
		sharePost(t, env, bobToken, skuID, fmt.Sprintf("第 %d 条", i))
	}

	// 第 1 页 2 条（最新在前），total 为全部 5 条。
	page1, total := feedOf(t, env, aliceToken, "?page=1&page_size=2")
	require.Equal(t, int64(5), total)
	require.Len(t, page1, 2)
	require.Equal(t, "第 4 条", page1[0].(map[string]any)["content"], "最新在前")

	// 第 2 页 2 条、第 3 页 1 条，各页无重复。
	page2, _ := feedOf(t, env, aliceToken, "?page=2&page_size=2")
	require.Len(t, page2, 2)
	page3, _ := feedOf(t, env, aliceToken, "?page=3&page_size=2")
	require.Len(t, page3, 1)
	require.Equal(t, "第 0 条", page3[0].(map[string]any)["content"], "第 3 页剩最早一条")

	// 越界页：空列表，total 不变。
	page4, total4 := feedOf(t, env, aliceToken, "?page=99&page_size=2")
	require.Empty(t, page4)
	require.Equal(t, int64(5), total4)
}

func TestPostDeleteOwn(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_pd")
	bobID, bobToken := register(t, env, "bob_pd")
	befriend(t, env, aliceToken, bobID, bobToken)

	productID, skuID := seedProductSKU(t, env, "pd")
	seedPurchase(t, env, aliceID, productID, skuID, ordermodel.OrderStatusCompleted)
	post := sharePost(t, env, aliceToken, skuID, "alice 的")
	postID := int64(post["id"].(float64))

	// 他人（bob）删除 → 403。
	w, _ := doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/posts/%d", postID), "", bobToken)
	require.Equal(t, http.StatusForbidden, w.Code)

	// 删除不存在 → 404。
	w, _ = doJSON(t, env, http.MethodDelete, "/api/posts/999999", "", aliceToken)
	require.Equal(t, http.StatusNotFound, w.Code)

	// 本人删除 → 204；时间线中消失。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/posts/%d", postID), "", aliceToken)
	require.Equal(t, http.StatusNoContent, w.Code)
	items, total := feedOf(t, env, bobToken, "")
	require.Zero(t, total, "删除后时间线不再包含该动态")
	require.Empty(t, items)

	// 未带 token → 401。
	w, _ = doJSON(t, env, http.MethodDelete, fmt.Sprintf("/api/posts/%d", postID), "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---- 我的动态（T24 前端个人页）----

func TestPostMine(t *testing.T) {
	env := requireEnv(t)
	aliceID, aliceToken := register(t, env, "ava_pm")
	bobID, bobToken := register(t, env, "bob_pm")
	befriend(t, env, aliceToken, bobID, bobToken)

	productID, skuID := seedProductSKU(t, env, "pm")
	seedPurchase(t, env, aliceID, productID, skuID, ordermodel.OrderStatusPaid)
	seedPurchase(t, env, bobID, productID, skuID, ordermodel.OrderStatusPaid)
	sharePost(t, env, aliceToken, skuID, "alice 第一条")
	sharePost(t, env, aliceToken, skuID, "alice 第二条")
	sharePost(t, env, bobToken, skuID, "bob 的") // 他人动态不混入

	// 我的动态：仅自己的两条，最新在前。
	w, body := doJSON(t, env, http.MethodGet, "/api/posts/mine?page=1&page_size=10", "", aliceToken)
	require.Equal(t, http.StatusOK, w.Code)
	items := body["items"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, float64(2), body["total"])
	require.Equal(t, "alice 第二条", items[0].(map[string]any)["content"], "最新在前")
	require.Equal(t, float64(aliceID), items[0].(map[string]any)["user_id"])
	require.True(t, strings.HasPrefix(items[0].(map[string]any)["author_username"].(string), "ava_pm_"))

	// 分页：page_size=1 时第一页 1 条，total 仍为 2。
	w, body = doJSON(t, env, http.MethodGet, "/api/posts/mine?page=2&page_size=1", "", aliceToken)
	require.Equal(t, http.StatusOK, w.Code)
	items = body["items"].([]any)
	require.Len(t, items, 1)
	require.Equal(t, "alice 第一条", items[0].(map[string]any)["content"], "第二页为最早一条")

	// bob 的 mine 不含 alice 的动态。
	_, body = doJSON(t, env, http.MethodGet, "/api/posts/mine", "", bobToken)
	require.Equal(t, float64(1), body["total"])

	// 未带 token → 401。
	w, _ = doJSON(t, env, http.MethodGet, "/api/posts/mine", "", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
