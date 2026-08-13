// Q4 幂等键生命周期：何时保留、何时释放。
// 运行：go run ./interview/ch07_flashsale/q04_idem_key
package main

import "fmt"

func main() {
	// 幂等键先于预扣抢占：挡得住并发重复提交。
	// 预扣结果决定键的去留（30min TTL 兜底自动清理）。
	fmt.Println("预扣成功           → 保留幂等键（直到订单落库/对账）")
	fmt.Println("业务拒绝(抢光/限购) → 释放幂等键（允许窗口内再试一次）")
	fmt.Println("基础设施失败(网络)  → 保留幂等键（防瞬时故障下重复预扣）")
	fmt.Println("MQ 发布失败         → 保留幂等键（对账兜底补单，不重复扣）")

	// 关键洞察：业务拒绝释放 = 用户可重试；基础失败保留 = 用户被"挡住"
	// 但不会扣两次。幂等键语义 = "本次请求是否已处理过"。
	fmt.Println()
	fmt.Println("对比：下单幂等键 order:idem:{user}:{client_request_id} TTL 15min")
}

// 项目位置：internal/flashsale/service/flashsale_service.go 的 Seckill（473-480 抢占、
// isBusinessReject 529-533 释放）；订单侧幂等键 order_service.go 72-83；
// restoreScript 在回补时 DEL 幂等键，允许取消后再次抢购。
