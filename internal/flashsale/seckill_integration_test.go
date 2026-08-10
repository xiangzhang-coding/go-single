// T11 抢购接口集成测试（主 seam）：真实 MySQL + Redis + httptest 完整路由，
// 覆盖抢购接口全路径——限流（全局令牌桶 + 按用户 Redis 计数）→ 幂等键 →
// Lua 原子预扣 → 202 排队中；并发不超卖、重复拦截、窗口/下架拒绝。
package flashsale_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
)

// purchasePermissive 抢购专项测试用的宽松全局限流（仅测业务路径）。
var purchasePermissive = limiter.TokenBucketConfig{QPS: 10000, Burst: 10000}

// seedPublished 创建并上架进行中活动（库存 stock，限购 1），返回活动 id。
// admin 操作走 env.router，抢购走专项路由——两者共享同一 gdb/Redis，活动状态一致。
func seedPublished(t *testing.T, env *testEnv, admin string, stock int) int64 {
	t.Helper()
	skuID := seedSKU(t, env, admin)
	id := createActivity(t, env, admin, skuID, stock, 1, -time.Minute, time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", id), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	return id
}

// 抢购成功：202 排队中；Redis 库存减一、幂等键落键。
func TestSeckillPurchaseOK(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	id := seedPublished(t, env, admin, 10)
	router := env.newFlashsaleRouter(t, purchasePermissive, limiter.RedisCounterConfig{})
	token := registerAndToken(t, env, uniqueName("buyer"))

	w, body := purchase(t, router, id, token)
	require.Equal(t, http.StatusAccepted, w.Code, "预扣成功应返回 202 排队中: %s", w.Body.String())
	require.Equal(t, "queued", body["status"])
	require.Equal(t, 9, redisStock(t, env, id))
}

// 并发抢购不超卖：100 并发抢 50 库存 → 恰好 50 个 202，Redis 库存归零。
func TestSeckillPurchaseConcurrentNoOversell(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	id := seedPublished(t, env, admin, 50)
	router := env.newFlashsaleRouter(t, purchasePermissive, limiter.RedisCounterConfig{})

	const users = 100
	tokens := make([]string, users)
	for i := 0; i < users; i++ {
		tokens[i] = registerAndToken(t, env, uniqueName(fmt.Sprintf("racer%d", i)))
	}

	var wg sync.WaitGroup
	codes := make([]int, users)
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, _ := purchase(t, router, id, tokens[i])
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	accepted := 0
	for _, code := range codes {
		if code == http.StatusAccepted {
			accepted++
		} else {
			require.Equal(t, http.StatusConflict, code, "失败请求应 409（抢光/限购）")
		}
	}
	require.Equal(t, 50, accepted, "并发抢购不得超卖")
	require.Equal(t, 0, redisStock(t, env, id))
}

// 重复抢购被幂等键拦截：同一用户同一活动第二次 409，库存不再扣减。
func TestSeckillPurchaseDuplicateBlocked(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	id := seedPublished(t, env, admin, 10)
	router := env.newFlashsaleRouter(t, purchasePermissive, limiter.RedisCounterConfig{})
	token := registerAndToken(t, env, uniqueName("dup"))

	w, _ := purchase(t, router, id, token)
	require.Equal(t, http.StatusAccepted, w.Code)

	w, _ = purchase(t, router, id, token)
	require.Equal(t, http.StatusConflict, w.Code, "重复提交应被幂等键拦截")
	require.Equal(t, 9, redisStock(t, env, id), "重复请求不得再次预扣")

	claims, err := env.verifier.Verify(context.Background(), token)
	require.NoError(t, err)
	n, err := env.redis.Exists(context.Background(), idemKey(id, claims.UserID)).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "预扣成功后幂等键应保留")
}

// 全局限流：令牌桶桶空时抢购请求被 429 拒绝。
func TestSeckillPurchaseGlobalRateLimit(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	id := seedPublished(t, env, admin, 10)
	// QPS 1 / burst 1：第一个请求用掉唯一令牌，第二个必被 429。
	router := env.newFlashsaleRouter(t, limiter.TokenBucketConfig{QPS: 1, Burst: 1}, limiter.RedisCounterConfig{})
	token := registerAndToken(t, env, uniqueName("hammer"))

	w, _ := purchase(t, router, id, token)
	require.Equal(t, http.StatusAccepted, w.Code, "桶内请求应放行")

	w, body := purchase(t, router, id, token)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "桶空应 429")
	require.NotEmpty(t, body["error"])
}

// 按用户限流：同一用户窗口内超过 Max 次被 429；不同用户互不影响。
func TestSeckillPurchasePerUserRateLimit(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	id := seedPublished(t, env, admin, 10)
	router := env.newFlashsaleRouter(t, purchasePermissive, limiter.RedisCounterConfig{Max: 1, Window: time.Minute})
	tokenA := registerAndToken(t, env, uniqueName("rl_a"))
	tokenB := registerAndToken(t, env, uniqueName("rl_b"))

	w, _ := purchase(t, router, id, tokenA)
	require.Equal(t, http.StatusAccepted, w.Code)
	w, _ = purchase(t, router, id, tokenA)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "同一用户窗口内第 2 次应 429")

	w, _ = purchase(t, router, id, tokenB)
	require.Equal(t, http.StatusAccepted, w.Code, "不同用户限流互不影响")
}

// 活动未开始/已结束/已下架时抢购被拒（409；业务拒绝释放幂等键，可重试）。
func TestSeckillPurchaseWindowRejections(t *testing.T) {
	env := requireEnv(t)
	admin := adminToken(t, env)
	router := env.newFlashsaleRouter(t, purchasePermissive, limiter.RedisCounterConfig{})
	token := registerAndToken(t, env, uniqueName("eager"))

	// 未开始：上架成功但窗口未开 → 409 窗口外。
	skuID := seedSKU(t, env, admin)
	notStarted := createActivity(t, env, admin, skuID, 10, 1, time.Hour, 2*time.Hour)
	w, _ := doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/publish", notStarted), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, _ = purchase(t, router, notStarted, token)
	require.Equal(t, http.StatusConflict, w.Code, "未开始抢购应被拒")

	// 已结束：把进行中活动的结束时间改到过去 → 409 窗口外。
	ended := seedPublished(t, env, admin, 10)
	require.NoError(t, env.gdb.Table("flashsale_activities").Where("id = ?", ended).
		Update("end_at", time.Now().Add(-time.Minute)).Error)
	w, _ = purchase(t, router, ended, token)
	require.Equal(t, http.StatusConflict, w.Code, "已结束抢购应被拒")

	// 已下架：status 非 on_sale + 清除预热库存 → 409 已下架。
	offline := seedPublished(t, env, admin, 10)
	w, _ = doJSON(t, env, http.MethodPost, fmt.Sprintf("/api/admin/flashsales/%d/unpublish", offline), "", admin)
	require.Equal(t, http.StatusNoContent, w.Code)
	w, _ = purchase(t, router, offline, token)
	require.Equal(t, http.StatusConflict, w.Code, "已下架抢购应被拒")

	// 业务拒绝后幂等键释放：同一用户可抢购其他进行中活动。
	retry := seedPublished(t, env, admin, 10)
	w, _ = purchase(t, router, retry, token)
	require.Equal(t, http.StatusAccepted, w.Code, "业务拒绝释放幂等键后应可重新抢购")
}
