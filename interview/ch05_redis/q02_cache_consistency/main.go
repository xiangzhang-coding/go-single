// Q2 缓存一致性：Cache-Aside 与"删缓存 vs 更新缓存"。
// 运行：go run ./interview/ch05_redis/q02_cache_consistency
package main

import (
	"fmt"
	"sync"
)

type store struct {
	mu    sync.Mutex
	db    map[string]string
	cache map[string]string
	log   []string
}

func (s *store) writeThrough(id, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db[id] = v
	// 写库后【删除】缓存而非"更新"缓存：
	// 更新会引入并发写顺序问题，删除则让下一次读自然回填新值。
	delete(s.cache, id)
	s.log = append(s.log, fmt.Sprintf("DB 写入 %s=%s，缓存删除", id, v))
}

func (s *store) read(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cache[id]; ok {
		return v
	}
	v := s.db[id]
	s.cache[id] = v // 回填
	return v
}

func main() {
	s := &store{db: map[string]string{"p1": "旧价"}, cache: map[string]string{"p1": "旧价"}}
	s.writeThrough("p1", "新价") // 管理端改价：先 DB 后删缓存
	fmt.Println("读:", s.read("p1"), "| 日志:", s.log)
	fmt.Println("要点：Cache-Aside 下写路径 = 更新 DB + 删除缓存（而非更新缓存）")
}

// 项目位置：product 详情走 Cache-Aside（product_service.go GetDetail 回填）；
// 秒杀库存的写路径特殊——Redis 是事实源（预扣），不适用本模式；对账回写见
// reconciliation.go。库存缓存键 product:detail:{id} TTL 5min 兜底一致性窗口。
