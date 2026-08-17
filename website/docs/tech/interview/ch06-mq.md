---
sidebar_position: 7
---

# 06 MQ（RabbitMQ）

## Q1. 为什么用消息队列：削峰填谷与解耦

**答案要点**

- **削峰**：生产端峰值瞬时积压，消费端按自身节奏处理，DB 只承受消费速率。
- **解耦**：生产方不关心谁消费、消费方不关心谁生产，各自演进。
- 代价：一致性变最终一致（at-least-once + 幂等 + 对账）、链路复杂度上升。
- 秒杀正是典型：预扣扛峰值（Redis），落单交给 MQ 串行消化。

**可运行代码**

```go title="interview/ch06_mq/q01_why_mq/main.go"
package main

import (
	"fmt"
	"time"
)

func main() {
	// 秒杀场景：1 万 QPS 抢购打到 DB 会打崩；经 MQ 后消费端按自身节奏落单。
	rate := 10000 // 抢购峰值 QPS
	dbCapacity := 200
	queueDepth := 0

	for second := 0; second < 10; second++ {
		incoming := rate
		queueDepth += incoming
		consumed := dbCapacity // 消费端恒定速率（Qos 1 串行）
		if queueDepth < consumed {
			queueDepth = 0
		} else {
			queueDepth -= consumed
		}
		fmt.Printf("第 %d 秒：生产 %d，消费 %d，积压 %d\n", second+1, incoming, consumed, queueDepth)
		if queueDepth > 0 && second == 9 {
			fmt.Println("→ DB 始终只承受消费端速率，峰值被 MQ 削平；积压由后台慢慢消化")
		}
	}
	time.Sleep(0)
}

```

**项目位置**：秒杀预扣成功 → 发布 `flashsale.order.create`（`SeckillOrderQueue`，flashsale_service.go）→ 消费者串行落单（`flashsale_consumer.go`、`cmd/server/main.go` 重连循环）。

## Q2. 发布确认（Publisher Confirm）

**答案要点**

- 开启 `Confirm` 模式后，发布返回 `DeferredConfirmation`，`WaitContext` 等 broker 落盘确认。
- 确认超时/未确认 = 未送达 → 调用方重试或降级。
- 消息 `DeliveryMode=Persistent` + durable 队列：broker 重启不丢。
- 秒杀发布失败保留幂等键 + 对账兜底：宁可慢，不可重复扣。

**可运行代码**

```go title="interview/ch06_mq/q02_publisher_confirm/main.go"
package main

import (
	"fmt"
	"time"
)

type broker struct{ down bool }

// 模拟 Confirm 模式：发布后等 broker 落盘确认；超时视为未送达。
func (b broker) publishWithConfirm(body string, timeout time.Duration) (bool, error) {
	if b.down {
		return false, fmt.Errorf("连接失败")
	}
	time.Sleep(5 * time.Millisecond) // 模拟落盘
	return true, nil
}

func main() {
	b := broker{}
	for attempt := 1; attempt <= 3; attempt++ {
		acked, err := b.publishWithConfirm(`{"order_no":"O1","activity_id":1001}`, time.Second)
		if err != nil {
			fmt.Printf("第 %d 次发布失败：%v\n", attempt, err)
			continue
		}
		if !acked {
			fmt.Println("broker 拒收（nack）")
			continue
		}
		fmt.Println("发布成功：broker 已确认落盘（DeliveryMode=Persistent 持久化消息）")
		return
	}
}

```

**项目位置**：`internal/platform/mq/rabbitmq.go` 的 `Publish`——`ch.Confirm(false)` + `PublishWithDeferredConfirm` + `conf.WaitContext(ctx)`；秒杀侧发布失败保留幂等键（`flashsale_service.go` publishSeckillSuccess）。

## Q3. 手动 ACK 与 QoS 预取：消费三态

**答案要点**

- 手动 ack 三态：成功 `Ack`（出队）、瞬时失败 `Nack(requeue=true)`（重投）、永久失败 `Nack(requeue=false)`（死信）。
- 不 ack 也不 nack：消息留在队列，连接断开自动重投（at-least-once）。
- `Qos(prefetch=1)`：单消费者一次只取一条，顺序处理、天然串行。
- 消费 handler 内要有**每条消息超时**，防坏消息卡死消费者。

**可运行代码**

