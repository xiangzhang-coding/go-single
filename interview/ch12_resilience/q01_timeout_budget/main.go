// Q1 超时预算：分层超时，故障快速失败不拖垮链路。
// 运行：go run ./interview/ch12_resilience/q01_timeout_budget
package main

import (
	"context"
	"fmt"
	"time"
)

func main() {
	// 项目的分层超时（故障逐层截断，错误向上快速传播）。
	fmt.Println("请求级 5s   requestTimeout 中间件（每请求 context，504）")
	fmt.Println("消息级 15s  MQ 单条消息处理超时（rabbitmq.go msgTimeout）")
	fmt.Println("任务级 5min cron 单次执行超时（registry.go per-run timeout）")
	fmt.Println("探活级 2s   health 探测各依赖")
	fmt.Println("退避可取消  retry.sleep 对 ctx 敏感，超时即停不再重试")

	// 演示：子任务超时传播。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	select {
	case <-ctx.Done():
		fmt.Printf("链路超时快速失败，耗时 %v\n", time.Since(start).Round(time.Millisecond))
	}
}

// 项目位置：cmd/server/middleware.go 的 requestTimeout；internal/platform/mq/rabbitmq.go
// 的 msgTimeout；internal/platform/cron/registry.go；health 2s（internal/platform/health）。
