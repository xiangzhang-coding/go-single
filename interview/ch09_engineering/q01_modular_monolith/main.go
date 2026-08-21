// Q1 模块化单体：单部署单元 + 模块间进程内调用。
// 运行：go run ./interview/ch09_engineering/q01_modular_monolith
package main

import "fmt"

// 模块化单体 = 一个进程内按业务域切分的模块集合，模块间经接口调用。
// 对比微服务：无网络跳转（低延迟）、事务跨模块简单、无分布式难题；
// 代价：只能整体扩容，模块间耦合靠纪律（DAG 依赖）维持。

// 依赖方向：flashsale → order（flashsale 声明自己需要的最小订单能力），无环。
type SeckillOrderWriter interface {
	CreateInTx(orderNo string) error
}

type orderModule struct{}

func (orderModule) CreateInTx(orderNo string) error {
	return fmt.Errorf("simulate create order %s", orderNo)
}

type flashsaleModule struct{ orders SeckillOrderWriter }

func (m flashsaleModule) Handle(orderNo string) error {
	// 实际项目在同一事务中继续扣减 flashsale 活动库存。
	return m.orders.CreateInTx(orderNo)
}

func main() {
	// 进程内装配：flashsale 只拿到 order 的最小接口，order 不反向持有 flashsale。
	consumer := flashsaleModule{orders: orderModule{}}
	fmt.Println("flashsale 调用方视角：", consumer.Handle("1001"))

	fmt.Println("跨模块写怎么保证原子性？→ tx 参数汇入同一事务（见 q07）")
}

// 项目位置：internal/flashsale/service 的消费者、SeckillCancellation 声明 order
// 最小接口并完成应用编排；order 不持有 flashsale；装配在 cmd/server/main.go；
// 模块依赖 DAG 见 docs/DESIGN.md 依赖图。