```go title="interview/ch06_mq/q03_manual_ack_qos/main.go"
package main

import (
	"fmt"
)

type ackKind int

const (
	ackOK    ackKind = iota // 成功：Ack
	ackRetry                // 瞬时失败：Nack(requeue=true) 重投
	ackDead                 // 永久失败：Nack(requeue=false) 进死信
)

func process(body string) ackKind {
	switch body {
	case "ok":
		return ackOK
	case "db-busy":
		return ackRetry // 瞬时故障：重投，at-least-once
	default:
		return ackDead // 数据问题：重投也会失败
	}
}

func main() {
	for _, body := range []string{"ok", "db-busy", "bad-data"} {
		switch process(body) {
		case ackOK:
			fmt.Printf("%-8s → Ack（消息出队）\n", body)
		case ackRetry:
			fmt.Printf("%-8s → Nack requeue（回到队首/队尾，等下次）\n", body)
		case ackDead:
			fmt.Printf("%-8s → Nack 不重投（进 DLQ 死信队列）\n", body)
		}
	}
	fmt.Println("QoS 预取 1：单消费者一次只取一条，天然串行（秒杀落单不需要并发）")
}

```

**项目位置**：`internal/platform/mq/rabbitmq.go` 的 `consumeOne` 三态分类（nil→Ack、`ErrPermanent`→Nack(false,false)、其余→Nack(false,true)）；`Qos(1,0,false)` 在 `Consume`。

## Q4. 死信队列（DLQ）

**答案要点**

- 声明主队列时配 `x-dead-letter-exchange` + `x-dead-letter-routing-key`，拒收消息自动路由进 DLQ。
- 命名约定 `<主队列>.dlq`；DLQ 也要 durable。
- 哪些消息该死信：数据问题（活动不存在、无地址、库存不足）——重投也失败。
- DLQ 由对账/人工补偿消费，避免坏消息无限循环阻塞主队列。

**可运行代码**

```go title="interview/ch06_mq/q04_dlq/main.go"
package main

import "fmt"

type queue struct {
	main []string
	dlq  []string
}

// 模拟：声明主队列时带 x-dead-letter-exchange/x-dead-letter-routing-key；
// 消费端 Nack(requeue=false) 的消息经默认交换机路由进 <主队列>.dlq。
func (q *queue) publish(body string) { q.main = append(q.main, body) }

func (q *queue) rejectDead(body string) {
	// 从主队列移除，进死信。
	for i, b := range q.main {
		if b == body {
			q.main = append(q.main[:i], q.main[i+1:]...)
			break
		}
	}
	q.dlq = append(q.dlq, body)
}

func main() {
	q := &queue{}
	q.publish(`{"order_no":"O2","activity_id":1001}`)
	q.publish(`{"order_no":"O3","activity_id":9999}`) // 活动不存在 → 永久失败

	// 消费者：O3 是坏消息（活动不存在），拒收进死信；O2 正常 Ack。
	q.rejectDead(`{"order_no":"O3","activity_id":9999}`)
	fmt.Println("主队列剩余:", q.main)
	fmt.Println("死信队列（供对账/人工补偿）:", q.dlq)
	fmt.Println("死信消息处理成功后需手动清理，或由对账任务兜底")
}

```

**项目位置**：`internal/platform/mq/rabbitmq.go` 的 `declareQueue`；秒杀消费者把"活动不存在/无地址/库存不足"归为永久失败（`flashsale_consumer.go` 的 `permanent`）。

## Q5. at-least-once 与幂等消费

**答案要点**

- RabbitMQ 手动 ack 模式默认 at-least-once：消费方崩溃/超时 → 消息重投，**不丢但可能重复**。
- 重复投递不可怕，重复执行才可怕：消费必须幂等。
- 幂等手段：唯一键兜底（DB 唯一约束 + 1062 映射）+ 业务条件（库存条件扣减）。
- 对账兜底：MQ 全挂时，预扣了但没落单的由对账发现并补偿。

**可运行代码**

```go title="interview/ch06_mq/q05_at_least_once/main.go"
package main

import (
	"fmt"
)

// 秒杀落单消费者：同一消息可能被投递多次（重连/超时重投），
// 落单必须幂等——靠唯一键（order_no 主键 / user_activity_key 唯一约束）去重。
type orderRepo struct {
	created map[string]bool
	stock   int
}

func (r *orderRepo) createSeckill(orderNo string) error {
	if r.created[orderNo] {
		return fmt.Errorf("duplicate: %s（幂等成功，不重复扣减库存）", orderNo)
	}
	r.created[orderNo] = true
	r.stock--
	return nil
}

func main() {
	repo := &orderRepo{created: map[string]bool{}, stock: 5}

	// 同一消息投递 3 次（网络抖动重投）。
	for i := 0; i < 3; i++ {
		err := repo.createSeckill("O20260813001")
		if err != nil {
			fmt.Println("第", i+1, "次:", err)
		} else {
			fmt.Println("第", i+1, "次: 落单成功")
		}
	}
	fmt.Println("库存只扣 1 次:", repo.stock == 4)
}

```

