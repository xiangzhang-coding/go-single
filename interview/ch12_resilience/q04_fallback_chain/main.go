// Q4 降级链：缓存 → DB → 默认值，逐级兜底。
// 运行：go run ./interview/ch12_resilience/q04_fallback_chain
package main

import (
	"errors"
	"fmt"
)

var (
	ErrCacheDown = errors.New("cache down")
	ErrDBDown    = errors.New("db down")
)

// 三级降级：命中返回；miss/故障逐级下降，最终给默认值而不是报错。
func stockLeft(cacheOK, dbOK bool) int {
	if cacheOK {
		v := 46 // 命中缓存
		fmt.Println("① 缓存命中:", v)
		return v
	}
	fmt.Println("① 缓存 miss/故障 → ② 直查 DB")
	if dbOK {
		v := 50
		fmt.Println("② DB 查询:", v, "（并回填缓存）")
		return v
	}
	fmt.Println("② DB 也不可用 → ③ 返回配置库存（页面可用，数字不实时）")
	return 100
}

func main() {
	stockLeft(true, true)        // 正常
	stockLeft(false, true)       // 缓存挂
	stockLeft(false, false)      // 缓存 + DB 全挂
	_ = ErrCacheDown
	_ = ErrDBDown
}

// 项目位置：internal/flashsale/service/flashsale_service.go 的 ListUserActivities——
// 剩余库存读 Redis 降级配置库存（379-385）；product GetDetail 缓存 miss 回填、
// 读失败直查 DB（internal/product/service/product_service.go，slog 打降级日志）。
