// Q6 重试分类：永久失败 vs 瞬时失败。
// 运行：go run ./interview/ch06_mq/q06_retry_classify
package main

import (
	"errors"
	"fmt"
)

var ErrPermanent = errors.New("permanent failure")
var ErrSoldOut = errors.New("活动已抢光")
var ErrDBDown = errors.New("database timeout")

// 消费者分类：数据/业务问题 = 永久（重投也失败）；环境问题 = 瞬时（值得重投）。
func classify(err error) string {
	switch {
	case errors.Is(err, ErrPermanent), errors.Is(err, ErrSoldOut):
		return "永久 → 死信（不重投）"
	case errors.Is(err, ErrDBDown):
		return "瞬时 → 重投（requeue=true）"
	default:
		return "未知 → 按瞬时处理重投"
	}
}

func main() {
	cases := []error{ErrSoldOut, ErrDBDown, errors.New("unexpected")}
	for _, err := range cases {
		fmt.Printf("%-30v → %s\n", err, classify(err))
	}
	fmt.Println("另：发布侧用 retry.Do 有限重试吸收瞬时故障；业务拒绝用 retry.Stop 终止重试")
}

// 项目位置：internal/flashsale/service/flashsale_consumer.go 的 classifyCreateError——
// ErrInvalidInput/ErrSeckillStockInsufficient/ErrSKUNotFound/ErrSKUUnavailable → 永久，
// 其余瞬时；internal/platform/mq/mq.go 定义 ErrPermanent 哨兵。
