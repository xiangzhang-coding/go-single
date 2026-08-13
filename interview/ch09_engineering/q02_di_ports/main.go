// Q2 端口-适配器（Ports & Adapters）：接口在调用方、实现在提供方。
// 运行：go run ./interview/ch09_engineering/q02_di_ports
package main

import "fmt"

// 端口（Port）：order 模块需要"减库存"能力，只声明自己用到的子集。
type StockDeduction interface {
	Deduct(skuID int64, qty int) error
}

// 适配器 1：真实 GORM 实现。
type GormStock struct{}

func (GormStock) Deduct(skuID int64, qty int) error {
	return fmt.Errorf("UPDATE skus SET stock=stock-%d WHERE id=%d AND stock>=%d", qty, skuID, qty)
}

// 适配器 2：测试替身（fake）——单测不需要真实 DB。
type FakeStock struct{ Calls int }

func (f *FakeStock) Deduct(skuID int64, qty int) error {
	f.Calls++
	return nil
}

func main() {
	var real StockDeduction = GormStock{}
	fmt.Println("生产:", real.Deduct(1, 1))

	fake := &FakeStock{}
	var svc StockDeduction = fake
	_ = svc.Deduct(1, 1)
	fmt.Println("测试注入 fake，调用次数:", fake.Calls)
}

// 项目位置：各模块 Repository/Cache/MQ 均为接口 + GORM/Redis/RabbitMQ 实现
//（internal/*/repository/*_gorm.go），并带编译期断言 var _ Repository = (*GORMRepo)(nil)；
// ADR-0003 记录该约定（docs/adr/0003-port-seams.md）。
