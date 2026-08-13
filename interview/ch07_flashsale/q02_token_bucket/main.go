// Q2 令牌桶限流：平滑突发流量。
// 运行：go run ./interview/ch07_flashsale/q02_token_bucket
package main

import (
	"fmt"
	"time"
)

// 令牌桶：桶容量 burst，按 rate 恒定补充；有令牌才放行。
// 与 internal/platform/limiter 用 x/time/rate 的实现语义一致。
type tokenBucket struct {
	rate   float64 // 每秒补充令牌数
	burst  int
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: burst, tokens: float64(burst), last: time.Now()}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > float64(b.burst) {
		b.tokens = float64(b.burst)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func main() {
	b := newTokenBucket(2, 5) // 2 QPS，桶 5
	allowed := 0
	for i := 1; i <= 10; i++ {
		if b.allow() {
			allowed++
			fmt.Printf("请求 %2d 放行\n", i)
		} else {
			fmt.Printf("请求 %2d 限流 429\n", i)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("突发 5 个被桶接住，随后按 2/s 节奏放行（共放行 %d）\n", allowed)
}

// 项目位置：internal/platform/limiter/limiter.go 的 NewTokenBucket（x/time/rate），
// 只挂在 POST /api/flashsales/:id/purchase 上（flashsale_handler.go），QPS 配置见
// configs/config.yaml 的 flashsale.token_bucket。
