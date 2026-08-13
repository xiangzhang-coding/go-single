---
sidebar_position: 13
---

# 12 容错与降级

## Q1. 超时预算：分层超时，故障快速失败

**答案要点**

- 无超时 = 依赖挂多久请求就挂多久，goroutine 堆积拖垮进程。
- 分层超时：请求 5s / MQ 消息 15s / cron 任务 5min / 探活 2s。
- 超时经 context 逐层传递：内层超时 = 外层快速失败（不重复等待）。
- 超时值要留余量：下游最坏延迟 × 安全系数，太长掩盖故障、太短误伤正常流量。

**可运行代码**

```go title="interview/ch12_resilience/q01_timeout_budget/main.go"
package main

import (
	"context"
	"fmt"
	"time"
)

func main() {
	// 项目的分层超时（故障逐层截断，错误向上快速传播）。
	fmt.Println("请求级 5s   requestTimeout 中间件（每请求 context，504）")
	fmt.Println("消息级 15s  MQ 单条消息处理超时（rabbitmq.go msgTimeout）")
	fmt.Println("任务级 5min cron 单次执行超时（registry.go per-run timeout）")
	fmt.Println("探活级 2s   health 探测各依赖")
	fmt.Println("退避可取消  retry.sleep 对 ctx 敏感，超时即停不再重试")

	// 演示：子任务超时传播。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	select {
	case <-ctx.Done():
		fmt.Printf("链路超时快速失败，耗时 %v\n", time.Since(start).Round(time.Millisecond))
	}
}

```

**项目位置**：`cmd/server/middleware.go` 的 `requestTimeout`；`internal/platform/mq/rabbitmq.go` 的 `msgTimeout`；`internal/platform/cron/registry.go`；health 2s。

## Q2. 指数退避 + 抖动

**答案要点**

- 固定间隔重试在故障恢复瞬间"惊群"（thundering herd）——指数退避拉开节奏。
- 公式：`initial × 2^attempt`，封顶 max，叠加随机抖动 `[0, jitter)`。
- 退避等待必须可取消（ctx）：超时/关闭立即停，不再发下一次尝试。
- 约束：**只有幂等操作能重试**（重试 = 重复执行 fn）。

**可运行代码**

```go title="interview/ch12_resilience/q02_backoff/main.go"
package main

import (
	"fmt"
	"time"
)

// 与 retry.backoff 同构：InitialBackoff × 2^attempt，封顶 MaxBackoff，叠加 [0,Jitter)。
func backoff(initial, max time.Duration, attempt int) time.Duration {
	d := initial
	for i := 0; i < attempt; i++ {
		d *= 2
		if d > max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

func main() {
	cfg := struct {
		attempts, initial, max time.Duration
	}{3, 100 * time.Millisecond, time.Second}

	for attempt := 0; attempt < int(cfg.attempts); attempt++ {
		fmt.Printf("第 %d 次失败后的等待: %v（封顶 %v；抖动避免惊群）\n",
			attempt+1, backoff(cfg.initial, cfg.max, attempt), cfg.max)
	}
	fmt.Println("使用约束：只有幂等操作才允许重试（retry.Do 包注释）")
}

```

**项目位置**：`internal/platform/retry/retry.go`——`backoff`（86-102）+ 可取消 `sleep`（105-114）+ `retry.Stop`；启用点：order Create（265-276）、flashsale publishSeckillSuccess（520）、payment MockPay（84-92）。

## Q3. 熔断接线：保护薄弱下游

**答案要点**

- 熔断保护"已打满的下游"：连续失败 → 打开，快速失败不再加重下游。
- 打开 → 冷却 → 半开试探 → 恢复/再打开。
- **包装位置**：只包 Consume（发布失败靠重试而非熔断）；`ErrCircuitOpen` 视为瞬时错误 requeue。
- 数据类错误（ErrPermanent）不计入失败——业务问题不该触发熔断。

**可运行代码**

