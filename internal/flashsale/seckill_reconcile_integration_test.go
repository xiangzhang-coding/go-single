// T13 秒杀超时回补与对账集成测试（主 seam）：真实 MySQL + Redis + RabbitMQ +
// httptest 完整路由 + 常驻消费者，覆盖——
//   - 超时取消闭环：订单取消 → MySQL/Redis 库存回补 + 用户计数回补 + 幂等键释放
//     → 同一用户可再次抢购（新订单，取消订单不占 (user, activity) 去重位）；
//   - 进行中对账：Redis 有扣减无订单 → 补单信号告警，Redis 不被写回；
//   - 收尾对账：刚结束活动以 MySQL 为准对齐 Redis。
//
// 需要 RabbitMQ 就绪（docker compose up -d），不可达时本地跳过、CI 失败。
package flashsale_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	flashsalerepo "github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"

	flashsalesvc "github.com/xiangzhang-coding/go-single/internal/flashsale/service"
)

type failingRestoreActivities struct {
	flashsalerepo.ActivityRepository
}

func (failingRestoreActivities) RestoreStock(context.Context, *transaction.Handle, int64, int) error {
	return errors.New("restore activity stock")
}

// redisGet 读取 Redis key 值（nil = key 不存在）。
func redisGet(t *testing.T, key string) *string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, err := env.redis.Get(ctx, key).Result()
	if err != nil {
		return nil
	}
	return &v
}

// orderStatus 读取订单状态。
func orderStatus(t *testing.T, orderNo string) string {
	t.Helper()
	var s string
	require.NoError(t, env.gdb.Table("orders").Select("status").Where("order_no = ?", orderNo).Scan(&s).Error)
	return s
}

// 取消只释放对应购买槽位；同一用户的另一待支付槽位保持有效，并可补购新槽位。
func TestSeckillTimeoutCancelReleasesOnlyItsSlotAndAllowsReplacement(t *testing.T) {
	requireEnv(t) // 初始化共享 env（adminToken/seed 依赖）
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10, 2)
	token, userID := registerWithAddress(t, e, uniqueName("repurchase"))
	stockKey, countKey := fmt.Sprintf("flashsale:stock:%d", id), fmt.Sprintf("flashsale:count:%d:%d", id, userID)

	w, first := purchaseOn(t, e, id, token, "cancel-slot-1")
	require.Equal(t, http.StatusAccepted, w.Code)
	w, second := purchaseOn(t, e, id, token, "cancel-slot-2")
	require.Equal(t, http.StatusAccepted, w.Code)
	firstOrderNo := first["order_no"].(string)
	secondOrderNo := second["order_no"].(string)
	require.NotNil(t, pollOrder(t, e, firstOrderNo, token))
	require.NotNil(t, pollOrder(t, e, secondOrderNo, token))
	firstSlot := first["pre_deduction_id"].(string)
	secondSlot := second["pre_deduction_id"].(string)
	require.Equal(t, "8", *redisGet(t, stockKey))
	require.Equal(t, "2", *redisGet(t, countKey))
	require.NotNil(t, redisGet(t, fmt.Sprintf("flashsale:idem:%d:%d:%s", id, userID, firstSlot)))
	require.NotNil(t, redisGet(t, fmt.Sprintf("flashsale:idem:%d:%d:%s", id, userID, secondSlot)))
	require.Equal(t, 8, mysqlStock(t, e, id))

	require.NoError(t, env.gdb.Exec("UPDATE orders SET expire_at = ? WHERE order_no = ?",
		time.Now().Add(-time.Minute), firstOrderNo).Error)
	cancelled, _, _, err := e.timeout.CancelExpired(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, cancelled, 1)
	require.Equal(t, ordermodel.OrderStatusCancelled, orderStatus(t, firstOrderNo))
	require.Equal(t, ordermodel.OrderStatusPendingPayment, orderStatus(t, secondOrderNo))
	require.Equal(t, 9, mysqlStock(t, e, id))
	require.Equal(t, "9", *redisGet(t, stockKey))
	require.Equal(t, "1", *redisGet(t, countKey))
	require.Nil(t, redisGet(t, fmt.Sprintf("flashsale:idem:%d:%d:%s", id, userID, firstSlot)))
	require.NotNil(t, redisGet(t, fmt.Sprintf("flashsale:idem:%d:%d:%s", id, userID, secondSlot)))

	w, replacement := purchaseOn(t, e, id, token, "replacement-slot")
	require.Equal(t, http.StatusAccepted, w.Code, "released slot should be purchasable again: %s", w.Body.String())
	require.NotEqual(t, firstSlot, replacement["pre_deduction_id"])
	require.NotNil(t, pollOrder(t, e, replacement["order_no"].(string), token))
	require.Equal(t, ordermodel.OrderStatusPendingPayment, orderStatus(t, secondOrderNo))
	require.Equal(t, 3, countSeckillOrders(t, e, id), "cancelled, surviving, and replacement orders coexist")
	require.Equal(t, 8, mysqlStock(t, e, id))
	require.Equal(t, "8", *redisGet(t, stockKey))
	require.Equal(t, "2", *redisGet(t, countKey))
}

