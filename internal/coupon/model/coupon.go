// Package model 定义优惠券域数据模型：券模板 / 用户券。
package model

import "time"

// 券模板类型：直减与满减。
const (
	TemplateTypeDirect    = "direct"    // 直减：满 0 减面额
	TemplateTypeThreshold = "threshold" // 满减：满 min_amount 减面额
)

// 用户券状态：used 落库；expired 由读取时按有效期派生（见 UserCouponView）。
const (
	CouponStatusUnused  = "unused"
	CouponStatusUsed    = "used"
	CouponStatusExpired = "expired"
)

// CouponTemplate 券模板（admin 发布）：类型/面额/门槛/总量/限领/有效期。
// 金额单位均为分。
type CouponTemplate struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Value        int64     `json:"value"`
	MinAmount    int64     `json:"min_amount"`
	Total        int       `json:"total"`
	PerUserLimit int       `json:"per_user_limit"`
	ValidFrom    time.Time `json:"valid_from"`
	ValidUntil   time.Time `json:"valid_until"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CouponTemplateView 可领券列表项：模板 + 当前用户视角状态。
// State 取值：claimable / not_started / ended / sold_out / limit_reached。
type CouponTemplateView struct {
	CouponTemplate
	ClaimedCount int64  `json:"claimed_count"`
	State        string `json:"state"`
}

// UserCoupon 用户持有的券：一条领取记录，核销时置 used。
type UserCoupon struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	TemplateID int64      `json:"template_id"`
	Status     string     `json:"status"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// UserCouponView 我的券：券 + 模板关键信息 + 派生状态（unused/used/expired）。
// expired = 未用且已过 valid_until，读取时按当前时间派生，不落库。
type UserCouponView struct {
	ID         int64      `json:"id"`
	TemplateID int64      `json:"template_id"`
	Name       string     `json:"name"`
	Type       string     `json:"type"`
	Value      int64      `json:"value"`
	MinAmount  int64      `json:"min_amount"`
	Status     string     `json:"status"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidUntil time.Time  `json:"valid_until"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}
