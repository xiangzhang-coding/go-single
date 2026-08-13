// Q7 错误处理：哨兵错误、%w 包装与 errors.Is/As。
// 运行：go run ./interview/ch01_go_basics/q07_error_handling
package main

import (
	"errors"
	"fmt"
)

var ErrSoldOut = errors.New("活动已抢光")
var ErrInvalidInput = errors.New("参数不合法")

// 包装错误：%w 保留原错误链。
func buy() error {
	return fmt.Errorf("%w: 剩余库存 0", ErrSoldOut)
}

// 业务拒绝可加上下文后继续包装。
func placeOrder() error {
	if err := buy(); err != nil {
		return fmt.Errorf("下单失败: %w", err)
	}
	return nil
}

func main() {
	err := placeOrder()

	fmt.Println("errors.Is 命中哨兵:", errors.Is(err, ErrSoldOut))
	fmt.Println("errors.Is 不命中:", errors.Is(err, ErrInvalidInput))

	// errors.As 取链上特定类型。
	var target interface{ Error() string }
	fmt.Println("errors.As 取出:", errors.As(err, &target))

	// 对比：不用 %w 则链条断裂，Is 返回 false。
	broken := fmt.Errorf("下单失败: %v", ErrSoldOut)
	fmt.Println("未包装无法 Is:", errors.Is(broken, ErrSoldOut))
}

// 项目位置：internal/flashsale/service/flashsale_service.go 顶部哨兵错误族
// （ErrActivityNotFound/ErrSoldOut/ErrLimitReached/ErrDuplicateRequest）；
// 服务间翻译用 translateCouponError/translateProductError（internal/order/service/order_service.go）。
