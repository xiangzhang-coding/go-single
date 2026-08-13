// Q1 唯一索引与幂等：为什么唯一约束比"先查后插"可靠。
// 运行：go run ./interview/ch04_mysql/q01_unique_index
package main

import (
	"errors"
	"fmt"
)

// 模拟 uk_orders_user_activity_key：并发下"查重→插入"两步会被两个请求同时穿过，
// 唯一索引在数据库层原子拦截第二个插入（1062 错误）。
var store = map[string]string{}

var ErrDuplicate = errors.New("duplicate key (MySQL 1062)")

func insertOrderNoDup(key string) error {
	if _, exists := store[key]; exists { // 先查
		return ErrDuplicate
	}
	store[key] = "order_1001" // 后插：并发时可能被绕过
	return nil
}

func main() {
	// 模拟两个并发请求同时到达：查重都通过 → 双双插入，破坏了唯一性。
	// 真实项目用数据库 UNIQUE 约束兜底：第二个 insert 直接报 1062。
	fmt.Println("并发缺陷演示：", insertOrderNoDup("u1:a1") == nil, insertOrderNoDup("u1:a1") == nil)

	// 正确姿势：直接插入并捕获 1062，映射为业务错误。
	// 项目映射：mysqlErr 1062 → ErrOrderDuplicate（order_repository_gorm.go）。
	fmt.Println("唯一索引（DB 层）才是幂等兜底")
}

// 项目位置：migrations/000014_seckill_repurchase 的 uk_orders_user_activity_key
// （可空，取消订单后释放）；internal/order/repository/order_repository_gorm.go 中
// errors.As(err, &mysqlErr) 把 1062 映射为 ErrOrderDuplicate → 消费者按幂等成功处理。
