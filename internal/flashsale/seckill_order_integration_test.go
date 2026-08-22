// T12 秒杀异步落单集成测试（主 seam）：真实 MySQL + Redis + RabbitMQ +
// httptest 完整路由 + 常驻消费者，覆盖完整闭环——抢购（202 排队中 + order_no）
// → MQ 异步落单（消费者幂等建单 + 同事务扣活动库存）→ 订单可轮询查询；
// 并发不重复建单（唯一约束）、重复投递幂等、永久失败进死信。
// 需要 RabbitMQ 就绪（docker compose up -d），不可达时本地跳过、CI 失败。
package flashsale_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	cartrepo "github.com/xiangzhang-coding/go-single/internal/cart/repository"
	cartsvc "github.com/xiangzhang-coding/go-single/internal/cart/service"
	couponrepo "github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	couponsvc "github.com/xiangzhang-coding/go-single/internal/coupon/service"
	flashsalehandler "github.com/xiangzhang-coding/go-single/internal/flashsale/handler"
	flashsalemodel "github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	flashsalerepo "github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
	orderhandler "github.com/xiangzhang-coding/go-single/internal/order/handler"
	orderrepo "github.com/xiangzhang-coding/go-single/internal/order/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/auth"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	"github.com/xiangzhang-coding/go-single/internal/platform/snowflake"
	"github.com/xiangzhang-coding/go-single/internal/testsupport"
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	pollRounds = 60 // 轮询上限（每 100ms 一次，共 6s）
)

func testRabbitURL() string {
	return envOr("GO_SINGLE_MQ_URL", "amqp://guest:guest@127.0.0.1:5672/")
}

func testSeckillQueue() string {
	runID := envOr("GO_SINGLE_MQ_RUN_ID", fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()))
	return fmt.Sprintf("%s.test.%s", flashsalesvc.SeckillOrderQueue, runID)
}

type isolatedQueueMQ struct {
	mq.MQ
	queue string
}

func (m isolatedQueueMQ) Publish(ctx context.Context, queue string, body []byte) error {
	if queue == flashsalesvc.SeckillOrderQueue {
		queue = m.queue
	}
	return m.MQ.Publish(ctx, queue, body)
}

func (m isolatedQueueMQ) Consume(ctx context.Context, queue string, handler mq.MessageHandler) error {
	if queue == flashsalesvc.SeckillOrderQueue {
		queue = m.queue
	} else if queue == flashsalesvc.SeckillOrderDeadLetterQueue {
		queue = m.queue + ".dlq"
	}
	return m.MQ.Consume(ctx, queue, handler)
}

// mqEnv 秒杀异步落单测试环境：真实 MQ 发布 + 常驻消费者 + 完整路由。
// 复用 integration_test.go 的 testEnv（MySQL/Redis/verifier/product 等）。
type mqEnv struct {
	router        http.Handler
	mqClient      mq.MQ
	queue         string
	orderSvc      ordersvc.Service
	flashsaleSvc  flashsalesvc.Service
	activities    flashsalerepo.ActivityRepository
	preDeductions flashsalerepo.PreDeductionRepository
	tx            flashsalerepo.TxRunner
	consumer      *flashsalesvc.SeckillOrderConsumer
	timeout       flashsalesvc.SeckillCancellation
	reconcile     flashsalesvc.Reconciliation
}

type testOrderCancellation struct {
	orders  ordersvc.Service
	seckill flashsalesvc.SeckillCancellation
}

func (c testOrderCancellation) Cancel(ctx context.Context, userID int64, orderNo string) error {
	err := c.orders.Cancel(ctx, userID, orderNo)
	if errors.Is(err, ordersvc.ErrSeckillCancellationRequired) {
		return c.seckill.Cancel(ctx, userID, orderNo)
	}
	return err
}

var (
	mqEnvOnce sync.Once
	mqEnvVal  *mqEnv
	mqEnvErr  error
)

// requireMQEnv 构建秒杀异步落单环境；任一依赖不可达时本地跳过、CI 失败。
func requireMQEnv(t *testing.T) *mqEnv {
	t.Helper()
	mqEnvOnce.Do(func() { mqEnvVal, mqEnvErr = buildMQEnv() })
	testsupport.RequireDependency(t, "MySQL/Redis/RabbitMQ", mqEnvErr)
	return mqEnvVal
}

