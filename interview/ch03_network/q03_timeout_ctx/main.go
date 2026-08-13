// Q3 请求超时与 context 传播（504）。
// 运行：go run ./interview/ch03_network/q03_timeout_ctx
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 业务函数：ctx 一路传递，链路任何一处超时立即失败。
func loadActivity(ctx context.Context) error {
	select {
	case <-time.After(2 * time.Second): // 模拟慢 DB
		return nil
	case <-ctx.Done():
		return fmt.Errorf("加载活动: %w", ctx.Err())
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := loadActivity(ctx)
	fmt.Printf("耗时 %v\n", time.Since(start).Round(time.Millisecond))
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Println("→ 超时，HTTP 应返回 504（handler 将 DeadlineExceeded 映射为 504）")
	} else if err != nil {
		fmt.Println("其他错误:", err)
	}
}

// 项目位置：cmd/server/middleware.go 的 requestTimeout 为请求重建带 deadline 的
// context 并回写 c.Request；各模块 handler 的 writeError 把 context.DeadlineExceeded
// 映射为 504（如 internal/flashsale/handler/flashsale_handler.go）。
