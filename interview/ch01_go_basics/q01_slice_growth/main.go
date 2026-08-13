// Q1 slice 的扩容机制与底层数组。
// 运行：go run ./interview/ch01_go_basics/q01_slice_growth
package main

import "fmt"

func main() {
	// append 触发扩容时，cap 增长策略：<1024 翻倍，>=1024 约 1.25 倍（不保证，是实现细节）。
	var s []int
	for i := 0; i < 5; i++ {
		s = append(s, i)
		fmt.Printf("len=%d cap=%d\n", len(s), cap(s))
	}

	// 子切片共享底层数组：对 s2 的修改会影响 s。
	s2 := s[:2]
	s2[0] = 100
	fmt.Println("共享底层数组:", s[0] == 100)

	// 扩容后不再共享：append 超过 cap 会分配新数组。
	s3 := append(s, 99)
	s3[0] = -1
	fmt.Println("扩容后不共享:", s[0] == 100)
}

// 项目位置：无直接对应；切片/数组是语言基础，购物车条目列表、订单项列表等多用 slice 承载
// （如 internal/order/service 中 order_items []model.OrderItem）。
