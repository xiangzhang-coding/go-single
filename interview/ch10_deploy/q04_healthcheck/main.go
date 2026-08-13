// Q4 就绪探针：服务真的能干活才算健康。
// 运行：go run ./interview/ch10_deploy/q04_healthcheck
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type dependency struct {
	name string
	err  error
}

// 并发探测各依赖（与 platform/health 同构：goroutine + buffered channel + 超时）。
func check(ctx context.Context) (map[string]bool, bool) {
	deps := []dependency{{name: "mysql"}, {name: "redis"}, {name: "rabbitmq"}}
	results := make(chan struct {
		string
		bool
	}, len(deps))

	var wg sync.WaitGroup
	for _, d := range deps {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			time.Sleep(20 * time.Millisecond) // 模拟探测
			results <- struct {
				string
				bool
			}{n, d.err == nil}
		}(d.name)
	}
	go func() { wg.Wait(); close(results) }()

	ok := true
	state := map[string]bool{}
	select {
	case <-ctx.Done():
		return nil, false
	default:
	}
	for r := range results {
		state[r.string] = r.bool
		if !r.bool {
			ok = false
		}
	}
	return state, ok
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, ok := check(ctx)
	fmt.Printf("状态: %v → 探针返回 %v（任一依赖挂 → 503 从负载均衡摘除）\n", state, ok)
	_ = errors.New("unused")
}

// 项目位置：internal/platform/health/health.go 的 Check + GET /healthz（main.go
// healthHandler 395-404）；compose 各服务 healthcheck 与 nginx 反代均依赖探针，
// 探针失败容器标记 unhealthy 并重启。
