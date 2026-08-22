// Package model 定义订单域数据模型：订单与订单项（含地址快照与状态机）。
package model

import (
	"encoding/json"
	"time"
)

// 订单状态：普通/秒杀共用状态机。
// 待支付 → 已支付 → 已发货 → 已完成；含取消与超时取消。
const (
	OrderStatusPendingPayment = "pending_payment" // 待支付
	OrderStatusPaid           = "paid"            // 已支付（支付回调）
	OrderStatusShipped        = "shipped"         // 已发货（后台发货）
	OrderStatusCompleted      = "completed"       // 已完成（用户确认收货）
	OrderStatusCancelled      = "cancelled"       // 已取消（用户取消/超时取消）
)

// 订单类型。
const (
	OrderTypeNormal  = "normal"  // 普通订单
	OrderTypeSeckill = "seckill" // 秒杀订单（不使用优惠券，按购买槽位去重）
)

// 状态机迁移表：仅允许合法跃迁，其余一律拒绝（如 待支付→已完成）。
var transitions = map[string]map[string]bool{
	OrderStatusPendingPayment: {OrderStatusPaid: true, OrderStatusCancelled: true},
	OrderStatusPaid:           {OrderStatusShipped: true},
	OrderStatusShipped:        {OrderStatusCompleted: true},
}

// CanTransition 状态机校验：from → to 是否合法跃迁。
func CanTransition(from, to string) bool { return transitions[from][to] }

// Order 订单：一次购买的完整记录；金额单位均为分。
// 地址字段为下单时从地址簿固化的地址快照，用户后续改地址不影响历史订单。
type Order struct {
	OrderNo         string     `json:"order_no"`
	UserID          int64      `json:"user_id"`
	ClientRequestID *string    `json:"-" gorm:"column:client_request_id"`
	OrderType       string     `json:"order_type"`
	Status          string     `json:"status"`
	ActivityID      *int64     `json:"activity_id,omitempty" gorm:"column:activity_id"`
	PurchaseSlot    *int64     `json:"purchase_slot,omitempty,string" gorm:"column:purchase_slot"`
	TotalAmount     int64      `json:"total_amount" gorm:"column:total_amount"`
	DiscountAmount  int64      `json:"discount_amount" gorm:"column:discount_amount"`
	PayAmount       int64      `json:"pay_amount" gorm:"column:pay_amount"`
	CouponID        *int64     `json:"coupon_id,omitempty" gorm:"column:coupon_id"`
	Receiver        string     `json:"receiver"`
	Phone           string     `json:"phone"`
	Province        string     `json:"province"`
	City            string     `json:"city"`
	District        string     `json:"district"`
	Detail          string     `json:"detail"`
	PaidAt          *time.Time `json:"paid_at,omitempty" gorm:"column:paid_at"`
	ShippedAt       *time.Time `json:"shipped_at,omitempty" gorm:"column:shipped_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty" gorm:"column:completed_at"`
	CancelledAt     *time.Time `json:"cancelled_at,omitempty" gorm:"column:cancelled_at"`
	ExpireAt        time.Time  `json:"expire_at" gorm:"column:expire_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	// UserActivityKey 秒杀订单去重键：落单写 "user_id:activity_id:purchase_slot"，
	// 取消/超时取消同事务置 NULL（MySQL 唯一索引允许多个 NULL）；同一用户可
	// 占用活动限购范围内的多个槽位，同槽消息重投仍只命中一单。
	// 普通订单恒 NULL（JSON 不暴露内部键）。
	UserActivityKey *string `json:"-" gorm:"column:user_activity_key"`
}

// OrderItem 订单项：下单时固化的商品快照（标题/规格/成交单价），
// 价格与标题不受商品域后续修改影响。
type OrderItem struct {
	ID        int64           `json:"id"`
	OrderNo   string          `json:"order_no" gorm:"column:order_no"`
	SKUID     int64           `json:"sku_id" gorm:"column:sku_id"`
	ProductID int64           `json:"product_id" gorm:"column:product_id"`
	Title     string          `json:"title"`
	Specs     json.RawMessage `json:"specs"`
	Price     int64           `json:"price"`
	Quantity  int             `json:"quantity"`
	Subtotal  int64           `json:"subtotal"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// OrderView 订单视图：订单 + 订单项（详情与列表项共用）。
type OrderView struct {
	Order
	Items []OrderItem `json:"items"`
}
