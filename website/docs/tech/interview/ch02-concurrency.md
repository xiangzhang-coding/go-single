---
sidebar_position: 3
---

# 02 并发

## Q1. goroutine 泄漏与 context 取消

**答案要点**

- goroutine 泄漏 = 协程永久阻塞无法退出（阻塞在 channel 读、死循环等），栈与资源无法回收。
- 根治手段：**取消机制**——`context.WithCancel`/`WithTimeout`，工作协程 `select ctx.Done()`。
- 每个请求一个 ctx 并贯穿 service/repo/存储调用，超时即整链取消。
- 排查：`go tool pprof goroutine` 看阻塞栈；go.uber.org/goleak 做泄漏断言。

**可运行代码**

```go title="interview/ch02_concurrency/q01_goroutine_leak/main.go"
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func main() {
	// 反例：goroutine 永久阻塞在 channel 读上，无人取消 → 泄漏。
	// 正例：ctx 取消让 goroutine 有机会退出。

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			fmt.Println("goroutine 感知取消，正常退出")
		case <-time.After(10 * time.Second):
			fmt.Println("（永不执行）")
		}
	}()
	wg.Wait()
	fmt.Println("主流程结束")
}

```

**项目位置**：`internal/platform/health/health.go` 探测用 ctx 限时；`cmd/server/middleware.go` 的 `requestTimeout` 为请求建带 deadline 的 context 贯穿 service→repository；MQ 消费循环 `select ctx.Done()` 退出（`cmd/server/main.go`）。

## Q2. channel 缓冲与 select 多路复用

**答案要点**

- 无缓冲 channel：发送/接收同步握手；有缓冲：缓冲满才阻塞，解耦生产消费节奏。
- `select` 多路复用多个 channel（含 `ctx.Done()`、`time.After`），多个就绪随机选。
- 缓冲大小是**背压**手段：缓冲满 = 生产方受阻，可作慢消费兜底。
- `close` 后接收立即返回零值；用 `v, ok := <-ch` 判断关闭。

**可运行代码**

```go title="interview/ch02_concurrency/q02_channel_select/main.go"
package main

import (
	"fmt"
	"time"
)

func main() {
	// 有缓冲 channel：缓冲满才阻塞，解耦生产与消费节奏。
	send := make(chan string, 64)

	// 无缓冲 channel：发送与接收同步。
	handshake := make(chan struct{})

	go func() {
		// select 多路复用：等消息或等退出信号，先到先处理。
		select {
		case msg := <-send:
			fmt.Println("收到:", msg)
		case <-handshake:
			fmt.Println("收到握手信号（直接退出）")
		case <-time.After(2 * time.Second):
			fmt.Println("超时兜底")
		}
	}()

	// 缓冲未满，不会阻塞发送方。
	send <- "hello"
	fmt.Println("写入缓冲成功（非阻塞）")
	close(send)
	time.Sleep(10 * time.Millisecond)
}

```

**项目位置**：`internal/platform/ws/hub.go` 的 `client.send chan []byte`（缓冲 64）——慢消费者由"缓冲满 → 断开连接"兜底；`writePump` 用 select 同时监听发送队列与退出信号。

## Q3. WaitGroup 与并发编排

**答案要点**

- `WaitGroup`：`Add`/`Done`/`Wait` 组合；`Done` 必须在 goroutine 内保证调用（defer）。
- 收集结果用**缓冲 channel**（容量 = 任务数）避免无缓冲阻塞泄漏。
- `Wait` 后 `close(results)` 才能让 `range` 结束。
- 演进：`errgroup` 支持错误传播与 ctx 取消（首错即停）。

**可运行代码**

```go title="interview/ch02_concurrency/q03_waitgroup/main.go"
package main

import (
	"fmt"
	"sync"
)

func main() {
	// 健康检查并行探测：每个依赖一个 goroutine，全部完成后再汇总。
	deps := []string{"mysql", "redis", "rabbitmq"}
	results := make(chan string, len(deps))

	var wg sync.WaitGroup
	for _, d := range deps {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			// 模拟探测，耗时随机；真实实现见 platform/health。
			results <- fmt.Sprintf("%s ok", name)
		}(d)
	}

	wg.Wait()      // 等全部探测完成
	close(results) // 关闭后 range 才会结束
	for r := range results {
		fmt.Println(r)
	}
	fmt.Println("全部检查完成")
}

```

**项目位置**：`internal/platform/health/health.go` 的 `Check` 正是"goroutine 探测 + buffered channel 收集 + 超时兜底"；WS `Hub.Close` 也等写泵退出。

## Q4. Mutex 与 RWMutex：读多写少场景

**答案要点**

- `RWMutex`：读读不互斥、读写/写写互斥；适合读远多于写的共享表。
- 选择依据：读多写少 → RWMutex；写多或写频繁 → 普通 Mutex 更简单（锁升级成本）。
- 锁粒度：锁**数据**不锁代码；尽早释放、避免锁内做 IO。
- 延伸：`sync.Map`、原子操作、分片锁是 RWMutex 的替代。