func TestUserCancelSeckillOrderRestoresInventoryAndPurchaseSlot(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 3)
	token, userID := registerWithAddress(t, e, uniqueName("user-cancel"))
	stockKey, countKey := fmt.Sprintf("flashsale:stock:%d", id), fmt.Sprintf("flashsale:count:%d:%d", id, userID)

	w, purchase := purchaseOn(t, e, id, token, "user-cancel-slot")
	require.Equal(t, http.StatusAccepted, w.Code)
	orderNo := purchase["order_no"].(string)
	require.NotNil(t, pollOrder(t, e, orderNo, token))
	require.Equal(t, "2", *redisGet(t, stockKey))
	require.Equal(t, "1", *redisGet(t, countKey))

	w, body := doJSONOn(t, e.router, http.MethodPost, "/api/orders/"+orderNo+"/cancel", "", token)
	require.Equal(t, http.StatusNoContent, w.Code, "主动取消失败: %v", body)
	require.Equal(t, ordermodel.OrderStatusCancelled, orderStatus(t, orderNo))
	require.Equal(t, 3, mysqlStock(t, e, id))
	require.Equal(t, "3", *redisGet(t, stockKey))
	require.Equal(t, "0", *redisGet(t, countKey))
}

func TestSeckillTimeoutTransactionRollsBackWhenActivityRestoreFails(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	token, _ := registerWithAddress(t, e, uniqueName("rbcancel"))
	w, body := purchaseOn(t, e, id, token)
	require.Equal(t, http.StatusAccepted, w.Code)
	orderNo := body["order_no"].(string)
	require.NotNil(t, pollOrder(t, e, orderNo, token))
	require.Equal(t, 9, mysqlStock(t, e, id))
	require.NoError(t, env.gdb.Exec(
		"UPDATE orders SET expire_at = ? WHERE order_no = ?", time.Now().Add(-time.Minute), orderNo,
	).Error)
	t.Cleanup(func() {
		if err := env.gdb.Exec(
			"UPDATE orders SET expire_at = ? WHERE order_no = ?", time.Now().Add(time.Hour), orderNo,
		).Error; err != nil {
			t.Errorf("清理回滚测试订单: %v", err)
		}
	})
	timeout := flashsalesvc.NewSeckillCancellation(
		e.tx, e.orderSvc, failingRestoreActivities{e.activities},
		flashsalerepo.NewGORMPreDeduction(env.gdb), e.flashsaleSvc, metrics.New().Business(),
	)

	cancelled, failed, redisFailed, err := timeout.CancelExpired(context.Background())

	require.NoError(t, err)
	require.Zero(t, cancelled)
	require.GreaterOrEqual(t, failed, 1)
	require.Zero(t, redisFailed)
	require.Equal(t, ordermodel.OrderStatusPendingPayment, orderStatus(t, orderNo),
		"活动库存回补失败应回滚订单取消")
	require.Equal(t, 9, mysqlStock(t, e, id), "活动库存回补失败不得改变 MySQL 库存")
}

// T13 进行中对账（验收标准 2/4）：Redis 有扣减但无对应订单 → 补单信号告警；
// 只比对不写回——Redis 预扣不被覆盖。
func TestSeckillReconcileActiveWarnsOnly(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	stockKey := fmt.Sprintf("flashsale:stock:%d", id)
	token, _ := registerWithAddress(t, e, uniqueName("reccount"))
	w, body := purchaseOn(t, e, id, token)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.NotNil(t, pollOrder(t, e, body["order_no"].(string), token))

	// 已有 1 笔有效订单（Redis/MySQL 均剩 9），再人为制造额外预扣 2。
	require.Equal(t, "9", *redisGet(t, stockKey))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, env.redis.DecrBy(ctx, stockKey, 2).Err())

	warnings, err := e.reconcile.ReconcileActive(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, warnings, "差异应输出告警")
	var found flashsalesvc.ReconcileWarning
	ok := false
	for _, w := range warnings {
		if w.ActivityID == id {
			found, ok = w, true
			require.Equal(t, 7, w.RedisStock)
			require.Equal(t, 9, w.MySQLStock)
			require.Equal(t, 1, w.OrderCount, "对账应经 order 端口统计有效秒杀订单")
			break
		}
	}
	require.True(t, ok, "本活动差异应在告警列表中")
	require.Contains(t, found.Detail, "补单")
	require.Equal(t, "7", *redisGet(t, stockKey), "进行中只比对告警，Redis 预扣不得被覆盖")
}

// T13 收尾对账（验收标准 3）：刚结束的活动 Redis 库存与 MySQL 不一致 →
// 以 MySQL 为准对齐 Redis。
func TestSeckillReconcileEndedAlignsToMySQL(t *testing.T) {
	requireEnv(t)
	e := requireMQEnv(t)
	admin := adminToken(t, env)
	id := seedPublishedOnSale(t, admin, 10)
	stockKey := fmt.Sprintf("flashsale:stock:%d", id)

	// 活动拨成刚结束（窗口内）；Redis 残留扣减（3 ≠ MySQL 10）。
	require.NoError(t, env.gdb.Exec("UPDATE flashsale_activities SET end_at = ? WHERE id = ?",
		time.Now().Add(-5*time.Minute), id).Error)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, env.redis.Set(ctx, stockKey, 3, time.Minute).Err())

	aligned, err := e.reconcile.ReconcileEnded(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, aligned, 1, "存在差异的活动应对齐")
	require.Equal(t, "10", *redisGet(t, stockKey), "收尾后 Redis 与 MySQL 活动库存一致")
}
