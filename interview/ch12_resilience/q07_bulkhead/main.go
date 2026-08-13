// Q7 舱壁隔离与背压：故障不扩散。
// 运行：go run ./interview/ch12_resilience/q07_bulkhead
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

// 项目位置：RabbitMQ 消费 Qos(1,0,false) 即背压实现（internal/platform/mq/rabbitmq.go）；
// WS 慢消费者"缓冲 64 满即断开"是另一种舱壁（internal/platform/ws/hub.go）；
// 显式 semaphore 舱壁列在 BACKLOG（二期能力）。