func buildMQEnv() (*mqEnv, error) {
	env, err := buildEnv() // 复用 MySQL/Redis 环境（迁移 + 连接 + Redis 清理）
	if err != nil {
		return nil, err
	}

	rabbitMQ, err := mq.NewRabbitMQ(testRabbitURL())
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ 连接失败: %w", err)
	}
	queue := testSeckillQueue()
	mqClient := isolatedQueueMQ{MQ: rabbitMQ, queue: queue}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mqClient.Ping(ctx); err != nil {
		mqClient.Close()
		return nil, err
	}
	// 清空主队列与死信队列：避免上次运行的遗留消息污染断言。
	if err := purgeQueue(ctx, queue); err != nil {
		mqClient.Close()
		return nil, err
	}

	verifier := env.verifier
	gdb := env.gdb
	cacheClient := env.cacheClient
	productSvc := env.productSvc

	// userSvc 需要签发令牌（TokenIssuer）；env.verifier 仅校验，重新构造同密钥 JWT。
	jwt := auth.NewJWT(auth.JWTConfig{Secret: testSecret, TTL: 2 * time.Hour})
	userSvc := usersvc.New(userrepo.Store{Users: userrepo.NewGORM(gdb), Addresses: userrepo.NewGORMAddress(gdb)}, jwt)
	userHandler := userhandler.New(userSvc, verifier, testsupport.AllowAllAuthAttempts{})
	addressHandler := userhandler.NewAddress(userSvc, verifier)
	productHandler := env.productHandler

	// 秒杀服务：真实 MQ 发布 + 真实雪花订单号（与 main 装配一致）。
	orderNoGen, err := snowflake.New(3)
	if err != nil {
		return nil, err
	}
	flashsaleActivityStore := flashsalerepo.NewGORMActivity(gdb)
	flashsaleStore := flashsalerepo.Store{
		Activities:    flashsaleActivityStore,
		PreDeductions: flashsalerepo.NewGORMPreDeduction(gdb),
		Tx:            flashsaleActivityStore,
	}
	flashsaleSvc := flashsalesvc.New(flashsaleStore, productSvc, cacheClient,
		limiter.RedisCounterConfig{}, mqClient, orderNoGen, metrics.New().Business())
	flashsaleHandler := flashsalehandler.New(flashsaleSvc, verifier)

	cartSvc := cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(gdb)}, productSvc)
	couponSvc := couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(gdb), UserCoupon: couponrepo.NewGORMUserCoupon(gdb)}, cacheClient, metrics.New().Business())
	orderStore := orderrepo.NewGORMOrder(gdb)
	orderSvc := ordersvc.New(orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(gdb), Tx: orderStore},
		cacheClient, orderNoGen, productSvc, couponSvc, cartSvc, userSvc, metrics.New().Business())
	timeout := flashsalesvc.NewSeckillCancellation(
		flashsaleStore.Tx, orderSvc, flashsaleStore.Activities, flashsaleStore.PreDeductions,
		flashsaleSvc, metrics.New().Business())
	orderHandler := orderhandler.New(orderSvc, verifier, testOrderCancellation{orders: orderSvc, seckill: timeout})

	// T13 秒杀库存对账：有效订单数经 order 服务端口统计。
	reconcile := flashsalesvc.NewReconciliation(flashsaleStore, cacheClient, orderSvc)

	// 常驻消费者：订阅"抢购成功"队列异步落单；连接中断自动重连（at-least-once）。
	log := zap.NewNop()
	consumer := flashsalesvc.NewSeckillOrderConsumer(
		flashsaleStore.Activities, flashsaleStore.PreDeductions, cacheClient, flashsaleStore.Tx,
		orderSvc, metrics.New().Business(), log)
	go func() {
		for {
			if err := mqClient.Consume(context.Background(), flashsalesvc.SeckillOrderQueue, consumer.Handle); err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return
		}
	}()
	go func() {
		for {
			if err := mqClient.Consume(context.Background(), flashsalesvc.SeckillOrderDeadLetterQueue, consumer.HandleDeadLetter); err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return
		}
	}()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	userHandler.RegisterRoutes(api)
	addressHandler.RegisterRoutes(api)
	productHandler.RegisterRoutes(api)
	flashsaleHandler.RegisterRoutes(api, allowAll)
	orderHandler.RegisterRoutes(api)

	return &mqEnv{
		router: r, mqClient: mqClient, queue: queue,
		orderSvc: orderSvc, flashsaleSvc: flashsaleSvc,
		activities: flashsaleStore.Activities, tx: flashsaleStore.Tx,
		preDeductions: flashsaleStore.PreDeductions,
		consumer:      consumer, timeout: timeout, reconcile: reconcile,
	}, nil
}

