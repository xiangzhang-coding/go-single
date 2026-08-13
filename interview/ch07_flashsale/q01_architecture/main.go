// Q1 秒杀整体架构：限流 → 幂等 → 预扣 → MQ → 异步落单。
// 运行：go run ./interview/ch07_flashsale/q01_architecture
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

// 项目位置：internal/flashsale/handler/flashsale_handler.go 的 Purchase（202 排队 +
// order_no）；internal/flashsale/service/flashsale_service.go 的 Seckill；
// 消费者 internal/flashsale/service/flashsale_consumer.go；时序图 docs/DESIGN.md。
