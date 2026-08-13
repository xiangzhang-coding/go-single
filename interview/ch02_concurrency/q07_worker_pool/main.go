// Q7 生产者-消费者与优雅退出（MQ 消费循环的原型）。
// 运行：go run ./interview/ch02_concurrency/q07_worker_pool
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

// 项目位置：cmd/server/main.go 的消费者重连循环——`go func(){ for { mqClient.Consume(ctx,
// ...); time.Sleep(3s) } }()`，ctx 取消后 Consume 返回，循环退出；
// RabbitMQ 侧用 Qos(1) 天然串行，单消费者处理（internal/platform/mq/rabbitmq.go）。
