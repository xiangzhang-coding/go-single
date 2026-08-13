// Q2 事务 ACID：提交/回滚与跨表原子性。
// 运行：go run ./interview/ch04_mysql/q02_transaction
package main

import (
	"errors"
	"fmt"
)

// 极简内存"数据库"：操作记录可以提交或回滚。
type memDB struct {
	ops       []string
	committed bool
}

func (db *memDB) exec(sql string) error {
	db.ops = append(db.ops, sql)
	return nil
}

func (db *memDB) begin()  { db.committed = false }
func (db *memDB) commit() { db.committed = true }
func (db *memDB) rollback() {
	if !db.committed {
		db.ops = nil // 未提交即丢弃（模拟回滚）
	}
}

func main() {
	// 下单事务 = 订单 + 订单项 + 扣减库存 + 地址快照 + 券核销 + 清购物车（全在一个事务）。
	db := &memDB{}
	db.begin()
	_ = db.exec("INSERT orders ...")
	_ = db.exec("UPDATE skus SET stock = stock - 1 ...")
	if errors.New("扣减库存失败") != nil { // 模拟任一步失败
		db.rollback()
		fmt.Println("任一步失败 → 全部回滚，无部分写入")
		return
	}
	db.commit()
	fmt.Println("全部成功 → 提交，原子生效")

	// 关键点：ACID 的原子性保证"要么全有要么全无"，配合行锁避免并发串改。
	fmt.Println("回滚后 ops 长度:", len(db.ops))
}

// 项目位置：internal/order/service/order_service.go 的 createOrder（286-396）与
// CreateSeckill（404-487）；跨模块写经 TxRunner.WithinTx 汇入同一事务
// （internal/order/repository/order_repository.go）。
