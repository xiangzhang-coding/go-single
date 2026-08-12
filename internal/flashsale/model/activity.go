// Package model 定义秒杀域数据模型：秒杀活动（独立库存 + 秒杀价 + 时间窗口 + 限购）。
package model

import (
	"encoding/json"
	"time"
)

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

// 用户视角活动状态（秒杀页派生，服务端按 server_time 判定；
// 列表只返回进行中/即将开始，已结束与下架的活动不出现）。
const (
	ActivityStateNotStarted = "not_started" // 即将开始
	ActivityStateInProgress = "in_progress" // 进行中
)

// SKUView 秒杀页 SKU 摘要：规格与原价（与秒杀价对照展示）。
type SKUView struct {
	ID        int64           `json:"id"`
	ProductID int64           `json:"product_id"`
	Specs     json.RawMessage `json:"specs"`
	Price     int64           `json:"price"`
}

// ActivityView 秒杀页活动视图：活动 + 派生状态 + 剩余库存 + SKU/商品摘要。
// Stock 为用户视角剩余库存（Redis 预扣余量，缓存不可用时降级配置库存）。
type ActivityView struct {
	Activity
	State        string  `json:"state"`
	ProductTitle string  `json:"product_title"`
	SKU          SKUView `json:"sku"`
}
