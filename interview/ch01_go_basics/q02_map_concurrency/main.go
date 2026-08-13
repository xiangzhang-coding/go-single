// Q2 map 的并发安全与正确姿势。
// 运行：go run ./interview/ch01_go_basics/q02_map_concurrency
package main

import (
	"fmt"
	"sync"
)

func main() {
	// 错误姿势：并发写 map 会触发 fatal error: concurrent map writes（进程崩溃）。
	// 正确姿势一：互斥锁保护。
	var mu sync.RWMutex
	counts := map[string]int{}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mu.Lock()
				counts["hit"]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Println("互斥锁保护后的计数:", counts["hit"])

	// 正确姿势二：sync.Map（读多写少或键集固定的场景）。
	var hits sync.Map
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := hits.LoadOrStore("hit", 0)
			hits.Store("hit", v.(int)+1)
		}()
	}
	wg.Wait()
	v, _ := hits.Load("hit")
	fmt.Println("sync.Map 计数:", v)
}

// 项目位置：internal/platform/ws/hub.go 的 Hub 用 sync.RWMutex 保护 clients map
// （WS 连接集合，写少读多）；商品详情缓存等用 Redis 而非进程内 map。
