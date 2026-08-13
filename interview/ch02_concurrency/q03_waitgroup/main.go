// Q3 WaitGroup 与并发编排。
// 运行：go run ./interview/ch02_concurrency/q03_waitgroup
package main

import (
	"fmt"
	"sync"
)

func main() {
	// 健康检查并行探测：每个依赖一个 goroutine，全部完成后再汇总。
	deps := []string{"mysql", "redis", "rabbitmq"}
	results := make(chan string, len(deps))

	var wg sync.WaitGroup
	for _, d := range deps {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			// 模拟探测，耗时随机；真实实现见 platform/health。
			results <- fmt.Sprintf("%s ok", name)
		}(d)
	}

	wg.Wait()        // 等全部探测完成
	close(results)   // 关闭后 range 才会结束
	for r := range results {
		fmt.Println(r)
	}
	fmt.Println("全部检查完成")
}

// 项目位置：internal/platform/health/health.go 的 Check 正是"goroutine 探测 +
// buffered channel 收集 + 超时兜底"的结构；WS Hub 的 Close 也等写泵退出。
