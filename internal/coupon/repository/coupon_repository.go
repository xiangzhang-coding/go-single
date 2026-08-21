// Package repository 定义 coupon 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
package repository

import (
	"context"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/coupon/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// ClaimResult is the complete outcome set of an authoritative database claim.
type ClaimResult uint8

const (
	ClaimCreated ClaimResult = iota + 1
	ClaimTemplateNotFound
	ClaimNotInWindow
	ClaimSoldOut
	ClaimLimitReached
)

// ClaimOutcome includes the committed database counts used to repair Redis.
type ClaimOutcome struct {
	Result       ClaimResult
	Coupon       *model.UserCoupon
	ClaimedCount int64
	PerUserCount int64
}

// RedemptionFacts is the authoritative user coupon and template state read
// under the caller's transaction lock. It is internal to the coupon module.
type RedemptionFacts struct {
	CouponID   int64
	UserID     int64
	Status     string
	Value      int64
	MinAmount  int64
	ValidFrom  time.Time
	ValidUntil time.Time
}

// CouponTemplateRepository 券模板数据访问接口。
type CouponTemplateRepository interface {
	Create(ctx context.Context, t *model.CouponTemplate) error
	Update(ctx context.Context, t *model.CouponTemplate) error
	GetByID(ctx context.Context, id int64) (*model.CouponTemplate, error)
	List(ctx context.Context) ([]model.CouponTemplate, error)
}

// UserCouponRepository 用户券数据访问接口。
type UserCouponRepository interface {
	// Claim locks the template and atomically rechecks limits before inserting.
	Claim(ctx context.Context, userID, templateID int64) (ClaimOutcome, error)
	// ListByUser 我的券（JOIN 模板，SQL 内派生状态）；status 为空返回全部，
	// 否则按派生状态筛选（unused/used/expired），返回条目与总数。
	ListByUser(ctx context.Context, userID int64, status string, offset, limit int) ([]model.UserCouponView, int64, error)
	// CountByTemplate 模板已领总数（列表展示可领状态用）。
	CountByTemplate(ctx context.Context, templateID int64) (int64, error)
	// CountUserByTemplate 用户已领该模板的张数（每人限领校验用）。
	CountUserByTemplate(ctx context.Context, userID, templateID int64) (int64, error)
	// GetRedemptionForUpdate locks both the user coupon and its template so an
	// order observes one serializable set of mutable template facts.
	GetRedemptionForUpdate(ctx context.Context, tx *transaction.Handle, couponID int64) (*RedemptionFacts, error)
	// Use 事务内条件核销：unused→used（并发下仅一次成功），供 order 下单调用。
	// 返回是否核销成功；tx 由调用方（order 模块）开启。
	Use(ctx context.Context, tx *transaction.Handle, userID, couponID int64) (bool, error)
	// Rollback 事务内条件回退：used→unused（取消订单回退券）。
	Rollback(ctx context.Context, tx *transaction.Handle, userID, couponID int64) (bool, error)
}

// Store 聚合两个仓储，作为 service 的构造入参。
type Store struct {
	Template   CouponTemplateRepository
	UserCoupon UserCouponRepository
}
