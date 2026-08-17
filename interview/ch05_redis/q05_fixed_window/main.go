// Q5 固定窗口限流（INCR + TTL）：按用户限流。
// 运行：go run ./interview/ch05_redis/q05_fixed_window
package main

import (
	"fmt"
	"sync"
)

// 内存版固定窗口脚本：key 不存在 SET 1 + EXPIRE，存在则 INCR。
// 固定窗口的局限：窗口边界突发（第 59s 与第 61s 各放满一批）。
type windowCounter struct {
	mu     sync.Mutex
	counts map[string]int
	ttl    map[string]int
}

func (w *windowCounter) allow(key string, limit, window int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.counts[key]; !ok {
		w.counts[key] = 1
		w.ttl[key] = window
		return 1 <= limit
	}
	w.counts[key]++
	return w.counts[key] <= limit
}

func main() {
	w := &windowCounter{counts: map[string]int{}, ttl: map[string]int{}}
	key := "flashsale:rl:7" // 用户 7
	for i := 1; i <= 6; i++ {
		fmt.Printf("第 %d 次请求 → 允许=%v\n", i, w.allow(key, 5, 60))
	}
	fmt.Println("第 6 次被拒：固定窗口 60s 内最多 5 次")
}

// 项目位置：internal/platform/cache/atomic.go 的 IncrementFixedWindow 与
// internal/platform/limiter/limiter.go 的 RedisCounter.Allow；调用点为 Seckill。
// 演进：滑动窗口/令牌桶在 Redis 侧实现（BACKLOG "Redis 分布式限流"）。
