// Package model 定义秒杀域数据模型：秒杀活动（独立库存 + 秒杀价 + 时间窗口 + 限购）。
package model

import "time"

// 活动状态：仅 on_sale 可被抢购；进行中由时间窗口动态判定（DESIGN.md），
// status 仅用于手动下架/紧急停止。
const (
	ActivityStatusOffSale = "off_sale"
	ActivityStatusOnSale  = "on_sale"
)

// Activity 秒杀活动：绑定 SKU，独立库存与秒杀价，与 sku.stock 互不干扰。
// 金额单位：分。
type Activity struct {
	ID           int64     `json:"id"`
	SKUID        int64     `json:"sku_id" gorm:"column:sku_id"`
	Title        string    `json:"title"`
	Price        int64     `json:"price"`
	Stock        int       `json:"stock"`
	PerUserLimit int       `json:"per_user_limit"`
	Status       string    `json:"status"`
	StartAt      time.Time `json:"start_at"`
	EndAt        time.Time `json:"end_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// IsOnSale 是否已上架。
func (a *Activity) IsOnSale() bool { return a.Status == ActivityStatusOnSale }

// TableName 显式指定表名（GORM 默认复数化会推断为 activities）。
func (Activity) TableName() string { return "flashsale_activities" }

// InProgress 是否进行中：上架且在时间窗口内（进行中 = 上架 && start_at <= now <= end_at）。
func (a *Activity) InProgress(now time.Time) bool {
	return a.IsOnSale() && !now.Before(a.StartAt) && !now.After(a.EndAt)
}
