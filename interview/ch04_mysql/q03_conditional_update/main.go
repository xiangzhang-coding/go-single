// Q3 条件更新防超卖：UPDATE ... WHERE stock >= ?。
// 运行：go run ./interview/ch04_mysql/q03_conditional_update
package main

import (
	"errors"
	"fmt"
	"sync"
)

type sku struct {
	mu    sync.Mutex
	stock int
}

// 内存版"UPDATE skus SET stock=stock-1 WHERE id=? AND stock>=1"：
// 条件不满足不扣减（对比先 SELECT 再 UPDATE 的检查-执行两步）。
func (s *sku) deduct() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stock < 1 {
		return errors.New("库存不足（条件更新未命中 0 行）")
	}
	s.stock--
	return nil
}

func main() {
	s := &sku{stock: 3}
	succ := 0
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ { // 10 个并发扣减
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.deduct() == nil {
				succ++
			}
		}()
	}
	wg.Wait()
	fmt.Printf("并发 10 次扣减，成功 %d 次（库存 3，不会超卖）\n", succ)
}

// 项目位置：internal/flashsale/repository/activity_repository_gorm.go 的条件扣减
//（UPDATE ... WHERE stock >= ?），秒杀订单事务内 DeductStock 同样走条件更新；
// 商品 SKU 同理（internal/product 侧）。
