// Q6 唯一键冲突（1062）映射业务错误：幂等的关键。
// 运行：go run ./interview/ch04_mysql/q06_unique_idempotent
package main

import (
	"errors"
	"fmt"
)

// 模拟 MySQL 1062 错误。
var ErrMySQL1062 = errors.New("Error 1062: Duplicate entry 'xxx' for key 'uk_orders_user_activity_key'")

// 模拟订单仓储：插入命中唯一键 → 返回"重复"业务错误。
func saveOrder(no string) error {
	if no == "O20260813001" {
		return ErrMySQL1062
	}
	return nil
}

func main() {
	// 秒杀消费者：重复消息 → 幂等成功（不重复扣库存）。
	// 项目做法：errors.As(err, &mysqlErr) 解析 1062 → ErrOrderDuplicate → 消费者 Ack。
	no := "O20260813001"
	if err := saveOrder(no); err != nil {
		var mysqlErr *mySQLError
		if errors.As(err, &mysqlErr) {
			fmt.Println("按 1062 处理为幂等成功")
			return
		}
		fmt.Println("其他错误:", err)
	}
}

// mySQLError 占位类型：真实项目直接用 go-sql-driver/mysql 的 *mysql.MySQLError。
type mySQLError struct{ Number uint16 }

func (e *mySQLError) Error() string { return "mysql error" }

// 项目位置：internal/order/repository/order_repository_gorm.go（errors.As 判 1062 →
// ErrOrderDuplicate）；消费端把它视为幂等成功（flashsale_consumer.go 的 classifyCreateError）。
