// Q2 发布确认（Publisher Confirm）：消息真的到 broker 才放心。
// 运行：go run ./interview/ch06_mq/q02_publisher_confirm
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

// 项目位置：internal/platform/mq/rabbitmq.go 的 Publish——ch.Confirm(false) +
// PublishWithDeferredConfirm + conf.WaitContext(ctx)；确认超时（ctx 到点）视为未送达，
// 秒杀侧保留幂等键并靠对账兜底（flashsale_service.go publishSeckillSuccess）。