**项目位置**：`internal/flashsale/service/flashsale_consumer.go` → `order.CreateSeckillInTx`；唯一键 `uk_orders_user_activity_key`（`migrations/000014_seckill_repurchase.up.sql`）+ 1062 映射（`order_repository_gorm.go`）。

## Q6. 重试分类：永久失败 vs 瞬时失败

**答案要点**

- **永久失败**：数据/业务问题，重投也失败（活动不存在、库存不足）→ 死信。
- **瞬时失败**：环境问题（DB 超时、连接抖动）→ 重投值得。
- 分类集中在消费入口，用哨兵 `ErrPermanent` 标记，避免层层判断。
- 发布侧用 `retry.Do` 有限重试吸收瞬时故障；业务拒绝用 `retry.Stop` 终止重试。

**可运行代码**

```go title="interview/ch06_mq/q06_retry_classify/main.go"
package main

import (
	"errors"
	"fmt"
)

var ErrPermanent = errors.New("permanent failure")
var ErrSoldOut = errors.New("活动已抢光")
var ErrDBDown = errors.New("database timeout")

// 消费者分类：数据/业务问题 = 永久（重投也失败）；环境问题 = 瞬时（值得重投）。
func classify(err error) string {
	switch {
	case errors.Is(err, ErrPermanent), errors.Is(err, ErrSoldOut):
		return "永久 → 死信（不重投）"
	case errors.Is(err, ErrDBDown):
		return "瞬时 → 重投（requeue=true）"
	default:
		return "未知 → 按瞬时处理重投"
	}
}

func main() {
	cases := []error{ErrSoldOut, ErrDBDown, errors.New("unexpected")}
	for _, err := range cases {
		fmt.Printf("%-30v → %s\n", err, classify(err))
	}
	fmt.Println("另：发布侧用 retry.Do 有限重试吸收瞬时故障；业务拒绝用 retry.Stop 终止重试")
}

```

**项目位置**：`internal/flashsale/service/flashsale_consumer.go` 的 `classifyCreateError`（order 参数/SKU 错误与 flashsale `ErrSeckillStockInsufficient` → 永久）；`internal/platform/mq/mq.go` 定义 `ErrPermanent`。

## Q7. 熔断器：消费者自我保护（gobreaker 简化版）

**答案要点**

- 三态：关闭（全放行）→ 打开（直接拒绝）→ 半开（试探一个）。
- 连续失败达阈值 → 打开；冷却期后半开试探，成功即恢复。
- 熔断期间失败要**快速失败**（秒杀消费者：返回错误 → 消息 requeue，不阻塞）。
- 数据类错误（ErrPermanent）不计入失败数——不该让业务问题触发熔断。

**可运行代码**

```go title="interview/ch06_mq/q07_breaker/main.go"
package main

import (
	"errors"
	"fmt"
	"time"
)

type state int

const (
	stClosed   state = iota // 正常：全部放行
	stOpen                  // 熔断：直接拒绝
	stHalfOpen              // 试探：放行一个，成功即恢复
)

type breaker struct {
	state     state
	failures  int
	threshold int
	openedAt  time.Time
	cooldown  time.Duration
}

func (b *breaker) allow() bool {
	switch b.state {
	case stClosed:
		return true
	case stOpen:
		if time.Since(b.openedAt) > b.cooldown {
			b.state = stHalfOpen
			return true // 半开试探
		}
		return false
	case stHalfOpen:
		return true
	}
	return false
}

func (b *breaker) onSuccess() { b.failures = 0; b.state = stClosed }

func (b *breaker) onFailure() {
	if b.state == stHalfOpen {
		b.state = stOpen
		b.openedAt = time.Now()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.state = stOpen
		b.openedAt = time.Now()
	}
}

func main() {
	b := &breaker{threshold: 3, cooldown: 50 * time.Millisecond}
	var err error
	for i := 1; i <= 6; i++ {
		if !b.allow() {
			fmt.Printf("第 %d 次: 熔断打开，直接返回 ErrCircuitOpen（重投）\n", i)
			continue
		}
		if err = consumeErr(); err != nil {
			b.onFailure()
			fmt.Printf("第 %d 次: 消费失败，连续失败 %d 次\n", i, b.failures)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if b.allow() {
		fmt.Println("冷却期后半开试探：放行一个请求")
	}
}

func consumeErr() error { return errors.New("rabbitmq channel closed") }

```

**项目位置**：`internal/platform/mq/breaker.go` 的 `WrapCircuitBreaker`（gobreaker，配置 `mq.circuit.*`）；只包 Consume；`ErrCircuitOpen` 视为瞬时错误 requeue。
