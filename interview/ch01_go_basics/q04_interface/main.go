// Q4 接口的鸭子类型与动态派发。
// 运行：go run ./interview/ch01_go_basics/q04_interface
package main

import "fmt"

// 接口由使用方定义（调用方声明最小接口）：消费者只需自己需要的子集。
type StockSource interface {
	Remaining() int
}

type redisStock struct{ remaining int }

func (r redisStock) Remaining() int { return r.remaining }

// 空接口接收任意值，取值时需要类型断言。
func describe(v any) string {
	switch t := v.(type) {
	case int:
		return fmt.Sprintf("int:%d", t)
	case string:
		return fmt.Sprintf("string:%q", t)
	default:
		return "other"
	}
}

func main() {
	var src StockSource = redisStock{remaining: 42}
	fmt.Println("接口动态派发:", src.Remaining())

	var v any = "hello"
	if s, ok := v.(string); ok {
		fmt.Println("类型断言成功:", s)
	}
	fmt.Println(describe(123), describe("hi"), describe(1.5))
}

// 项目位置：模块间经 service 接口进程内调用——flashsale 消费者声明最小
// OrderService 接口，由 order 实现；依赖保持 flashsale → order 单向；
// 编译期断言 var _ Repository = (*GORMRepo)(nil) 遍布各模块 repository。
