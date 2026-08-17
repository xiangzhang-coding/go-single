---
sidebar_position: 8
---

# 07 秒杀架构

## Q1. 秒杀整体架构：限流 → 幂等 → 预扣 → MQ → 异步落单

**答案要点**

- **前置限流**：全局令牌桶（QPS 封顶）+ 按用户计数，把流量先削一轮。
- **幂等键**：先于预扣抢占，挡并发重复提交。
- **Redis 原子预扣**：状态/窗口/库存/限购一次校验 + 扣减，扛峰值。
- **MQ 异步落单**：DB 只按消费速率写单，天然串行。
- 返回 **202 + order_no**，前端轮询；失败分支有对应状态码。

**可运行代码**

```go title="interview/ch07_flashsale/q01_architecture/main.go"
package main

import "fmt"

type result struct {
	phase string
	ok    bool
}

func main() {
	// 一次抢购请求走过的完整链路（与 DESIGN.md 秒杀时序一致）。
	flow := []result{
		{"[1] 全局令牌桶中间件限流（429）", true},
		{"[2] 按用户 Redis 固定窗口限流（flashsale:rl:{user}）", true},
		{"[3] 幂等键抢占（flashsale:idem:{activity}:{user}）", true},
		{"[4] Lua 原子预扣（校验→DECR 库存 + INCR 计数）", true},
		{"[5] 生成雪花订单号 → 发布 MQ flashsale.order.create", true},
		{"[6] 返回 202 排队中 + order_no，前端轮询订单", true},
		{"[7] 消费者事务落单（订单+订单项+条件扣活动库存）", true},
	}
	for _, f := range flow {
		fmt.Printf("%-64s → %v\n", f.phase, f.ok)
	}
	fmt.Println("为什么拆两段：预扣在 Redis 扛峰值（微秒级），落单交给 DB 按自身节奏消费")
}

```

**项目位置**：`internal/flashsale/handler/flashsale_handler.go` 的 `Purchase`（202 排队 + order_no）；`internal/flashsale/service/flashsale_service.go` 的 `Seckill`；消费者 `flashsale_consumer.go`；时序图 `docs/DESIGN.md`。

## Q2. 令牌桶限流：平滑突发流量

**答案要点**

- 桶容量 burst + 恒定补充速率：突发可被桶接住，长期速率被封顶。
- 每个请求消耗一个令牌，无令牌 → 429。
- 项目用 x/time/rate 的 `Limiter`，只挂在秒杀购买接口。
- 演进：多实例需分布式限流（Redis 实现，见 BACKLOG）。

**可运行代码**

```go title="interview/ch07_flashsale/q02_token_bucket/main.go"
package main

import (
	"fmt"
	"time"
)

// 令牌桶：桶容量 burst，按 rate 恒定补充；有令牌才放行。
// 与 internal/platform/limiter 用 x/time/rate 的实现语义一致。
type tokenBucket struct {
	rate   float64 // 每秒补充令牌数
	burst  int
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: burst, tokens: float64(burst), last: time.Now()}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > float64(b.burst) {
		b.tokens = float64(b.burst)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func main() {
	b := newTokenBucket(2, 5) // 2 QPS，桶 5
	allowed := 0
	for i := 1; i <= 10; i++ {
		if b.allow() {
			allowed++
			fmt.Printf("请求 %2d 放行\n", i)
		} else {
			fmt.Printf("请求 %2d 限流 429\n", i)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("突发 5 个被桶接住，随后按 2/s 节奏放行（共放行 %d）\n", allowed)
}

```

**项目位置**：`internal/platform/limiter/limiter.go` 的 `NewTokenBucket`（x/time/rate）；挂在 `POST /api/flashsales/:id/purchase`（flashsale_handler.go）；QPS 配置 `configs/config.yaml` 的 `flashsale.token_bucket`。

## Q3. Lua 原子预扣：一次调用完成全部校验与扣减

**答案要点**

- Redis 适配器内部脚本返回码协议：1 成功 / 0 抢光 / -1 窗口外 / -2 超限购 / -3 下架。
- 缓存适配器先把整数码映射为类型化结果，service 再映射哨兵错误，handler 最后映射 HTTP 状态码。
- 参数全部由服务端注入（状态/时间窗口/限购来自 DB 行），脚本内不做 IO。
- 成功/失败打点 `seckill_prededuct_total{result}`，失败分类可观测。

**可运行代码**