// ---- 请求助手（复用 integration_test.go 的 doJSONOn）----

// purchaseOn 在 mqEnv 路由上抢购，返回响应与 body。
func purchaseOn(t *testing.T, e *mqEnv, activityID int64, token string, requestIDs ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	requestID := uniqueName("mq-purchase")
	if len(requestIDs) > 0 {
		requestID = requestIDs[0]
	}
	return doJSONOn(t, e.router, http.MethodPost, fmt.Sprintf("/api/flashsales/%d/purchase", activityID),
		fmt.Sprintf(`{"client_request_id":%q}`, requestID), token)
}

// registerWithAddress 注册用户并创建默认地址（秒杀落单固化的地址快照来源），
// 返回 (token, userID)。
func registerWithAddress(t *testing.T, e *mqEnv, name string) (string, int64) {
	t.Helper()
	w, body := doJSONOn(t, e.router, http.MethodPost, "/api/auth/register",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, name), "")
	require.Equal(t, http.StatusCreated, w.Code, "注册失败: %s", w.Body.String())
	userID := int64(body["id"].(float64))

	w, login := doJSONOn(t, e.router, http.MethodPost, "/api/auth/login",
		fmt.Sprintf(`{"username":%q,"password":"secret123"}`, name), "")
	require.Equal(t, http.StatusOK, w.Code)
	token := login["token"].(string)

	w, _ = doJSONOn(t, e.router, http.MethodPost, "/api/addresses",
		`{"receiver":"张三","phone":"13800138000","province":"广东省","city":"深圳市","district":"南山区","detail":"科技园 1 号"}`,
		token)
	require.Equal(t, http.StatusCreated, w.Code, "建地址失败: %s", w.Body.String())
	return token, userID
}

