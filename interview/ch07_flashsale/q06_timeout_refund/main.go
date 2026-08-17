// Q6 超时未支付回补：取消订单 + 回补 Redis/MySQL 库存。
// 运行：go run ./interview/ch07_flashsale/q06_timeout_refund
package main

import "fmt"

type state struct {
	redisStock  int
	mysqlStock  int
	orderPaid   bool
	orderStatus string
}

// 超时取消流程：事务内条件取消 + 回补 MySQL → 提交后经 RestoreFlashSale 回补 Redis。
func timeoutCancel(s *state) {
	if s.orderStatus != "pending_payment" {
		return // 条件更新不命中：订单已支付，跳过
	}
	s.orderStatus = "cancelled"
	s.mysqlStock++ // 事务内回补 MySQL（与订单取消同事务）
	s.redisStock++ // 提交后缓存适配器原子执行：INCR 库存 + DECR 计数 + DEL 幂等键
	s.orderPaid = false
}

func main() {
	s := &state{redisStock: 0, mysqlStock: 0, orderStatus: "pending_payment"}
	fmt.Println("下单后 10min 未支付，cron 每分钟扫描一次:")
	timeoutCancel(s)
	fmt.Printf("取消后：订单=%s redis=%d mysql=%d（库存回补，用户可再次抢购）\n",
		s.orderStatus, s.redisStock, s.mysqlStock)
}

// 项目位置：internal/flashsale/service/seckill_timeout.go——调用 order.ListExpiredSeckill
// → 同事务 order.CancelSeckill + 活动仓储 RestoreStock → 提交后 RestoreRedis；
// cron 任务 seckill-timeout-cancel 每分钟（cmd/server/main.go）。
