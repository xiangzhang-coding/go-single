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

// CartItemView 购物车列表条目：条目 + SKU/商品只读快照（标题/规格/价格/库存），
// 供列表展示；快照为跨表读模型（见仓储实现），不含商品域写路径。
type CartItemView struct {
	CartItem
	ProductID int64           `json:"product_id"`
	Title     string          `json:"title"`
	Specs     json.RawMessage `json:"specs"`
	Price     int64           `json:"price"`
	Stock     int             `json:"stock"`
}
