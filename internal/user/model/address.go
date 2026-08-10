package model

import "time"

// TableName 地址表名（避免 GORM 默认复数化）。
func (Address) TableName() string { return "user_addresses" }

// Address 收货地址：用户维护的地址簿条目，下单时从中选择一条固化为地址快照。
// 默认地址唯一性由 users.default_address_id 指针保证，is_default 为读取时派生标记。
type Address struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Receiver  string    `json:"receiver"`
	Phone     string    `json:"phone"`
	Province  string    `json:"province"`
	City      string    `json:"city"`
	District  string    `json:"district"`
	Detail    string    `json:"detail"`
	IsDefault bool      `json:"is_default" gorm:"->"` // 读取时派生（users.default_address_id 指针），不落库
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
