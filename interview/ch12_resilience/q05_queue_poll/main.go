// Q5 异步受理 + 轮询：202 与最终一致。
// 运行：go run ./interview/ch12_resilience/q05_queue_poll
package main

import (
	"fmt"
	"time"
)

type task struct {
	status string // pending_publish → pending_order → ordered / rolled_back
}

func main() {
	// 秒杀抢购：预扣成功立即返回 202，落单异步进行；前端轮询预扣生命周期。
	t := &task{status: "pending_publish"}

	fmt.Println("POST /api/flashsales/:id/purchase → 202 {\"status\":\"queued\",\"pre_deduction_id\":\"42\",\"pre_deduction_status\":\"pending_publish\"}")

	go func() { // 消费者异步落单
		time.Sleep(150 * time.Millisecond)
		t.status = "pending_order"
		time.Sleep(150 * time.Millisecond)
		t.status = "ordered"
	}()

	for i := 0; i < 8; i++ { // 前端 1.5s×30 轮询
		time.Sleep(100 * time.Millisecond)
		fmt.Printf("轮询 GET /api/flashsales/purchases/42 → %s\n", t.status)
		if t.status == "ordered" || t.status == "rolled_back" {
			fmt.Println("→ 生命周期已终结：已落单则跳转详情，已回退则提示失败")
			break
		}
	}
}

// 项目位置：internal/flashsale/handler/flashsale_handler.go 返回 202 + pre_deduction_id；
// 前端秒杀页轮询实现见 web/faire；异步落单
// flashsale_consumer.go；对账兜底保证"有预扣必有订单"。
