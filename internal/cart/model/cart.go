// Package model 定义购物车域数据模型：条目引用 SKU。
package model

import (
	"encoding/json"
	"time"
)

// CartItem 购物车条目：用户暂存待购的 SKU 与其数量。
type CartItem struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	SKUID     int64     `json:"sku_id" gorm:"column:sku_id"`
	Quantity  int       `json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CartItemView 购物车列表条目：条目 + 经 product 服务端口取得的 SKU/商品展示事实。
type CartItemView struct {
	CartItem
	ProductID int64           `json:"product_id"`
	Title     string          `json:"title"`
	Specs     json.RawMessage `json:"specs"`
	Price     int64           `json:"price"`
	Stock     int             `json:"stock"`
}
