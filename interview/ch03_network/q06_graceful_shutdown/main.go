// Q6 HTTP 服务优雅关闭。
// 运行：go run ./interview/ch03_network/q06_graceful_shutdown
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	srv := &http.Server{Addr: ":18080"}

	go func() {
		_ = srv.ListenAndServe()
	}()

	// 监听退出信号；Ctrl+C 后走优雅关闭路径。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	fmt.Println("收到退出信号，开始优雅关闭")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Println("关闭超时/出错:", err)
	}
	fmt.Println("在途请求已处理完，服务退出")
}

// 项目位置：cmd/server/main.go——SIGINT/SIGTERM → cronRegistry.Stop(5s) →
// srv.Shutdown(ctx 10s) → defer wsHub.Close()（WS 连接不在 http.Server 管辖内）；
// MQ 消费者经 ctx 取消退出。