// pollOrder 轮询订单详情直至落单完成（status 非空）；超时返回 nil。
func pollOrder(t *testing.T, e *mqEnv, orderNo, token string) map[string]any {
	t.Helper()
	for i := 0; i < pollRounds; i++ {
		w, body := doJSONOn(t, e.router, http.MethodGet, "/api/orders/"+orderNo, "", token)
		if w.Code == http.StatusOK && body["status"] != nil {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func pollPurchase(t *testing.T, e *mqEnv, preDeductionID, token string) map[string]any {
	t.Helper()
	for i := 0; i < pollRounds; i++ {
		w, body := doJSONOn(t, e.router, http.MethodGet, "/api/flashsales/purchases/"+preDeductionID, "", token)
		if w.Code == http.StatusOK && (body["status"] == "ordered" || body["status"] == "rolled_back") {
			return body
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// countSeckillOrders 统计活动已落单数（含不同购买槽位和已取消历史订单）。
func countSeckillOrders(t *testing.T, e *mqEnv, activityID int64) int {
	t.Helper()
	var n int64
	require.NoError(t, env.gdb.Table("orders").Where("activity_id = ?", activityID).Count(&n).Error)
	return int(n)
}

// mysqlStock 读取活动 MySQL 库存（落单事实源）。
func mysqlStock(t *testing.T, e *mqEnv, activityID int64) int {
	t.Helper()
	var stock int
	require.NoError(t, env.gdb.Table("flashsale_activities").Select("stock").Where("id = ?", activityID).Scan(&stock).Error)
	return stock
}

// waitSeckillOrders 等待活动落单数达到 n（超时失败）。
func waitSeckillOrders(t *testing.T, e *mqEnv, activityID int64, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for countSeckillOrders(t, e, activityID) < n {
		require.False(t, time.Now().After(deadline),
			"%d 单未在超时内落库（当前 %d）", n, countSeckillOrders(t, e, activityID))
		time.Sleep(100 * time.Millisecond)
	}
}

// seedPublishedOnSale 创建并上架进行中活动（库存 stock），并确保底层商品上架：
// 秒杀落单快照经商品详情（GetDetail 仅上架可见，与普通下单同规则）。
// admin 操作走 env.router，落单走 mqEnv 路由——两者共享同一 gdb/Redis。
func seedPublishedOnSale(t *testing.T, admin string, stock int, limits ...int) int64 {
	t.Helper()
	skuID := seedSKU(t, env, admin)
	var productID int64
	require.NoError(t, env.gdb.Table("skus").Select("product_id").Where("id = ?", skuID).Scan(&productID).Error)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/publish", productID), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code, "商品应上架: %s", w.Body.String())

	limit := 1
	if len(limits) > 0 {
		limit = limits[0]
	}
	id := createActivity(t, env, admin, skuID, stock, limit, -time.Minute, time.Hour)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	return id
}

// ---- 测试 ----

// 完整闭环：抢购（202 + pre_deduction_id）→ 消费者异步落单 → 生命周期收敛 ordered；
// 订单类型/活动/秒杀价/地址快照正确，活动 MySQL 库存同步扣减。
func TestSeckillOrderFullLoop(t *testing.T) {
	requireEnv(t) // 初始化共享 env（adminToken/seed 依赖）
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	token, _ := registerWithAddress(t, e, uniqueName("buyer"))

	w, body := purchaseOn(t, e, id, token)
	require.Equal(t, http.StatusAccepted, w.Code, "预扣成功应 202 排队中: %s", w.Body.String())
	require.Equal(t, "queued", body["status"])
	preDeductionID, ok := body["pre_deduction_id"].(string)
	require.True(t, ok && preDeductionID != "", "响应应携带 pre_deduction_id 供前端轮询")

	purchase := pollPurchase(t, e, preDeductionID, token)
	require.NotNil(t, purchase, "预扣生命周期应在轮询窗口内进入终态")
	require.Equal(t, "ordered", purchase["status"])
	require.NotEmpty(t, purchase["ordered_at"])
	require.NotContains(t, purchase, "rolled_back_at")
	orderNo, ok := purchase["order_no"].(string)
	require.True(t, ok && orderNo != "")
	w, order := doJSONOn(t, e.router, http.MethodGet, "/api/orders/"+orderNo, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, orderNo, order["order_no"])
	require.Equal(t, "seckill", order["order_type"])
	require.Equal(t, "pending_payment", order["status"])
	require.Equal(t, float64(id), order["activity_id"])
	require.Equal(t, preDeductionID, order["purchase_slot"])
	require.Equal(t, float64(9900), order["pay_amount"], "应付 = 秒杀价")
	require.Equal(t, float64(0), order["discount_amount"], "秒杀订单不使用券")

	items := order["items"].([]any)
	require.Len(t, items, 1)
	it := items[0].(map[string]any)
	require.Equal(t, float64(9900), it["price"], "订单项固化秒杀价")

	require.Equal(t, 9, mysqlStock(t, e, id), "落单应同事务扣减活动库存")
	require.Equal(t, 1, countSeckillOrders(t, e, id))
}

func TestSeckillOrderKeepsAcceptedSnapshotAfterProductUnpublish(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	var acceptedSKUID int64
	require.NoError(t, env.gdb.Table("flashsale_activities").Select("sku_id").Where("id = ?", id).Scan(&acceptedSKUID).Error)
	var productID int64
	require.NoError(t, env.gdb.Table("skus").Select("product_id").Where("id = ?", acceptedSKUID).Scan(&productID).Error)
	token, _ := registerWithAddress(t, e, uniqueName("snapshot"))

	w, response := purchase(t, env.router, id, token, "snapshot-request")
	require.Equal(t, http.StatusAccepted, w.Code)
	env.publisher.mu.Lock()
	acceptedMessage := append([]byte(nil), env.publisher.body...)
	env.publisher.mu.Unlock()
	require.NotEmpty(t, acceptedMessage)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/unpublish", productID), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)

	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/unpublish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	newSKUID := seedSKU(t, env, admin)
	w, _ = doJSON(t, env, http.MethodPut, fmt.Sprintf("/api/admin/flashsales/%d", id),
		activityBody(newSKUID, "换绑后的活动", 7700, 10, 1, time.Hour, 2*time.Hour), admin)
	require.Equal(t, http.StatusConflict, w.Code, "accepted reservation must settle before mutable activity fields change")

	require.NoError(t, e.consumer.Handle(context.Background(), acceptedMessage))
	order := pollOrder(t, e, response["order_no"].(string), token)
	require.NotNil(t, order)
	item := order["items"].([]any)[0].(map[string]any)
	require.Equal(t, float64(acceptedSKUID), item["sku_id"])
	require.Equal(t, float64(9900), item["price"])
	purchaseSlot, err := strconv.ParseInt(response["pre_deduction_id"].(string), 10, 64)
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(purchaseSlot, 10), order["purchase_slot"])
}

func TestSeckillOrderTransactionRollsBackWhenActivityStockDeductionFails(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 1)
	_, userID := registerWithAddress(t, e, uniqueName("rbcreate"))
	require.NoError(t, env.gdb.Exec(
		"UPDATE flashsale_activities SET stock = 0 WHERE id = ?", id,
	).Error)
	activity, err := e.activities.GetByID(context.Background(), id)
	require.NoError(t, err)
	orderNo := fmt.Sprintf("%d", time.Now().UnixNano())
	pd := &flashsalemodel.PreDeduction{
		UserID: userID, ActivityID: id, ClientRequestID: uniqueName("rollback-create"),
		OrderNo: &orderNo, SKUID: activity.SKUID, Price: activity.Price, Quantity: 1,
		Status: flashsalemodel.PreDeductionStatusPendingOrder,
	}
	require.NoError(t, e.preDeductions.Create(context.Background(), pd))
	body, err := json.Marshal(flashsalesvc.SeckillSuccessMessage{
		PreDeductionID: pd.ID, OrderNo: orderNo, UserID: userID, ActivityID: id,
		SKUID: pd.SKUID, Price: pd.Price, Quantity: pd.Quantity, PurchaseSlot: pd.PurchaseSlot,
	})
	require.NoError(t, err)

	err = e.consumer.Handle(context.Background(), body)

	require.ErrorIs(t, err, mq.ErrPermanent)
	var orders, items int64
	require.NoError(t, env.gdb.Table("orders").Where("order_no = ?", orderNo).Count(&orders).Error)
	require.NoError(t, env.gdb.Table("order_items").Where("order_no = ?", orderNo).Count(&items).Error)
	require.Zero(t, orders, "活动库存扣减失败应回滚订单")
	require.Zero(t, items, "活动库存扣减失败应回滚订单项")
	require.Equal(t, 0, mysqlStock(t, e, id))
}

// 并发抢购不重复建单：30 用户抢 20 库存 → 恰好 20 单（唯一约束挡重复），库存归零。
func TestSeckillOrderConcurrentNoDuplicate(t *testing.T) {
	requireEnv(t) // 初始化共享 env（adminToken/seed 依赖）
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 20)

	const users = 30
	orderNos := make([]string, users)
	var wg sync.WaitGroup
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token, _ := registerWithAddress(t, e, uniqueName(fmt.Sprintf("racer%d", i)))
			w, body := purchaseOn(t, e, id, token)
			if w.Code == http.StatusAccepted {
				orderNos[i] = body["order_no"].(string)
			}
		}(i)
	}
	wg.Wait()

	waitSeckillOrders(t, e, id, 20, 15*time.Second)
	require.Equal(t, 20, countSeckillOrders(t, e, id), "并发不得重复建单")
	require.Equal(t, 0, mysqlStock(t, e, id), "落单扣减与订单数一致")

	// 订单号互不相同（雪花唯一）；恰 20 个预扣成功者均返回了订单号。
	seen := map[string]bool{}
	for _, no := range orderNos {
		if no == "" {
			continue // 预扣失败者（抢光/限购）无订单号
		}
		require.False(t, seen[no], "订单号不得重复")
		seen[no] = true
	}
	require.Len(t, seen, 20, "恰 20 个预扣成功")
}

// 两个合法槽位分别成单；每个槽位的重复投递只命中自己的订单。
func TestSeckillOrderRedeliveryIsPurchaseSlotScoped(t *testing.T) {
	requireEnv(t) // 初始化共享 env（adminToken/seed 依赖）
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10, 2)
	token, userID := registerWithAddress(t, e, uniqueName("dup_msg"))
	w, first := purchaseOn(t, e, id, token, "mq-slot-1")
	require.Equal(t, http.StatusAccepted, w.Code)
	w, second := purchaseOn(t, e, id, token, "mq-slot-2")
	require.Equal(t, http.StatusAccepted, w.Code)
	require.NotNil(t, pollOrder(t, e, first["order_no"].(string), token))
	require.NotNil(t, pollOrder(t, e, second["order_no"].(string), token))

	firstID, err := strconv.ParseInt(first["pre_deduction_id"].(string), 10, 64)
	require.NoError(t, err)
	secondID, err := strconv.ParseInt(second["pre_deduction_id"].(string), 10, 64)
	require.NoError(t, err)
	firstPD, err := e.preDeductions.GetByID(context.Background(), firstID)
	require.NoError(t, err)
	secondPD, err := e.preDeductions.GetByID(context.Background(), secondID)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, pd := range []*flashsalemodel.PreDeduction{firstPD, secondPD} {
		body, marshalErr := json.Marshal(flashsalesvc.SeckillSuccessMessage{
			PreDeductionID: pd.ID, OrderNo: pd.OrderNumber(), UserID: userID, ActivityID: id,
			SKUID: pd.SKUID, Price: pd.Price, Quantity: pd.Quantity, PurchaseSlot: pd.PurchaseSlot,
		})
		require.NoError(t, marshalErr)
		require.NoError(t, e.mqClient.Publish(ctx, flashsalesvc.SeckillOrderQueue, body))
		require.NoError(t, e.mqClient.Publish(ctx, flashsalesvc.SeckillOrderQueue, body))
	}

	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 2, countSeckillOrders(t, e, id), "两个槽位各一单，重复投递不增单")
	require.Equal(t, 8, mysqlStock(t, e, id), "每个槽位只扣减一次库存")
}

