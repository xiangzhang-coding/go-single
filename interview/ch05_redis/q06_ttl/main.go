// Q6 TTL 与过期策略：为什么给 key 设计生命周期。
// 运行：go run ./interview/ch05_redis/q06_ttl
package main

import (
	"fmt"
	"time"
)

type redisKey struct {
	value   string
	expires time.Time
}

// 惰性删除：读取时发现过期即删（访问才淘汰，省资源）。
func get(k *redisKey) (string, bool) {
	if k == nil {
		return "", false
	}
	if time.Now().After(k.expires) {
		return "", false // 惰性删除 + 返回 miss
	}
	return k.value, true
}

func main() {
	stock := &redisKey{value: "10", expires: time.Now().Add(150 * time.Millisecond)}
	if v, ok := get(stock); ok {
		fmt.Println("未过期:", v)
	}
	time.Sleep(200 * time.Millisecond)
	_, ok := get(stock)
	fmt.Println("过期后读取 → miss（调用方降级到 DB/配置值）:", ok)

	fmt.Println("项目 TTL 设计：")
	fmt.Println("  flashsale:stock:{id}      TTL = 活动结束时间 + 1h（自清理）")
	fmt.Println("  flashsale:idem:{id}:{user} TTL 30min（幂等键）")
	fmt.Println("  order:idem:{user}:{req}    TTL 15min（下单幂等）")
	fmt.Println("  product:detail:{id}        TTL 5min（详情缓存）")
}

// 项目位置：internal/flashsale/service/flashsale_service.go 的 remainingTTL；
// 幂等键 TTL 见 Seckill 流程；product:detail TTL 在 internal/product/service。
