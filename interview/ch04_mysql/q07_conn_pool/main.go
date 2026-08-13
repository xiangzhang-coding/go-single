// Q7 连接池与慢查询：池参数为什么重要。
// 运行：go run ./interview/ch04_mysql/q07_conn_pool
package main

import (
	"fmt"
	"time"
)

type pool struct {
	maxOpen int
	maxIdle int
	life    time.Duration
	used    int
}

func (p *pool) acquire() bool {
	if p.used >= p.maxOpen {
		return false // 池耗尽：请求排队/超时，表现为连接超时
	}
	p.used++
	return true
}

func main() {
	p := &pool{maxOpen: 20, maxIdle: 5, life: 5 * time.Minute}
	fmt.Println("配置要点（main.go openMySQL）：")
	fmt.Println("  SetMaxOpenConns(20)   防止无限建连压垮 DB")
	fmt.Println("  SetMaxIdleConns(5)    控制空闲连接（过高浪费、过低频繁握手）")
	fmt.Println("  SetConnMaxLifetime(5m) 避免长连接被 LB/防火墙掐断")

	// 池耗尽演示
	for i := 0; i < 20; i++ {
		p.acquire()
	}
	fmt.Println("池是否耗尽:", !p.acquire())

	// 慢查询：WHERE user_id=? 无索引 vs 有索引。
	fmt.Println("orders 建 idx_orders_user_status 索引后，按用户查订单 O(log n) 而非全表扫")
}

// 项目位置：cmd/server/main.go 的 openMySQL（连接池四项 + PingContext 5s 超时）；
// migrations/000009_orders 建 idx_orders_user_status（user_id,status）；
// GORM Logger Warn 级别打印慢查询（main.go gorm.Config）。
