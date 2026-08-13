// Q4 死信队列（DLQ）：坏消息有去处。
// 运行：go run ./interview/ch06_mq/q04_dlq
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

// 项目位置：internal/platform/mq/rabbitmq.go 的 declareQueue——主队列 durable +
// x-dead-letter-exchange:"" + x-dead-letter-routing-key: <主队列>.dlq；
// 秒杀消费者把"活动不存在/无地址/库存不足"归为永久失败（flashsale_consumer.go 的 permanent）。
