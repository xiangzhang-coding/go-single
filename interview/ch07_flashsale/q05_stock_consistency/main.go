// Q5 库存三方一致性：Redis 预扣 vs MySQL 库存 vs 有效订单数。
// 运行：go run ./interview/ch07_flashsale/q05_stock_consistency
package main

import "fmt"

func main() {
	// 活动结束后的对账（ReconcileEnded）：以 MySQL 为准对齐 Redis。
	// 进行中对账（ReconcileActive）：只告警不写回。
	type snapshot struct {
		redis  int
		mysql  int
		orders int
		action string
	}
	rows := []snapshot{
		{50, 50, 0, "一致，无需处理"},
		{45, 50, 5, "一致：预扣 5 + 落单 5 = 一致"},
		{45, 50, 3, "异常：预扣 5 但只落 3 单 → 有预扣无订单，补单/回补信号"},
		{48, 50, 2, "一致：预扣 2 落单 2"},
	}
	for _, r := range rows {
		ok := r.redis+r.orders == r.mysql
		fmt.Printf("redis=%2d mysql=%2d 订单=%2d → %-40s %v\n",
			r.redis, r.mysql, r.orders, r.action, ok)
	}
	fmt.Println("结束 30min 内：以 MySQL 为准 SET 对齐 Redis（key 缺失回建）；下架活动不回建")
}

// 项目位置：internal/flashsale/service/reconciliation.go——diffActive（96-132）、
// ReconcileEnded（141-177，endedReconcileWindow=30min）；cron 注册在 cmd/server/main.go
//（flashsale-reconcile-active 每小时 / flashsale-reconcile-ended 每分钟）。