```go title="interview/ch12_resilience/q03_breaker_config/main.go"
package main

import "fmt"

type breakerSettings struct {
	name                   string
	maxConsecutiveFailures int
	interval               string
	timeout                string
}

func main() {
	// 项目配置（configs/config.yaml mq.circuit.*）。
	settings := breakerSettings{
		name:                   "mq.consume",
		maxConsecutiveFailures: 3,
		interval:               "30s",
		timeout:                "10s",
	}
	fmt.Printf("熔断器 %s：连续 %d 次失败 → 打开；%s 内不尝试；%s 后半开试探\n",
		settings.name, settings.maxConsecutiveFailures, settings.interval, settings.timeout)

	fmt.Println()
	fmt.Println("接线细节（internal/platform/mq/breaker.go）：")
	fmt.Println("  只包 Consume（Publish/Ping/Close 直通——发布失败要靠重试而非熔断）")
	fmt.Println("  ErrCircuitOpen 视为瞬时错误 → 消息 requeue 重投")
	fmt.Println("  ErrPermanent 不计入失败（数据问题不该触发熔断）")
}

```

**项目位置**：`internal/platform/mq/breaker.go` 的 `WrapCircuitBreaker`（gobreaker）；装饰器栈 `cmd/server/main.go` 221-230；配置 `mq.circuit.*`。

## Q4. 降级链：缓存 → DB → 默认值

**答案要点**

- 降级 = 故障时不报错，返回"够用"的数据：缓存 → DB → 默认值逐级兜底。
- 每级降级要**可观测**（日志/指标），否则故障静默。
- 降级数据要标注（如库存"约"值），前端/运营知道不实时。
- 降级不改变写路径：写仍走主链路，读可以降。

**可运行代码**

```go title="interview/ch12_resilience/q04_fallback_chain/main.go"
package main

import (
	"errors"
	"fmt"
)

var (
	ErrCacheDown = errors.New("cache down")
	ErrDBDown    = errors.New("db down")
)

// 三级降级：命中返回；miss/故障逐级下降，最终给默认值而不是报错。
func stockLeft(cacheOK, dbOK bool) int {
	if cacheOK {
		v := 46 // 命中缓存
		fmt.Println("① 缓存命中:", v)
		return v
	}
	fmt.Println("① 缓存 miss/故障 → ② 直查 DB")
	if dbOK {
		v := 50
		fmt.Println("② DB 查询:", v, "（并回填缓存）")
		return v
	}
	fmt.Println("② DB 也不可用 → ③ 返回配置库存（页面可用，数字不实时）")
	return 100
}

func main() {
	stockLeft(true, true)   // 正常
	stockLeft(false, true)  // 缓存挂
	stockLeft(false, false) // 缓存 + DB 全挂
	_ = ErrCacheDown
	_ = ErrDBDown
}

```

**项目位置**：`internal/flashsale/service/flashsale_service.go` 的 `ListUserActivities`（379-385 降级配置库存）；product `GetDetail` miss 回填、读失败直查 DB（`product_service.go`）。

## Q5. 异步受理 + 轮询：202 与最终一致

**答案要点**

- 高耗时/高并发操作不阻塞请求：**202 受理 + 状态轮询**（秒杀抢购）。
- 受理即返回排队号（order_no），前端轮询最终结果。
- 用户侧取舍：立即失败分支（限流/抢光）仍同步 4xx；只有"受理成功"走异步。
- 兜底：异步链路失败由对账补偿，保证最终一致。

**可运行代码**

```go title="interview/ch12_resilience/q05_queue_poll/main.go"
package main

import (
	"fmt"
	"time"
)

type task struct {
	status string // queued → processing → done
}

func main() {
	// 秒杀抢购：预扣成功立即返回 202，落单异步进行；前端轮询订单号。
	t := &task{status: "queued"}

	fmt.Println("POST /api/flashsales/:id/purchase → 202 {\"status\":\"queued\",\"order_no\":\"O1\"}")

	go func() { // 消费者异步落单
		time.Sleep(150 * time.Millisecond)
		t.status = "processing"
		time.Sleep(150 * time.Millisecond)
		t.status = "done"
	}()

	for i := 0; i < 8; i++ { // 前端 1.5s×30 轮询
		time.Sleep(100 * time.Millisecond)
		fmt.Printf("轮询 GET /api/orders/O1 → %s\n", t.status)
		if t.status == "done" {
			fmt.Println("→ 订单已生成，跳转订单详情（失败则提示稍后重试）")
			break
		}
	}
}

```

