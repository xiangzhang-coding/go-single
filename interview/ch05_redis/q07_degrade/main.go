// Q7 缓存降级：缓存故障不影响主流程。
// 运行：go run ./interview/ch05_redis/q07_degrade
package main

import (
	"errors"
	"fmt"
)

var ErrCacheMiss = errors.New("cache miss")

type cache interface {
	Get(key string) (int, error)
}

type redisCache struct {
	down bool // 模拟 Redis 故障
}

func (r redisCache) Get(key string) (int, error) {
	if r.down {
		return 0, errors.New("connection refused") // 基础设施故障
	}
	return 0, ErrCacheMiss
}

// 秒杀页剩余库存：缓存读失败/缺失 → 降级返回配置库存，页面照常展示。
func listActivityStock(c cache, configured int) int {
	left, err := c.Get("flashsale:stock:1001")
	if err != nil {
		fmt.Println("降级日志：", err, "→ 使用配置库存")
		return configured
	}
	return left
}

func main() {
	// 正常：缓存有数据。
	normal := listActivityStock(redisCache{down: false}, 100)
	fmt.Println("正常路径剩余库存:", normal)

	// Redis 挂：不报错，返回配置库存（秒杀页仍可浏览，只是数字不实时）。
	degraded := listActivityStock(redisCache{down: true}, 100)
	fmt.Println("降级路径剩余库存:", degraded)
}

// 项目位置：internal/flashsale/service/flashsale_service.go 的 ListUserActivities——
// 剩余库存读 Redis 预扣余量，缺失/读失败降级配置库存；product GetDetail 缓存
// miss 回填，读失败直查 DB（product_service.go，slog 打降级日志）。
