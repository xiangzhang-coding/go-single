// Q5 异步受理 + 轮询：202 与最终一致。
// 运行：go run ./interview/ch12_resilience/q05_queue_poll
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

// 项目位置：internal/flashsale/handler/flashsale_handler.go 返回 202 排队 + order_no；
// 前端秒杀页轮询实现见 web/faire 秒杀页（26822e 提交）；异步落单
// flashsale_consumer.go；对账兜底保证"有预扣必有订单"。
