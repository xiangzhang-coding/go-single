// Q1 goroutine 泄漏与 context 取消。
// 运行：go run ./interview/ch02_concurrency/q01_goroutine_leak
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func main() {
	// 反例：goroutine 永久阻塞在 channel 读上，无人取消 → 泄漏。
	// 正例：ctx 取消让 goroutine 有机会退出。

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			fmt.Println("goroutine 感知取消，正常退出")
		case <-time.After(10 * time.Second):
			fmt.Println("（永不执行）")
		}
	}()
	wg.Wait()
	fmt.Println("主流程结束")
}

// 项目位置：internal/platform/health/health.go 的 Check 用 ctx 限时探测各依赖；
// cmd/server/middleware.go 的 requestTimeout 中间件为每个请求建带 deadline 的 context，
// 贯穿 service → repository；MQ 消费循环 select ctx.Done() 退出（main.go 消费者重连循环）。
