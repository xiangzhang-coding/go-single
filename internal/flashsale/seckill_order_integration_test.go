// T12 秒杀异步落单集成测试（主 seam）：真实 MySQL + Redis + RabbitMQ +
// httptest 完整路由 + 常驻消费者，覆盖完整闭环——抢购（202 排队中 + order_no）
// → MQ 异步落单（消费者幂等建单 + 同事务扣活动库存）→ 订单可轮询查询；
// 并发不重复建单（唯一约束）、重复投递幂等、永久失败进死信。
// 需要 RabbitMQ 就绪（docker compose up -d），不可达时整体跳过。
package flashsale_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	userhandler "github.com/xiangzhang-coding/go-single/internal/user/handler"
	userrepo "github.com/xiangzhang-coding/go-single/internal/user/repository"
	usersvc "github.com/xiangzhang-coding/go-single/internal/user/service"
)

const (
	rabbitURL  = "amqp://guest:guest@127.0.0.1:5672/"
	pollRounds = 60 // 轮询上限（每 100ms 一次，共 6s）
)

// mqEnv 秒杀异步落单测试环境：真实 MQ 发布 + 常驻消费者 + 完整路由。
// 复用 integration_test.go 的 testEnv（MySQL/Redis/verifier/product 等）。
type mqEnv struct {
	router    http.Handler
	mqClient  mq.MQ
	orderSvc  ordersvc.Service
	reconcile flashsalesvc.Reconciliation
}

var (
	mqEnvOnce sync.Once
	mqEnvVal  *mqEnv
	mqEnvErr  error
)

// requireMQEnv 构建秒杀异步落单环境；MySQL/Redis/RabbitMQ 任一不可达整体跳过。
func requireMQEnv(t *testing.T) *mqEnv {
	t.Helper()
	mqEnvOnce.Do(func() { mqEnvVal, mqEnvErr = buildMQEnv() })
	if mqEnvErr != nil {
		t.Skipf("MySQL/Redis/RabbitMQ 不可达，跳过 T12 集成测试（先 docker compose up -d）：%v", mqEnvErr)
	}
	return mqEnvVal
}

