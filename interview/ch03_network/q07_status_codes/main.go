// Q7 REST 状态码语义：202/409/429/504 在秒杀链路中的含义。
// 运行：go run ./interview/ch03_network/q07_status_codes
package main

import "fmt"

func main() {
	// 秒杀抢购各失败分支 → HTTP 状态码映射。
	type outcome struct {
		phase string
		code  int
		why   string
	}

	table := []outcome{
		{"令牌桶限流", 429, "全局 QPS 超限，让客户端退避重试"},
		{"幂等键冲突", 409, "同用户同活动重复提交"},
		{"预扣成功", 202, "排队中：异步落单，轮询订单号"},
		{"活动抢光", 409, "Lua 返回 0 → ErrSoldOut"},
		{"窗口外/下架", 409, "ErrNotInWindow / ErrOffline"},
		{"超过限购", 409, "ErrLimitReached（每人限购）"},
		{"链路超时", 504, "context.DeadlineExceeded"},
	}
	for _, o := range table {
		fmt.Printf("%-14s → %d（%s）\n", o.phase, o.code, o.why)
	}

	// 下单/支付等其他语义：201 创建成功、404 订单不存在、403 越权访问。
	fmt.Println("202 = 异步受理；204 = 预检/删除无内容；409 = 状态冲突；429 = 限流；504 = 网关超时")
}

// 项目位置：internal/flashsale/handler/flashsale_handler.go 的 purchase 返回 202
// {"status":"queued","order_no":...}；writeError 统一映射（flashsale_handler.go 197-217，
// order_handler.go 209-228）；限流 429 在 limiter 中间件。