```go title="interview/ch07_flashsale/q03_lua_prededuct/main.go"
package main

import "fmt"

// 预扣返回码（与项目 Redis 适配器内部协议一致）：1 成功 / 0 抢光 / -1 窗口外 / -2 超限购 / -3 下架。
func preDeduct(status string, now, start, end, stock, limit, count int) int {
	if status != "on_sale" {
		return -3
	}
	if now < start || now > end {
		return -1
	}
	if stock < 1 {
		return 0
	}
	if count+1 > limit {
		return -2
	}
	return 1
}

func main() {
	cases := []struct {
		name   string
		status string
		stock  int
		count  int
		now    int
	}{
		{"正常抢购", "on_sale", 10, 0, 100},
		{"库存为 0", "on_sale", 0, 0, 100},
		{"未开始", "on_sale", 10, 0, -10},
		{"已结束", "on_sale", 10, 0, 300},
		{"超过限购", "on_sale", 10, 1, 100},
		{"已下架", "off_sale", 10, 0, 100},
	}
	for _, c := range cases {
		code := preDeduct(c.status, c.now, 0, 200, c.stock, 1, c.count)
		msg := map[int]string{1: "成功 DECR 库存 + INCR 计数", 0: "抢光 ErrSoldOut", -1: "窗口外 ErrNotInWindow", -2: "超限购 ErrLimitReached", -3: "下架 ErrOffline"}[code]
		fmt.Printf("%-10s → %2d（%s）\n", c.name, code, msg)
	}
}

```

**项目位置**：`internal/platform/cache/atomic.go` 的 `PreDeductFlashSale` 封装脚本与返回码；`internal/flashsale/service/flashsale_service.go` 的 `PreDeduct` 只处理类型化结果。

## Q4. 幂等键生命周期：何时保留、何时释放

**答案要点**

- 幂等键 = "本次请求是否已处理过"的标记，SETNX 抢占。
- 成功 → 保留（落单前不许再来一次）；业务拒绝 → 释放（允许重试）；
  基础设施失败 → 保留（防瞬时故障下重复预扣）。
- MQ 发布失败也保留：宁可这次用户被挡，也不能扣两次——对账兜底。
- TTL 30min 自清理；回补时 DEL 幂等键 = 取消后可再次抢购。

**可运行代码**

```go title="interview/ch07_flashsale/q04_idem_key/main.go"
package main

import "fmt"

func main() {
	// 幂等键先于预扣抢占：挡得住并发重复提交。
	// 预扣结果决定键的去留（30min TTL 兜底自动清理）。
	fmt.Println("预扣成功           → 保留幂等键（直到订单落库/对账）")
	fmt.Println("业务拒绝(抢光/限购) → 释放幂等键（允许窗口内再试一次）")
	fmt.Println("基础设施失败(网络)  → 保留幂等键（防瞬时故障下重复预扣）")
	fmt.Println("MQ 发布失败         → 保留幂等键（对账兜底补单，不重复扣）")

	// 关键洞察：业务拒绝释放 = 用户可重试；基础失败保留 = 用户被"挡住"
	// 但不会扣两次。幂等键语义 = "本次请求是否已处理过"。
	fmt.Println()
	fmt.Println("对比：下单幂等键 order:idem:{user}:{client_request_id} TTL 15min")
}

```

**项目位置**：`internal/flashsale/service/flashsale_service.go` 的 `Seckill` 抢占与 `isBusinessReject` 释放；`internal/platform/cache/atomic.go` 的 `AcquireIdempotency` / `RestoreFlashSale` 封装 SETNX 与回补时 DEL。

## Q5. 库存三方一致性：Redis 预扣 vs MySQL 库存 vs 有效订单数

**答案要点**

- 事实源切换：活动中 Redis 预扣为事实；结束后 MySQL 对齐。
- **进行中**：只告警不写回（避免误伤预扣数据）；`redis < mysql` 是"有预扣无订单"信号。
- **刚结束**（30min 窗口内）：以 MySQL 为准 SET 对齐 Redis；下架活动不回建。
- 对账是最终一致的兜底，不能替代主链路正确性。

**可运行代码**

```go title="interview/ch07_flashsale/q05_stock_consistency/main.go"
package main

import "fmt"

func main() {
	// 活动结束后的对账（ReconcileEnded）：以 MySQL 为准对齐 Redis。
	// 进行中对账（ReconcileActive）：只告警不写回。
	type snapshot struct {
		redis  int
		mysql  int
		orders int
		action string
	}
	rows := []snapshot{
		{50, 50, 0, "一致，无需处理"},
		{45, 50, 5, "一致：预扣 5 + 落单 5 = 一致"},
		{45, 50, 3, "异常：预扣 5 但只落 3 单 → 有预扣无订单，补单/回补信号"},
		{48, 50, 2, "一致：预扣 2 落单 2"},
	}
	for _, r := range rows {
		ok := r.redis+r.orders == r.mysql
		fmt.Printf("redis=%2d mysql=%2d 订单=%2d → %-40s %v\n",
			r.redis, r.mysql, r.orders, r.action, ok)
	}
	fmt.Println("结束 30min 内：以 MySQL 为准 SET 对齐 Redis（key 缺失回建）；下架活动不回建")
}

```

