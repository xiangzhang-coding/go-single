// Q2 channel 缓冲与 select 多路复用。
// 运行：go run ./interview/ch02_concurrency/q02_channel_select
package main

import (
	"fmt"
	"time"
)

func main() {
	// 有缓冲 channel：缓冲满才阻塞，解耦生产与消费节奏。
	send := make(chan string, 64)

	// 无缓冲 channel：发送与接收同步。
	handshake := make(chan struct{})

	go func() {
		// select 多路复用：等消息或等退出信号，先到先处理。
		select {
		case msg := <-send:
			fmt.Println("收到:", msg)
		case <-handshake:
			fmt.Println("收到握手信号（直接退出）")
		case <-time.After(2 * time.Second):
			fmt.Println("超时兜底")
		}
	}()

	// 缓冲未满，不会阻塞发送方。
	send <- "hello"
	fmt.Println("写入缓冲成功（非阻塞）")
	close(send)
	time.Sleep(10 * time.Millisecond)
}

// 项目位置：internal/platform/ws/hub.go 中 client.send chan []byte（缓冲 64）——
// 慢消费者由"缓冲满 → 断开连接"兜底；writePump 用 select 同时监听发送队列与退出信号。
