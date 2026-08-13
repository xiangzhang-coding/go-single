// Q4 行锁与锁顺序：按固定顺序加锁避免死锁。
// 运行：go run ./interview/ch04_mysql/q04_lock_order
package main

import (
	"fmt"
	"sort"
	"sync"
)

// 多 SKU 扣库存时按 (product_id, sku_id) 排序后加锁——
// 两个事务都以相同顺序拿锁，就不会出现"我等你、你等我"的环形等待。
type skuLock struct {
	id  int64
	mu  sync.Mutex
	got string
}

func main() {
	skus := map[int64]*skuLock{3: {id: 3}, 1: {id: 1}, 2: {id: 2}}
	// 事务内先排序再逐个加锁（模拟 SELECT ... FOR UPDATE 顺序）。
	ids := make([]int64, 0, len(skus))
	for id := range skus {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		skus[id].mu.Lock()
		skus[id].got = fmt.Sprintf("locked %d", id)
		skus[id].mu.Unlock()
	}
	for _, id := range ids {
		fmt.Println(skus[id].got)
	}
	fmt.Println("所有事务按相同排序拿锁 → 无死锁")
}

// 项目位置：internal/order/service/order_service.go 的 createOrder 在锁商品/SKU 前
// sort.Slice 统一排序（order_service.go:677-682）；加锁查询 GetSKUForUpdate 在
// internal/product/service/product_service.go。
