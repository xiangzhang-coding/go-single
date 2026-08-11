package model

import "time"

// Post 好友圈动态：购买成功后分享，引用已购 SKU + 可选文案 + 可选图片。
// Content / ImageURL 为空串表示未填写。
type Post struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	SKUID     int64     `json:"sku_id" gorm:"column:sku_id"`
	Content   string    `json:"content,omitempty"`
	ImageURL  string    `json:"image_url,omitempty" gorm:"column:image_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PostView 时间线条目：动态 + 发布者用户名（由 service 跨模块补齐）。
type PostView struct {
	Post
	AuthorUsername string `json:"author_username"`
}
