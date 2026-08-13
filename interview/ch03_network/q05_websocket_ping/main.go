// Q5 WebSocket 心跳与读写泵：writePump 模式。
// 运行：go run ./interview/ch03_network/q05_websocket_ping
package main

import (
	"fmt"
	"time"
)

// 模拟 WS 客户端：send 缓冲 + 退出信号 + 心跳写泵。
type client struct {
	send chan []byte
	done chan struct{}
}

func newClient() *client {
	return &client{send: make(chan []byte, 64), done: make(chan struct{})}
}

// writePump：select 同时等发送队列、退出信号与心跳时钟。
func (c *client) writePump() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case msg := <-c.send:
			fmt.Println("写帧:", string(msg))
		case <-ticker.C:
			fmt.Println("写帧: Ping（保活探测对端存活）")
		case <-c.done:
			fmt.Println("退出写泵")
			return
		}
	}
}

func main() {
	c := newClient()
	go c.writePump()

	c.send <- []byte("hello")
	time.Sleep(250 * time.Millisecond)
	close(c.done) // 触发写泵退出
	time.Sleep(10 * time.Millisecond)
}

// 项目位置：internal/platform/ws/hub.go——client.send 缓冲 64、writePump 用 select
// 监听发送/心跳（Ping 100s）/退出；慢消费者缓冲满即断开（hub.go PushToUser）。
// 消息实时推送链路：chat service → wsMessageNotifier → hub.PushToUser（cmd/server/main.go）。
