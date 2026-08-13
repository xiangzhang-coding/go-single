// Q5 sync.Once：只执行一次。
// 运行：go run ./interview/ch02_concurrency/q05_once
package main

import (
	"fmt"
	"sync"
)

var once sync.Once

func initClient() *string {
	s := "client"
	fmt.Println("初始化客户端（只应执行一次）")
	return &s
}

func main() {
	// 多个 goroutine 同时首次调用，只有一次真正执行。
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			once.Do(func() {
				c := initClient()
				_ = c
			})
		}()
	}
	wg.Wait()
	fmt.Println("完成")
}

// 项目位置：internal/platform/ws/hub.go 用 sync.Once 保证 client.send 只被 close 一次
// （重复 close 会 panic）；同一模式可用于单例客户端初始化。
