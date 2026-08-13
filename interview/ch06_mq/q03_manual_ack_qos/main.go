// Q3 手动 ACK 与 QoS 预取：消费三态。
// 运行：go run ./interview/ch06_mq/q03_manual_ack_qos
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

// 项目位置：internal/platform/mq/rabbitmq.go 的 consumeOne 三态分类（nil→Ack、
// ErrPermanent→Nack(false,false)、其余→Nack(false,true)）；Qos(1,0,false) 在 Consume。
