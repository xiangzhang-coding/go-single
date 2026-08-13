// Q3 优雅重启与配置热加载：SIGHUP 语义。
// 运行：go run ./interview/ch10_deploy/q03_graceful_reload
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func loadConfig() map[string]string {
	return map[string]string{"flashsale.token_bucket.qps": "20"}
}

func main() {
	// 运维惯例：SIGTERM 优雅停机，SIGHUP 重载配置/日志文件。
	// 本项目只实现 SIGTERM 优雅关闭（main.go signal.Notify）；
	// SIGHUP 重载留作扩展，viper 的 WatchConfig 是备选。
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM)

	cfg := loadConfig()
	fmt.Println("当前配置:", cfg)

	go func() {
		for sig := range ch {
			switch sig {
			case syscall.SIGHUP:
				cfg = loadConfig() // 重读配置
				fmt.Println("SIGHUP: 配置已重载", cfg)
			case syscall.SIGTERM:
				fmt.Println("SIGTERM: 优雅退出（处理在途请求）")
				os.Exit(0)
			}
		}
	}()
	select {} // 用 Ctrl+C 发 SIGINT 结束演示
}

// 项目位置：cmd/server/main.go——signal.Notify(quit, os.Interrupt, syscall.SIGTERM) →
// cronRegistry.Stop(5s) → srv.Shutdown(10s) → wsHub.Close()；docker 停止容器时发
// SIGTERM 走此路径；compose healthcheck 探 /healthz 而非仅进程存活。
