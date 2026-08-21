// Package model 定义商品域数据模型：类目 / 商品(SPU) / SKU。
package model

import (
	"encoding/json"
	"time"
)

// 商品状态：仅 on_sale 对游客可见。
const (
	ProductStatusOffSale = "off_sale"
	ProductStatusOnSale  = "on_sale"
	// MaxPriceCents 是 SKU 与秒杀成交价共同遵守的数据库业务上限：100 万元。
	MaxPriceCents int64 = 100_000_000
)

// Category 商品类目，组织商品结构。
type Category struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Product 商品(SPU)：标准化的售卖单元，与规格无关。
type Product struct {
	ID          int64     `json:"id"`
	CategoryID  int64     `json:"category_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// IsOnSale 是否已上架。
func (p *Product) IsOnSale() bool { return p.Status == ProductStatusOnSale }

// SKU 具体可售的库存单元：绑定规格组合、价格与库存数量。
// Specs 为规格组合 JSON（如 {"color":"红","size":"M"}），序列化时内嵌为对象。
type SKU struct {
	ID        int64           `json:"id"`
	ProductID int64           `json:"product_id"`
	Specs     json.RawMessage `json:"specs" gorm:"column:specs"`
	Price     int64           `json:"price"`
	Stock     int             `json:"stock"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// SKUSummary is the product module's batch read model for callers that need
// SKU facts together with the owning Product title.
type SKUSummary struct {
	SKU
	ProductTitle string `json:"product_title"`
}

// ProductDetail 商品详情：SPU 完整信息 + 全部 SKU（游客可见视图）。
type ProductDetail struct {
	Product
	Skus []SKU `json:"skus"`
}
