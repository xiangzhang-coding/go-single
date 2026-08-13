// Q5 at-least-once 与幂等消费：重复投递不可怕，重复执行才可怕。
// 运行：go run ./interview/ch06_mq/q05_at_least_once
package main

import (
	"fmt"
)

// 秒杀落单消费者：同一消息可能被投递多次（重连/超时重投），
// 落单必须幂等——靠唯一键（order_no 主键 / user_activity_key 唯一约束）去重。
type orderRepo struct {
	created map[string]bool
	stock   int
}

func (r *orderRepo) createSeckill(orderNo string) error {
	if r.created[orderNo] {
		return fmt.Errorf("duplicate: %s（幂等成功，不重复扣减库存）", orderNo)
	}
	r.created[orderNo] = true
	r.stock--
	return nil
}

func main() {
	repo := &orderRepo{created: map[string]bool{}, stock: 5}

	// 同一消息投递 3 次（网络抖动重投）。
	for i := 0; i < 3; i++ {
		err := repo.createSeckill("O20260813001")
		if err != nil {
			fmt.Println("第", i+1, "次:", err)
		} else {
			fmt.Println("第", i+1, "次: 落单成功")
		}
	}
	fmt.Println("库存只扣 1 次:", repo.stock == 4)
}

// 项目位置：internal/flashsale/service/flashsale_consumer.go 的 Handle → order.CreateSeckill；
// 唯一键 uk_orders_user_activity_key（migrations/000014）+ 1062 映射（order_repository_gorm.go）
// 使重复消费幂等；库存条件扣减保证不会二次扣。