**项目位置**：`internal/flashsale/service/reconciliation.go`——`diffActive`（96-132）、`ReconcileEnded`（141-177，`endedReconcileWindow`=30min）；cron 注册 `cmd/server/main.go`（每小时/每分钟）。

## Q6. 超时未支付回补：取消订单 + 回补库存

**答案要点**

- cron 每分钟扫 `pending_payment` 且超时的秒杀订单（10min 超时）。
- 事务内：条件取消（仅未支付才生效）+ 回补 MySQL 库存。
- 提交后：`RestoreFlashSale` 类型化缓存能力回补 Redis（内部 Lua：INCR 库存 + DECR 计数 + DEL 幂等键）。
- Redis 回补失败不阻塞：对账 cron 兜底对齐。

**可运行代码**

```go title="interview/ch07_flashsale/q06_timeout_refund/main.go"
package main

import "fmt"

type state struct {
	redisStock  int
	mysqlStock  int
	orderPaid   bool
	orderStatus string
}

// 超时取消流程：事务内条件取消 + 回补 MySQL → 提交后经 RestoreFlashSale 回补 Redis。
func timeoutCancel(s *state) {
	if s.orderStatus != "pending_payment" {
		return // 条件更新不命中：订单已支付，跳过
	}
	s.orderStatus = "cancelled"
	s.mysqlStock++ // 事务内回补 MySQL（与订单取消同事务）
	s.redisStock++ // 提交后缓存适配器原子执行：INCR 库存 + DECR 计数 + DEL 幂等键
	s.orderPaid = false
}

func main() {
	s := &state{redisStock: 0, mysqlStock: 0, orderStatus: "pending_payment"}
	fmt.Println("下单后 10min 未支付，cron 每分钟扫描一次:")
	timeoutCancel(s)
	fmt.Printf("取消后：订单=%s redis=%d mysql=%d（库存回补，用户可再次抢购）\n",
		s.orderStatus, s.redisStock, s.mysqlStock)
}

```

**项目位置**：`internal/order/service/order_service.go` 的 `CancelExpiredSeckill`（920-968）——批量扫描 `ListExpiredSeckillPending` → 事务条件取消 + `RestoreStock` → 提交后 `RestoreRedis`；cron `seckill-timeout-cancel` 每分钟（`cmd/server/main.go` 435-449）。

## Q7. 雪花 ID：全局唯一、趋势递增、无中心依赖

**答案要点**

- 布局：41bit 毫秒时间戳 + 10bit 机器位 + 12bit 序号。
- 趋势递增利于索引；可解码时间（排序/审计）。
- 同毫秒序号耗尽自旋等下一毫秒；时钟回拨要防御（ErrClockBackward）。
- 单机 Mutex + 原子自旋；多实例靠机器位区分。

**可运行代码**

```go title="interview/ch07_flashsale/q07_snowflake/main.go"
package main

import (
	"fmt"
	"sync"
	"time"
)

// 与项目 internal/platform/snowflake 同构的简化版：
// 41bit 毫秒时间戳 + 10bit 机器位 + 12bit 序号；同毫秒序号耗尽则自旋等下一毫秒。
const (
	epoch      = 1704067200000 // 2024-01-01
	machineBit = 10
	seqBit     = 12
)

type snowflake struct {
	mu      sync.Mutex
	machine int64
	lastMS  int64
	seq     int64
}

func (s *snowflake) Next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if now == s.lastMS {
		s.seq = (s.seq + 1) & (1<<seqBit - 1)
		for s.seq == 0 && time.Now().UnixMilli() == s.lastMS {
			time.Sleep(100 * time.Microsecond) // 序号耗尽自旋
		}
		now = time.Now().UnixMilli()
	} else {
		s.seq = 0
	}
	s.lastMS = now
	return (now-epoch)<<(machineBit+seqBit) | s.machine<<seqBit | s.seq
}

func main() {
	s := &snowflake{machine: 1}
	seen := map[int64]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ { // 并发 100 次生成
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := s.Next()
			mu.Lock()
			seen[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	fmt.Printf("生成 %d 个 ID，全部唯一: %v\n", len(seen), len(seen) == 100)
	fmt.Println("特性：趋势递增（利于索引）、可解码时间（取高 41bit）")
}

```

**项目位置**：`internal/platform/snowflake/snowflake.go`（41+10+12 布局、时钟回拨防御）；订单号/支付号由它生成（order/payment service）。
