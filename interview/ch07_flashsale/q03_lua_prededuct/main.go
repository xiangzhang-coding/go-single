// Q3 Lua 原子预扣：一次调用完成全部校验与扣减。
// 运行：go run ./interview/ch07_flashsale/q03_lua_prededuct
package main

import "fmt"

// 预扣返回码（与项目 preDeductScript 一致）：1 成功 / 0 抢光 / -1 窗口外 / -2 超限购 / -3 下架。
func preDeduct(status string, now, start, end, stock, limit, count int) int {
	if status != "on_sale" {
		return -3
	}
	if now < start || now > end {
		return -1
	}
	if stock < 1 {
		return 0
	}
	if count+1 > limit {
		return -2
	}
	return 1
}

func main() {
	cases := []struct {
		name   string
		status string
		stock  int
		count  int
		now    int
	}{
		{"正常抢购", "on_sale", 10, 0, 100},
		{"库存为 0", "on_sale", 0, 0, 100},
		{"未开始", "on_sale", 10, 0, -10},
		{"已结束", "on_sale", 10, 0, 300},
		{"超过限购", "on_sale", 10, 1, 100},
		{"已下架", "off_sale", 10, 0, 100},
	}
	for _, c := range cases {
		code := preDeduct(c.status, c.now, 0, 200, c.stock, 1, c.count)
		msg := map[int]string{1: "成功 DECR 库存 + INCR 计数", 0: "抢光 ErrSoldOut", -1: "窗口外 ErrNotInWindow", -2: "超限购 ErrLimitReached", -3: "下架 ErrOffline"}[code]
		fmt.Printf("%-10s → %2d（%s）\n", c.name, code, msg)
	}
}

// 项目位置：internal/flashsale/service/flashsale_service.go 的 preDeductScript（91-109）
// 与 PreDeduct（569-611）；返回码映射哨兵错误；成功/失败打点 seckill_prededuct_total。