// 永久失败先持久化回退意图，消息进入 DLQ 后被自动消费，恢复任务完整回退。
func TestSeckillOrderDeadLetterTriggersAutomaticRollback(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	token := registerAndToken(t, env, uniqueName("no_address"))
	claims, err := env.verifier.Verify(context.Background(), token)
	require.NoError(t, err)

	w, body := purchaseOn(t, e, id, token)
	require.Equal(t, http.StatusAccepted, w.Code)
	pdID := body["pre_deduction_id"].(string)

	require.Eventually(t, func() bool {
		var status string
		err := env.gdb.Table("flashsale_pre_deductions").Select("status").Where("id = ?", pdID).Scan(&status).Error
		return err == nil && status == "pending_rollback"
	}, 5*time.Second, 100*time.Millisecond)

	pdIDInt, err := strconv.ParseInt(pdID, 10, 64)
	require.NoError(t, err)
	require.NoError(t, e.flashsaleSvc.RecoverPreDeduction(context.Background(), pdIDInt))
	var status string
	require.NoError(t, env.gdb.Table("flashsale_pre_deductions").Select("status").Where("id = ?", pdID).Scan(&status).Error)
	require.Equal(t, "rolled_back", status)
	w, rolledBack := doJSONOn(t, e.router, http.MethodGet, "/api/flashsales/purchases/"+pdID, "", token)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, rolledBack["rolled_back_at"])
	require.NotContains(t, rolledBack, "ordered_at")
	require.Equal(t, "10", *redisGet(t, fmt.Sprintf("flashsale:stock:%d", id)))
	require.Nil(t, redisGet(t, fmt.Sprintf("flashsale:idem:%d:%d", id, claims.UserID)))
}

