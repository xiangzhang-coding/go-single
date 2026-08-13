// Q3 defer 执行时机与命名返回值陷阱。
// 运行：go run ./interview/ch01_go_basics/q03_defer_return
package main

import "fmt"

func order() int {
	defer fmt.Println("先 defer 的后执行")
	defer fmt.Println("后 defer 的先执行")
	return 1
}

// 命名返回值陷阱：defer 可以修改返回值。
func named() (n int) {
	n = 10
	defer func() { n++ }() // 返回前执行，n 变成 11
	return n
}

func anonymous() int {
	n := 10
	defer func() { n++ }() // 只改局部变量，返回值已在 return 时确定
	return n
}

func main() {
	order()
	fmt.Println("命名返回值 defer 生效:", named())
	fmt.Println("匿名返回值 defer 不生效:", anonymous())
}

// 项目位置：资源释放用 defer 的典型场景——internal/platform/mq/rabbitmq.go 中
// Publish/Consume 内 `defer ch.Close()`；WS client.send 通道的关闭也由 defer 保证
// （internal/platform/ws/hub.go）。
