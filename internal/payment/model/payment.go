// Package model 定义支付域数据模型：支付流水（Payment）。
package model

import "time"

// 支付结果：模拟支付回调的最终结果；失败流水留档审计，订单停留待支付可重付。
const (
	PaymentResultSuccess = "success" // 支付成功（驱动订单 待支付→已支付）
	PaymentResultFail    = "fail"    // 支付失败（订单状态不变，可重试）
)

// Payment 支付流水：一次模拟支付尝试的完整记录。
// PaymentID 由客户端生成（模拟支付渠道的交易号），UNIQUE 约束挡重复回调；
// Amount 为回调申报金额（分），成功回调与订单 pay_amount 核对。
type Payment struct {
	ID        int64     `json:"id"`
	PaymentID string    `json:"payment_id"`
	OrderNo   string    `json:"order_no"`
	UserID    int64     `json:"user_id"`
	Amount    int64     `json:"amount"`
	Result    string    `json:"result"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
