// Q3 熔断接线：保护薄弱的下游（MQ 消费者）。
// 运行：go run ./interview/ch12_resilience/q03_breaker_config
package main

import "fmt"

type breakerSettings struct {
	name                   string
	maxConsecutiveFailures int
	interval               string
	timeout                string
}

func main() {
	// 项目配置（configs/config.yaml mq.circuit.*）。
	settings := breakerSettings{
		name:                   "mq.consume",
		maxConsecutiveFailures: 3,
		interval:               "30s",
		timeout:                "10s",
	}
	fmt.Printf("熔断器 %s：连续 %d 次失败 → 打开；%s 内不尝试；%s 后半开试探\n",
		settings.name, settings.maxConsecutiveFailures, settings.interval, settings.timeout)

	fmt.Println()
	fmt.Println("接线细节（internal/platform/mq/breaker.go）：")
	fmt.Println("  只包 Consume（Publish/Ping/Close 直通——发布失败要靠重试而非熔断）")
	fmt.Println("  ErrCircuitOpen 视为瞬时错误 → 消息 requeue 重投")
	fmt.Println("  ErrPermanent 不计入失败（数据问题不该触发熔断）")
}

// 项目位置：internal/platform/mq/breaker.go 的 WrapCircuitBreaker（gobreaker）；
// 装饰器栈 cmd/server/main.go 221-230（raw → metrics.WrapMQ → WrapCircuitBreaker）；
// 配置 mq.circuit.* 见 internal/platform/config/config.go。
