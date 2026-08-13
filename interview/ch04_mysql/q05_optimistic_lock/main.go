// Q5 乐观锁 vs 悲观锁。
// 运行：go run ./interview/ch04_mysql/q05_optimistic_lock
package main

import (
	"errors"
	"fmt"
)

type account struct {
	balance int
	version int // 乐观锁版本号
}

// 乐观锁：UPDATE ... SET balance=balance-100, version=version+1
// WHERE id=? AND version=? —— version 不匹配则影响 0 行，重试。
func optimisticTransfer(a *account, amount int) error {
	if a.version%2 == 0 { // 模拟并发冲突：版本已被别的请求改掉
		return errors.New("冲突，version 不匹配，重试")
	}
	a.balance -= amount
	a.version++
	return nil
}

func main() {
	acc := &account{balance: 500, version: 1}
	fmt.Println("悲观锁（SELECT FOR UPDATE）：先锁行再修改，串行安全但吞吐低")
	fmt.Println("乐观锁（version 条件更新）：冲突时影响 0 行，业务重试")
	if err := optimisticTransfer(acc, 100); err != nil {
		fmt.Println("乐观锁冲突:", err)
	}
	// 重试一次
	_ = optimisticTransfer(acc, 100)
	fmt.Printf("重试成功，余额=%d version=%d\n", acc.balance, acc.version)
}

// 项目位置：本项目主用"条件更新（CAS 语义）+ 行锁"，即悲观与条件更新结合
//（activity_repository_gorm.go 条件扣库存、order_repository_gorm.go 的 MarkPaid
// 带金额断言 UPDATE ... WHERE amount=?）；乐观锁留作延伸讨论。
