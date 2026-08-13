// Q4 Mutex 与 RWMutex：读多写少场景。
// 运行：go run ./interview/ch02_concurrency/q04_mutex_rw
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func main() {
	// RWMutex：读读不互斥、写写/读写互斥，适合读多写少的连接表。
	var mu sync.RWMutex
	conns := map[int64]string{1: "conn-1"}

	var reads, writes atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { // 并发读者
			defer wg.Done()
			mu.RLock()
			_ = conns[1]
			mu.RUnlock()
			reads.Add(1)
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int64) { // 写者
			defer wg.Done()
			mu.Lock()
			conns[n+1] = fmt.Sprintf("conn-%d", n+1)
			mu.Unlock()
			writes.Add(1)
		}(int64(i))
	}
	wg.Wait()
	fmt.Printf("完成 读=%d 写=%d 连接数=%d\n", reads.Load(), writes.Load(), len(conns))
}

// 项目位置：internal/platform/ws/hub.go 的 Hub 用 sync.RWMutex 保护 clients map
// （PushToUser 遍历是读、register/unregister 是写）；同一毫秒序号自旋用 Mutex
// （internal/platform/snowflake/snowflake.go）。
