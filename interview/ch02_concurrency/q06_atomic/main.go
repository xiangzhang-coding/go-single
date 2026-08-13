// Q6 原子操作 atomic：无锁计数。
// 运行：go run ./interview/ch02_concurrency/q06_atomic
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

func main() {
	var hits atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				hits.Add(1) // 原子自增，无锁
			}
		}()
	}
	wg.Wait()
	fmt.Println("原子计数:", hits.Load())

	// CAS：读-改-写需要比较交换时。
	old := hits.Load()
	if hits.CompareAndSwap(old, old+1) {
		fmt.Println("CAS 成功")
	}
}

// 项目位置：internal/platform/limiter/limiter.go 的令牌桶用 x/time/rate（内部基于原子）；
// 业务侧高并发计数（预扣成功/失败）落 Prometheus Counter（平台自带原子语义），
// 见 internal/platform/metrics/business.go。