// ---- 死信读取助手（测试直连 amqp 读死信队列）----

// purgeQueue 清空主队列与其死信队列的遗留消息（测试环境隔离，避免跨运行污染）。
func purgeQueue(ctx context.Context, queue string) error {
	conn, err := amqp.Dial(testRabbitURL())
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	// 与 platform/mq declareQueue 保持同参声明（幂等；参数不一致会 406）。
	// 注意：死信队列为普通队列（无 DLX 参数），主队列带死信配置。
	if _, err := ch.QueueDeclare(queue+".dlq", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": queue + ".dlq",
	}); err != nil {
		return err
	}
	for _, q := range []string{queue, queue + ".dlq"} {
		for {
			msg, ok, err := ch.Get(q, false)
			if err != nil {
				break // no more messages（channel 级错误终止）
			}
			if !ok {
				break
			}
			msg.Ack(false)
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}
	return nil
}

func receiveFromDLQ(t *testing.T, queue string) []byte {
	t.Helper()
	conn, err := amqp.Dial(testRabbitURL())
	require.NoError(t, err)
	defer conn.Close()
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer ch.Close()
	dlq := queue + ".dlq"
	if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
		t.Fatalf("声明死信队列 %s: %v", dlq, err)
	}
	msgs, err := ch.Consume(dlq, "", true, false, false, false, nil)
	require.NoError(t, err)
	select {
	case d := <-msgs:
		return d.Body
	case <-time.After(5 * time.Second):
		return nil
	}
}
