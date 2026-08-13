// Q6 表驱动测试与假对象注入（testify 风格）。
// 运行：go test ./interview/ch09_engineering/q06_testing -v
package main

import (
	"errors"
	"fmt"
)

// 被测函数：把仓储错误翻译成业务错误（对应 translateProductError 的简化版）。
func placeOrder(engine Engine) error {
	if err := engine.CreateSeckill(); err != nil {
		if errors.Is(err, ErrSoldOut) {
			return fmt.Errorf("下单失败: %w", ErrSoldOut)
		}
		return fmt.Errorf("下单失败: %w", err)
	}
	return nil
}

var ErrSoldOut = errors.New("已抢光")

type Engine interface{ CreateSeckill() error }

type fakeEngine struct{ err error }

func (f fakeEngine) CreateSeckill() error { return f.err }

// 表驱动用例：输入 + 期望输出一张表。
var cases = []struct {
	name    string
	engine  Engine
	wantErr bool
	wantIs  error
}{
	{"成功", fakeEngine{nil}, false, nil},
	{"库存不足", fakeEngine{ErrSoldOut}, true, ErrSoldOut},
	{"DB 故障", fakeEngine{errors.New("conn refused")}, true, nil},
}

func main() {
	for _, c := range cases {
		err := placeOrder(c.engine)
		fmt.Printf("%-10s → err=%v wantErr=%v\n", c.name, err, c.wantErr)
	}
}

// 项目位置：单测以手写 fake 替换仓储/缓存（internal/order/service/order_service_test.go、
// flashsale_consumer_test.go），全模块同构；集成测试 *_integration_test.go 连真实
// MySQL/Redis（compose Redis 开 21 个库做包隔离）。