**可运行代码**

```go title="interview/ch02_concurrency/q04_mutex_rw/main.go"
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func main() {
	// RWMutex：读读不互斥、写写/读写互斥，适合读多写少的连接表。
	var mu sync.RWMutex
	conns := map[int64]string{1: "conn-1"}

	var reads, writes atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { // 并发读者
			defer wg.Done()
			mu.RLock()
			_ = conns[1]
			mu.RUnlock()
			reads.Add(1)
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int64) { // 写者
			defer wg.Done()
			mu.Lock()
			conns[n+1] = fmt.Sprintf("conn-%d", n+1)
			mu.Unlock()
			writes.Add(1)
		}(int64(i))
	}
	wg.Wait()
	fmt.Printf("完成 读=%d 写=%d 连接数=%d\n", reads.Load(), writes.Load(), len(conns))
}

```

**项目位置**：`internal/platform/ws/hub.go` 的 `Hub` 用 `sync.RWMutex` 保护 `clients` map（`PushToUser` 读、`register/unregister` 写）；雪花 ID 同毫秒序号自旋用 `sync.Mutex`（`internal/platform/snowflake/snowflake.go`）。

## Q5. sync.Once：只执行一次

**答案要点**

- `once.Do(f)`：无论多少 goroutine 并发调用，`f` 只执行一次；之后调用直接返回。
- 用途：单例初始化、惰性加载、只关一次的清理。
- 注意：`Do` 内 panic 会标记为已执行（后续不再跑）；`f` 应避免依赖外部可变状态。
- 延伸：`sync.OnceFunc`/`OnceValue`（Go 1.21+）。

**可运行代码**

```go title="interview/ch02_concurrency/q05_once/main.go"
package main

import (
	"fmt"
	"sync"
)

var once sync.Once

func initClient() *string {
	s := "client"
	fmt.Println("初始化客户端（只应执行一次）")
	return &s
}

func main() {
	// 多个 goroutine 同时首次调用，只有一次真正执行。
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			once.Do(func() {
				c := initClient()
				_ = c
			})
		}()
	}
	wg.Wait()
	fmt.Println("完成")
}

```

**项目位置**：`internal/platform/ws/hub.go` 用 `sync.Once` 保证 `client.done` 与底层连接只关闭一次；`send` 不主动关闭，避免并发推送快照向已关闭 channel 发送而 panic。

## Q6. 原子操作 atomic：无锁计数

**答案要点**

- `atomic.Int64` 等类型化原子值：`Add`/`Load`/`Store`/`CompareAndSwap`。
- CAS 用于"读-改-写"：期望值比对，不匹配说明被并发修改，自行决定重试。
- 适合单变量计数/标志；多变量复合状态仍需锁。
- 延伸：Prometheus Counter 自带原子语义，业务计数直接打点即可。

**可运行代码**

```go title="interview/ch02_concurrency/q06_atomic/main.go"
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func main() {
	var hits atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				hits.Add(1) // 原子自增，无锁
			}
		}()
	}
	wg.Wait()
	fmt.Println("原子计数:", hits.Load())

	// CAS：读-改-写需要比较交换时。
	old := hits.Load()
	if hits.CompareAndSwap(old, old+1) {
		fmt.Println("CAS 成功")
	}
}

```

**项目位置**：令牌桶 `internal/platform/limiter/limiter.go`（x/time/rate，内部基于原子）；预扣成功/失败等业务计数落 Prometheus（`internal/platform/metrics/business.go`）。

## Q7. 生产者-消费者与优雅退出（MQ 消费循环原型）

**答案要点**

- 消费者循环结构：`select { ctx.Done() / 队列消息 }`，ctx 取消即退出，不泄漏。
- 生产端同样受 ctx 约束：不再投递已取消的上下文。
- MQ 场景：broker 自动管理消息（未确认自动重投），进程退出前无需清空队列。
- 优雅退出顺序：先停生产者/调度，再停消费者，最后关资源。

**可运行代码**

```go title="interview/ch02_concurrency/q07_worker_pool/main.go"
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	queue := make(chan int, 8)
	var wg sync.WaitGroup

	// 消费者（模拟 MQ 手动 ack 循环：ctx 取消即退出）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				fmt.Println("消费者退出")
				return
			case n, ok := <-queue:
				if !ok {
					fmt.Println("队列关闭，消费者退出")
					return
				}
				fmt.Printf("处理消息 %d\n", n)
			}
		}
	}()

	// 生产者：往队列投递。
	for i := 0; i < 10; i++ {
		select {
		case queue <- i:
		case <-ctx.Done():
			fmt.Println("生产者被取消，停止投递")
		}
	}

	wg.Wait() // 等消费者退出，保证无泄漏
	fmt.Println("优雅退出完成")
}

```

**项目位置**：`cmd/server/main.go` 消费者重连循环——`go func(){ for { mqClient.Consume(ctx,...); time.Sleep(3s) } }()`；RabbitMQ 侧 `Qos(1)` 天然串行（`internal/platform/mq/rabbitmq.go`）。
