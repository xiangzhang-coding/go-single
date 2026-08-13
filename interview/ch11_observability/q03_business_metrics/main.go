// Q3 业务指标：带标签的计数器与 Gauge 刷新时机。
// 运行：go run ./interview/ch11_observability/q03_business_metrics
package main

import "fmt"

// 业务指标设计要点：标签区分维度（result=success|fail），计数打点位置要紧贴业务事件。
type businessMetrics struct {
	preDeduct map[string]int64 // seckill_prededuct_total{result}
	stock     map[int64]int64  // seckill_stock_remaining{activity_id} gauge
}

func (b *businessMetrics) incPreDeduct(result string) {
	b.preDeduct[result]++
}

func (b *businessMetrics) setStock(activityID, left int64) {
	b.stock[activityID] = left
}

func main() {
	m := &businessMetrics{preDeduct: map[string]int64{}, stock: map[int64]int64{}}
	for _, r := range []string{"success", "success", "sold_out", "success"} {
		m.incPreDeduct(r)
	}
	m.setStock(1001, 46) // 预扣/回补/页面浏览/上下架后刷新
	fmt.Println("seckill_prededuct_total:", m.preDeduct)
	fmt.Println("seckill_stock_remaining:", m.stock)

	fmt.Println("其他业务点：orders_created_total{order_type}、orders_payment_total{result}、")
	fmt.Println("mq_consume_failed_total{reason=permanent|transient}、coupon_issued_total")
}

// 项目位置：internal/platform/metrics/business.go 的 Business 注册器（44-94）；
// 打点调用点：flashsale preDeductSuccess/Failed、order OrderCreated/OrderStatusChanged、
// payment PaymentResult、coupon CouponIssued/Redeemed；Grafana business.json 面板对应。
