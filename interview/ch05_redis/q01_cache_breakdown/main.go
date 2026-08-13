// Q1 缓存击穿/穿透/雪崩 与 缓存回填。
// 运行：go run ./interview/ch05_redis/q01_cache_breakdown
package main

import (
	"fmt"
	"sync"
)

// 缓存 + 回填：击穿是"热点 key 过期瞬间大量请求直击 DB"。
// 本 demo 用"单飞（singleflight）"思想：同 key 只放一个请求去 DB 回填。
type detailCache struct {
	mu       sync.Mutex
	data     map[string]string
	inflight map[string]*sync.WaitGroup
}

func (c *detailCache) getOrLoad(key string, load func() string) string {
	c.mu.Lock()
	if v, ok := c.data[key]; ok {
		c.mu.Unlock()
		return v // 命中缓存
	}
	// 击穿防护：同 key 只有一个回填者，其余等待。
	if wg, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		wg.Wait()
		c.mu.Lock()
		v := c.data[key]
		c.mu.Unlock()
		return v
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.inflight[key] = wg
	c.mu.Unlock()

	v := load() // DB 查询
	c.mu.Lock()
	c.data[key] = v
	delete(c.inflight, key)
	wg.Done()
	c.mu.Unlock()
	return v
}

func main() {
	c := &detailCache{data: map[string]string{}, inflight: map[string]*sync.WaitGroup{}}
	loads := 0
	load := func() string {
		loads++
		return "product-detail"
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ { // 10 并发读同一未命中 key
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.getOrLoad("p1", load) }()
	}
	wg.Wait()
	fmt.Printf("10 个并发请求，DB 只回填 %d 次\n", loads)
	fmt.Println("应对雪崩：过期时间加随机抖动，避免同一时刻集体过期")
}

// 项目位置：internal/product/service/product_service.go 的 GetDetail——缓存 miss 后
// 查 DB 并回填（product:detail:{id}，TTL 5min）；防击穿/单飞留作演进（BACKLOG）。
