// Q2 指数退避 + 抖动：重试不再火上浇油。
// 运行：go run ./interview/ch12_resilience/q02_backoff
package main

import (
	"fmt"
	"time"
)

// 与 retry.backoff 同构：InitialBackoff × 2^attempt，封顶 MaxBackoff，叠加 [0,Jitter)。
func backoff(initial, max time.Duration, attempt int) time.Duration {
	d := initial
	for i := 0; i < attempt; i++ {
		d *= 2
		if d > max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

func main() {
	cfg := struct {
		attempts, initial, max time.Duration
	}{3, 100 * time.Millisecond, time.Second}

	for attempt := 0; attempt < int(cfg.attempts); attempt++ {
		fmt.Printf("第 %d 次失败后的等待: %v（封顶 %v；抖动避免惊群）\n",
			attempt+1, backoff(cfg.initial, cfg.max, attempt), cfg.max)
	}
	fmt.Println("使用约束：只有幂等操作才允许重试（retry.Do 包注释）")
}

// 项目位置：internal/platform/retry/retry.go——backoff（86-102）+ sleep（105-114，
// 可被 ctx 取消）；retry.Stop 标记业务拒绝不重试；启用点：order Create（265-276）、
// flashsale publishSeckillSuccess（520）、payment MockPay（84-92）。