func buildMQEnv() (*mqEnv, error) {
	env, err := buildEnv() // 复用 MySQL/Redis 环境（迁移 + 连接 + Redis 清理）
	if err != nil {
		return nil, err
	}

	mqClient, err := mq.NewRabbitMQ(rabbitURL)
	if err != nil {
		return nil, fmt.Errorf("RabbitMQ 连接失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mqClient.Ping(ctx); err != nil {
		mqClient.Close()
		return nil, err
	}
	// 清空主队列与死信队列：避免上次运行的遗留消息污染断言。
	if err := purgeQueue(ctx, flashsalesvc.SeckillOrderQueue); err != nil {
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
	userHandler := userhandler.New(userSvc, verifier)
	addressHandler := userhandler.NewAddress(userSvc, verifier)
	productHandler := env.productHandler

	// 秒杀服务：真实 MQ 发布 + 真实雪花订单号（与 main 装配一致）。
	orderNoGen, err := snowflake.New(3)
	if err != nil {
		return nil, err
	}
	flashsaleStore := flashsalerepo.Store{Activities: flashsalerepo.NewGORMActivity(gdb)}
	flashsaleSvc := flashsalesvc.New(flashsaleStore, productSvc, cacheClient,
		limiter.RedisCounterConfig{}, mqClient, orderNoGen, metrics.New().Business())
	flashsaleHandler := flashsalehandler.New(flashsaleSvc, verifier)

	cartSvc := cartsvc.New(cartrepo.Store{Items: cartrepo.NewGORMCartItem(gdb)}, productSvc)
	couponSvc := couponsvc.New(couponrepo.Store{Template: couponrepo.NewGORMCouponTemplate(gdb), UserCoupon: couponrepo.NewGORMUserCoupon(gdb)}, cacheClient, metrics.New().Business())
	orderStore := orderrepo.NewGORMOrder(gdb)
	orderSvc := ordersvc.New(orderrepo.Store{Orders: orderStore, Items: orderrepo.NewGORMOrderItem(gdb), Tx: orderStore},
		cacheClient, orderNoGen, productSvc, couponSvc, cartSvc, userSvc, flashsaleSvc, flashsaleSvc, metrics.New().Business())
	orderHandler := orderhandler.New(orderSvc, verifier)

	// T13 秒杀库存对账：有效订单数经 order 服务端口统计。
	reconcile := flashsalesvc.NewReconciliation(flashsaleStore, cacheClient, orderSvc)

	// 常驻消费者：订阅"抢购成功"队列异步落单；连接中断自动重连（at-least-once）。
	log := zap.NewNop()
	consumer := flashsalesvc.NewSeckillOrderConsumer(flashsaleStore.Activities, orderSvc, userSvc, log)
	go func() {
		for {
			if err := mqClient.Consume(context.Background(), flashsalesvc.SeckillOrderQueue, consumer.Handle); err != nil {
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

	return &mqEnv{router: r, mqClient: mqClient, orderSvc: orderSvc, reconcile: reconcile}, nil
}

// ---- 请求助手（复用 integration_test.go 的 doJSONOn）----

// purchaseOn 在 mqEnv 路由上抢购，返回响应与 body。
func purchaseOn(t *testing.T, e *mqEnv, activityID int64, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return doJSONOn(t, e.router, http.MethodPost, fmt.Sprintf("/api/flashsales/%d/purchase", activityID), "", token)
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

// countSeckillOrders 统计活动已落单数（(user_id, activity_id) 唯一约束下即有效订单数）。
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
func seedPublishedOnSale(t *testing.T, admin string, stock int) int64 {
	t.Helper()
	skuID := seedSKU(t, env, admin)
	var productID int64
	require.NoError(t, env.gdb.Table("skus").Select("product_id").Where("id = ?", skuID).Scan(&productID).Error)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/products/%d/publish", productID), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code, "商品应上架: %s", w.Body.String())

	id := createActivity(t, env, admin, skuID, stock, 1, -time.Minute, time.Hour)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	return id
}

// ---- 测试 ----

// 完整闭环：抢购（202 排队中 + order_no）→ 消费者异步落单 → 轮询订单可查询；
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
	orderNo, ok := body["order_no"].(string)
	require.True(t, ok && orderNo != "", "响应应携带 order_no 供前端轮询")

	// 轮询订单详情：异步落单完成 → 订单可查询。
	order := pollOrder(t, e, orderNo, token)
	require.NotNil(t, order, "异步落单应在轮询窗口内完成")
	require.Equal(t, orderNo, order["order_no"])
	require.Equal(t, "seckill", order["order_type"])
	require.Equal(t, "pending_payment", order["status"])
	require.Equal(t, float64(id), order["activity_id"])
	require.Equal(t, float64(9900), order["pay_amount"], "应付 = 秒杀价")
	require.Equal(t, float64(0), order["discount_amount"], "秒杀订单不使用券")

	items := order["items"].([]any)
	require.Len(t, items, 1)
	it := items[0].(map[string]any)
	require.Equal(t, float64(9900), it["price"], "订单项固化秒杀价")

	require.Equal(t, 9, mysqlStock(t, e, id), "落单应同事务扣减活动库存")
	require.Equal(t, 1, countSeckillOrders(t, e, id))
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

// 重复投递幂等：同一"抢购成功"消息发布两次 → 消费者只建一单（唯一约束 + 不重复扣减库存）。
func TestSeckillOrderRedeliveryIdempotent(t *testing.T) {
	requireEnv(t) // 初始化共享 env（adminToken/seed 依赖）
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	token, _ := registerWithAddress(t, e, uniqueName("dup_msg"))
	claims, err := env.verifier.Verify(context.Background(), token)
	require.NoError(t, err)

	body, _ := json.Marshal(flashsalesvc.SeckillSuccessMessage{
		OrderNo: fmt.Sprintf("%d", time.Now().UnixNano()%1e14), UserID: claims.UserID, ActivityID: id,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.mqClient.Publish(ctx, flashsalesvc.SeckillOrderQueue, body))
	require.NoError(t, e.mqClient.Publish(ctx, flashsalesvc.SeckillOrderQueue, body))

	waitSeckillOrders(t, e, id, 1, 5*time.Second)
	time.Sleep(500 * time.Millisecond) // 给第二条（重复）消息处理时间
	require.Equal(t, 1, countSeckillOrders(t, e, id), "重复投递不得重复建单")
	require.Equal(t, 9, mysqlStock(t, e, id), "重复投递不得重复扣减库存")
}

// 永久失败进死信：活动不存在的消息被消费者拒收 → 落入死信队列（对账兜底）。
func TestSeckillOrderDeadLetter(t *testing.T) {
	e := requireMQEnv(t)
	body, _ := json.Marshal(flashsalesvc.SeckillSuccessMessage{
		OrderNo: "dead999", UserID: 1, ActivityID: 999999,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, e.mqClient.Publish(ctx, flashsalesvc.SeckillOrderQueue, body))

	got := receiveFromDLQ(t, flashsalesvc.SeckillOrderQueue)
	require.NotNil(t, got, "永久失败消息应进死信队列")
	var msg flashsalesvc.SeckillSuccessMessage
	require.NoError(t, json.Unmarshal(got, &msg))
	require.Equal(t, int64(999999), msg.ActivityID)
}

// ---- 死信读取助手（测试直连 amqp 读死信队列）----

// purgeQueue 清空主队列与其死信队列的遗留消息（测试环境隔离，避免跨运行污染）。
func purgeQueue(ctx context.Context, queue string) error {
	conn, err := amqp.Dial(rabbitURL)
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
	conn, err := amqp.Dial(rabbitURL)
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
