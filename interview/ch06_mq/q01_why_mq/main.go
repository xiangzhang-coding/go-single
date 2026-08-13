// Q1 为什么用消息队列：削峰填谷与解耦。
// 运行：go run ./interview/ch06_mq/q01_why_mq
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

// 项目位置：秒杀预扣成功 → 发布 MQ（flashsale.order.create）→ 消费者串行落单
//（cmd/server/main.go 消费者重连循环 + internal/flashsale/service/flashsale_consumer.go）；
// 队列定义 SeckillOrderQueue 在 flashsale_service.go。
