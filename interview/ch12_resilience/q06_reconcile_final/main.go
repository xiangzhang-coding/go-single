// Q6 最终一致兜底：每类故障都有对账/补偿路径。
// 运行：go run ./interview/ch12_resilience/q06_reconcile_final
package main

import "fmt"

func main() {
	// 秒杀链路的故障矩阵 → 兜底机制。
	type row struct{ failure, fallback string }
	table := []row{
		{"MQ 发布失败（预扣已成功）", "保留幂等键 + 对账补单（有预扣无订单 → 回补/补单信号）"},
		{"消费者 DB 瞬时故障", "Nack requeue 重投，at-least-once"},
		{"消息数据问题（活动不存在）", "永久失败进 DLQ，对账/人工补偿消费"},
		{"Redis 回补失败（取消订单后）", "对账 cron 以 MySQL 为准对齐 Redis"},
		{"结束 30min 后 Redis key 残留/丢失", "ReconcileEnded 以 MySQL 为准 SET 对齐"},
		{"重复消息/重复提交", "唯一键 + 幂等键（两套去重）"},
	}
	for _, r := range table {
		fmt.Printf("%-28s → %s\n", r.failure, r.fallback)
	}
	fmt.Println("原则：主链路只保证快速受理，最终一致由对账兜底（每小时/每分钟 cron）")
}

// 项目位置：internal/flashsale/service/reconciliation.go（diffActive/ReconcileEnded）、
// cmd/server/main.go 对账 cron 注册（450-487）、flashsale_consumer.go 错误分类、
// mq declareQueue 死信配置。
