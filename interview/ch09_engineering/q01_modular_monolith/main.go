// Q1 模块化单体：单部署单元 + 模块间进程内调用。
// 运行：go run ./interview/ch09_engineering/q01_modular_monolith
package main

import "fmt"

// 模块化单体 = 一个进程内按业务域切分的模块集合，模块间经接口调用。
// 对比微服务：无网络跳转（低延迟）、事务跨模块简单、无分布式难题；
// 代价：只能整体扩容，模块间耦合靠纪律（DAG 依赖）维持。

// 依赖方向：order → flashsale（order 侧声明接口，flashsale 实现），无环。
type ActivityStock interface {
	DeductStock(activityID, qty int64) error
}

type flashsaleModule struct{}

func (flashsaleModule) DeductStock(activityID, qty int64) error {
	return fmt.Errorf("simulate deduct stock of activity %d", activityID)
}

func main() {
	// 进程内装配：order 拿到的是接口，运行时才绑定 flashsale 实现。
	var port ActivityStock = flashsaleModule{}
	fmt.Println("order 调用方视角：", port.DeductStock(1001, 1))

	fmt.Println("跨模块写怎么保证原子性？→ tx 参数汇入同一事务（见 q07）")
}

// 项目位置：internal/order/service/order_service.go 声明 ActivityStock/SeckillRestore
// 最小接口（119-133），internal/flashsale/service 实现；装配在 cmd/server/main.go；
// 模块依赖 DAG 见 docs/DESIGN.md 依赖图。
