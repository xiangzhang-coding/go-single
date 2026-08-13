// Q6 健康检查端点：存活/就绪与聚合状态。
// 运行：go run ./interview/ch11_observability/q06_healthz
package main

import (
	"context"
	"fmt"
	"time"
)

// 探活函数族：每个依赖带超时，聚合出整体健康。
func probeMySQL(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil
	}
}

func main() {
	// 整体健康 = 全依赖健康；任一超时 → 503 + checks 明细（healthHandler 输出）。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	healthy := true
	for name, probe := range map[string]func(context.Context) error{
		"mysql": probeMySQL, "redis": probeMySQL, "mq": probeMySQL,
	} {
		if err := probe(ctx); err != nil {
			fmt.Printf("checks.%s = down（%v）\n", name, err)
			healthy = false
		} else {
			fmt.Printf("checks.%s = up\n", name)
		}
	}
	fmt.Println("整体 status:", map[bool]string{true: "ok(200)", false: "unavailable(503)"}[healthy])
}

// 项目位置：internal/platform/health/health.go（并发探测 + buffered channel + 2s 超时）、
// cmd/server/main.go 的 GET /healthz（395-404 返回 {"status","checks"}）；
// compose healthcheck 与 Prometheus 拉取同源复用。
