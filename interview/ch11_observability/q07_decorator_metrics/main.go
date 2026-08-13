// Q7 装饰器模式打点：透明包裹基础设施（WrapMQ 语义）。
// 运行：go run ./interview/ch11_observability/q07_decorator_metrics
package main

import (
	"fmt"
)

type mqClient interface {
	Publish(queue string, body []byte) error
	Consume(queue string) error
}

// 真实实现。
type rabbitMQ struct{}

func (rabbitMQ) Publish(queue string, body []byte) error {
	return fmt.Errorf("simulated publish to %s", queue)
}

func (rabbitMQ) Consume(queue string) error { return nil }

// 装饰器：不改原实现，外层加指标打点。
type metricsMQ struct {
	inner mqClient
	count map[string]int64
}

func (m *metricsMQ) Publish(queue string, body []byte) error {
	err := m.inner.Publish(queue, body)
	key := fmt.Sprintf("mq_published_total{queue=%s,result=%s}", queue, resultOf(err))
	m.count[key]++
	return err
}

func (m *metricsMQ) Consume(queue string) error {
	return m.inner.Consume(queue)
}

func resultOf(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func main() {
	// 装饰器栈（main.go 221-230）：raw → metrics.WrapMQ → mq.WrapCircuitBreaker。
	m := &metricsMQ{inner: rabbitMQ{}, count: map[string]int64{}}
	_ = m.Publish("flashsale.order.create", []byte(`{"order_no":"O1"}`))
	for k, v := range m.count {
		fmt.Printf("%-65s → %d\n", k, v)
	}
	fmt.Println("打点对业务代码零侵入：业务不感知指标层存在")
}

// 项目位置：internal/platform/metrics/business.go 的 WrapMQ 装饰器（177-207）；
// 消费端打点 mq_consumed_total / mq_consume_failed_total{reason} 在 wrapMQ 的
// Consume 包裹层；装配套接在 cmd/server/main.go。