**项目位置**：`internal/flashsale/handler/flashsale_handler.go` 返回 202 + order_no；前端轮询见 `web/faire` 秒杀页；异步落单 `flashsale_consumer.go`。

## Q6. 最终一致兜底：故障矩阵与对账

**答案要点**

- 主链路保证"快速受理"，**对账保证最终一致**——每类故障都有对应兜底。
- 兜底设计原则：不丢（at-least-once + 重投）、不重（幂等键 + 唯一键）、最终一致（对账）。
- 对账只告警 or 写回要分场景：进行中只告警（避免误伤预扣），结束后才对齐。
- 对账频率按损失半径定：库存一致性每分钟，慢速补偿每小时。

**可运行代码**

```go title="interview/ch12_resilience/q06_reconcile_final/main.go"
package main

import "fmt"

func main() {
	// 秒杀链路的故障矩阵 → 兜底机制。
	type row struct{ failure, fallback string }
	table := []row{
		{"MQ 发布失败（预扣已成功）", "保留幂等键 + 对账补单（有预扣无订单 → 回补/补单信号）"},
		{"消费者 DB 瞬时故障", "Nack requeue 重投，at-least-once"},
		{"消息数据问题（活动不存在）", "永久失败进 DLQ，对账/人工补偿消费"},
		{"Redis 回补失败（取消订单后）", "对账 cron 以 MySQL 为准对齐 Redis"},
		{"结束 30min 后 Redis key 残留/丢失", "ReconcileEnded 以 MySQL 为准 SET 对齐"},
		{"重复消息/重复提交", "唯一键 + 幂等键（两套去重）"},
	}
	for _, r := range table {
		fmt.Printf("%-28s → %s\n", r.failure, r.fallback)
	}
	fmt.Println("原则：主链路只保证快速受理，最终一致由对账兜底（每小时/每分钟 cron）")
}

```

**项目位置**：`internal/flashsale/service/reconciliation.go`（`diffActive`/`ReconcileEnded`）、`cmd/server/main.go` 对账 cron（450-487）、`flashsale_consumer.go` 错误分类、`mq.go` 死信配置。

## Q7. 舱壁隔离与背压

**答案要点**

- 舱壁（Bulkhead）：给慢依赖/关键路径设置并发上限，一个模块打满不拖垮全局。
- 背压（Backpressure）：消费端按自身能力拉数据（QoS 预取 1），不无限积压内存。
- 实现：semaphore 并发池、有界队列、缓冲满断开（WS 慢消费者）。
- 演进：Sentinel-golang 流量控制/熔断（BACKLOG 备选 gobreaker）。

**可运行代码**

```go title="interview/ch12_resilience/q07_bulkhead/main.go"
package main

import (
	"fmt"
	"sync"
)

type bulkhead struct {
	sem chan struct{}
}

func newBulkhead(max int) *bulkhead { return &bulkhead{sem: make(chan struct{}, max)} }

func (b *bulkhead) run(task string) bool {
	select {
	case b.sem <- struct{}{}: // 有舱位：执行
	default:
		fmt.Printf("%s 被拒（舱位满，降级处理）\n", task)
		return false
	}
	defer func() { <-b.sem }()
	fmt.Printf("%s 执行中（并发 %d）\n", task, len(b.sem)+1)
	return true
}

func main() {
	// 舱壁：给慢依赖/关键路径设置并发上限，个别模块打满不影响全局。
	b := newBulkhead(2)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			b.run(fmt.Sprintf("任务%d", n))
		}(i)
	}
	wg.Wait()

	fmt.Println()
	fmt.Println("背压同理：MQ Qos(1) 预取 1 让消费端按自身节奏拉消息（不积压内存）")
	fmt.Println("演进：关键路径 semaphore 并发池（BACKLOG 舱壁隔离）、Sentinel-golang")
}

```

**项目位置**：RabbitMQ 消费 `Qos(1,0,false)`（`internal/platform/mq/rabbitmq.go`）；WS 慢消费者缓冲 64 满即断开（`internal/platform/ws/hub.go`）；显式舱壁列 BACKLOG。
