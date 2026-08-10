// Package service 承载 coupon 模块业务：admin 发布券模板，
// 用户浏览可领券并领取（Lua 原子防超发），查看我的券（未用/已用/过期派生）。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/coupon/model"
	"github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrTemplateNotFound  = errors.New("coupon template not found")
	ErrInvalidInput      = errors.New("invalid input")
	ErrNotInWindow       = errors.New("coupon not in valid period")
	ErrSoldOut           = errors.New("coupon sold out")
	ErrClaimLimitReached = errors.New("claim limit reached")

	ErrCouponNotFound       = errors.New("coupon not found")
	ErrCouponUsed           = errors.New("coupon already used")
	ErrCouponExpired        = errors.New("coupon is not valid at this time")
	ErrCouponRollbackFailed = errors.New("coupon not in used state, rollback failed")
)

// 可领券列表的状态取值。
const (
	stateClaimable    = "claimable"
	stateNotStarted   = "not_started"
	stateEnded        = "ended"
	stateSoldOut      = "sold_out"
	stateLimitReached = "limit_reached"
)

// Redis key 约定：coupon:claimed:{template_id}（总量计数）/
// coupon:peruser:{template_id}:{user_id}（每人限领计数）。
func claimedKey(templateID int64) string { return fmt.Sprintf("coupon:claimed:%d", templateID) }
func perUserKey(templateID, userID int64) string {
	return fmt.Sprintf("coupon:peruser:%d:%d", templateID, userID)
}

// claimScript Lua 原子领券（学习点，复用秒杀 Lua 模式防超发）：
// 校验有效期窗口 → 检查总量 → 检查每人限领 → 双计数 INCR。
// 返回码：1 成功 / 0 已抢光 / -1 不在有效期 / -2 超过每人限领。
// KEYS[1] 总量计数；KEYS[2] 每人限领计数。
// ARGV[1] 当前时间(ms)；ARGV[2] 总量；ARGV[3] 开始时间(ms)；
// ARGV[4] 结束时间(ms)；ARGV[5] 每人限领。
const claimScript = `
if ARGV[1] < ARGV[3] or ARGV[1] > ARGV[4] then
    return -1
end
local claimed = tonumber(redis.call('GET', KEYS[1]) or '0')
if claimed >= tonumber(ARGV[2]) then
    return 0
end
local per_user = tonumber(redis.call('GET', KEYS[2]) or '0')
if per_user >= tonumber(ARGV[5]) then
    return -2
end
redis.call('INCR', KEYS[1])
redis.call('INCR', KEYS[2])
return 1
`

// 领券脚本返回码。
const (
	claimOK          int64 = 1
	claimSoldOut     int64 = 0
	claimNotInWindow int64 = -1
	claimLimitReach  int64 = -2
)

// TemplateParams 券模板参数（创建/编辑共用）。
type TemplateParams struct {
	Name         string
	Type         string
	Value        int64
	MinAmount    int64
	Total        int
	PerUserLimit int
	ValidFrom    time.Time
	ValidUntil   time.Time
}

// Service coupon 模块的业务接口。
type Service interface {
	// ---- admin ----
	CreateTemplate(ctx context.Context, p TemplateParams) (*model.CouponTemplate, error)
	UpdateTemplate(ctx context.Context, id int64, p TemplateParams) error
	ListTemplates(ctx context.Context) ([]model.CouponTemplate, error)

	// ---- 用户 ----
	// ListClaimable 可领券列表（含当前用户视角的领取状态）。
	ListClaimable(ctx context.Context, userID int64) ([]model.CouponTemplateView, error)
	// Claim 领取：Lua 原子校验（有效期/总量/每人限领）后 DB 落库为最终态。
	Claim(ctx context.Context, userID, templateID int64) (*model.UserCoupon, error)
	// ListMine 我的券（status 空 = 全部；unused/used/expired 筛选）。
	ListMine(ctx context.Context, userID int64, status string, page, pageSize int) ([]model.UserCouponView, int64, error)
	// GetUsable 校验并读取一张可用券（归属/未用/在有效期），供 order 模块结算使用。
	GetUsable(ctx context.Context, userID, couponID int64) (*model.UserCouponView, error)
	// UseCoupon 事务内条件核销（unused→used + 有效期窗口原子校验，并发仅一次
	// 成功），供 order 模块下单调用；失败时区分已用/已过期/不存在。
	UseCoupon(ctx context.Context, tx *gorm.DB, userID, couponID int64) error
	// RollbackCoupon 事务内条件回退（used→unused），供 order 模块取消订单调用。
	RollbackCoupon(ctx context.Context, tx *gorm.DB, userID, couponID int64) error
}

type couponService struct {
	store repository.Store
	cache cache.Cache
}

// New 构造优惠券服务。
func New(store repository.Store, c cache.Cache) Service {
	return &couponService{store: store, cache: c}
}

// ---- admin ----

func (s *couponService) CreateTemplate(ctx context.Context, p TemplateParams) (*model.CouponTemplate, error) {
	if err := validateTemplate(&p); err != nil {
		return nil, err
	}
	t := &model.CouponTemplate{
		Name:         p.Name,
		Type:         p.Type,
		Value:        p.Value,
		MinAmount:    p.MinAmount,
		Total:        p.Total,
		PerUserLimit: p.PerUserLimit,
		ValidFrom:    p.ValidFrom,
		ValidUntil:   p.ValidUntil,
	}
	if err := s.store.Template.Create(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *couponService) UpdateTemplate(ctx context.Context, id int64, p TemplateParams) error {
	t, err := s.store.Template.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if t == nil {
		return ErrTemplateNotFound
	}
	if err := validateTemplate(&p); err != nil {
		return err
	}
	return s.store.Template.Update(ctx, &model.CouponTemplate{
		ID:           id,
		Name:         p.Name,
		Type:         p.Type,
		Value:        p.Value,
		MinAmount:    p.MinAmount,
		Total:        p.Total,
		PerUserLimit: p.PerUserLimit,
		ValidFrom:    p.ValidFrom,
		ValidUntil:   p.ValidUntil,
	})
}

func (s *couponService) ListTemplates(ctx context.Context) ([]model.CouponTemplate, error) {
	return s.store.Template.List(ctx)
}

// ---- 用户 ----

// ListClaimable 状态判定：
// not_started（未开始）/ ended（已结束）/ sold_out（总量已领完）/
// limit_reached（用户已达每人限领）/ claimable（可领）。
// 计数取 DB 已领数，仅作展示；防超发的强制校验在 Lua。
func (s *couponService) ListClaimable(ctx context.Context, userID int64) ([]model.CouponTemplateView, error) {
	templates, err := s.store.Template.List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	views := make([]model.CouponTemplateView, 0, len(templates))
	for i := range templates {
		t := &templates[i]
		v := model.CouponTemplateView{CouponTemplate: *t}
		switch {
		case now.Before(t.ValidFrom):
			v.State = stateNotStarted
		case now.After(t.ValidUntil):
			v.State = stateEnded
		default:
			claimed, err := s.store.UserCoupon.CountByTemplate(ctx, t.ID)
			if err != nil {
				return nil, err
			}
			v.ClaimedCount = claimed
			if claimed >= int64(t.Total) {
				v.State = stateSoldOut
			} else {
				perUser, err := s.store.UserCoupon.CountUserByTemplate(ctx, userID, t.ID)
				if err != nil {
					return nil, err
				}
				if perUser >= int64(t.PerUserLimit) {
					v.State = stateLimitReached
				} else {
					v.State = stateClaimable
				}
			}
		}
		views = append(views, v)
	}
	return views, nil
}

// Claim 领取流程：DB 读模板（含有效期快照）→ Lua 原子计数 → DB 落库最终态。
// 模板过期/不存在由 DB 校验，其余并发条件由 Lua 原子强制。
func (s *couponService) Claim(ctx context.Context, userID, templateID int64) (*model.UserCoupon, error) {
	t, err := s.store.Template.GetByID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTemplateNotFound
	}

	now := time.Now()
	code, err := s.cache.Eval(ctx, claimScript,
		[]string{claimedKey(templateID), perUserKey(templateID, userID)},
		now.UnixMilli(), t.Total, t.ValidFrom.UnixMilli(), t.ValidUntil.UnixMilli(), t.PerUserLimit)
	if err != nil {
		return nil, err
	}

	switch code {
	case claimOK:
	case claimSoldOut:
		return nil, ErrSoldOut
	case claimNotInWindow:
		return nil, ErrNotInWindow
	case claimLimitReach:
		return nil, ErrClaimLimitReached
	default:
		return nil, fmt.Errorf("%w: unexpected claim code %d", ErrInvalidInput, code)
	}

	c := &model.UserCoupon{UserID: userID, TemplateID: templateID, Status: model.CouponStatusUnused}
	if err := s.store.UserCoupon.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *couponService) ListMine(ctx context.Context, userID int64, status string, page, pageSize int) ([]model.UserCouponView, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	return s.store.UserCoupon.ListByUser(ctx, userID, status, (page-1)*pageSize, pageSize)
}

// GetUsable 结算前校验：券必须存在且归属当前用户、未用、处于有效期；
// 门槛校验（满减）由 order 模块按订单总额完成（全场券，无商品维度限制）。
func (s *couponService) GetUsable(ctx context.Context, userID, couponID int64) (*model.UserCouponView, error) {
	v, err := s.store.UserCoupon.GetViewByID(ctx, userID, couponID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, ErrCouponNotFound
	}
	now := time.Now()
	if v.Status == model.CouponStatusUsed {
		return nil, ErrCouponUsed
	}
	if now.Before(v.ValidFrom) || now.After(v.ValidUntil) {
		return nil, ErrCouponExpired
	}
	return v, nil
}

// UseCoupon 事务内核销（条件更新 unused→used + 有效期窗口），事务由 order
// 模块开启并提交。条件更新失败时重查区分原因：已用 / 过期 / 不存在，
// 避免"结算通过但事务内过期"被误报为已用。
func (s *couponService) UseCoupon(ctx context.Context, tx *gorm.DB, userID, couponID int64) error {
	ok, err := s.store.UserCoupon.Use(ctx, tx, userID, couponID)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	v, err := s.store.UserCoupon.GetViewByID(ctx, userID, couponID)
	if err != nil {
		return err
	}
	if v == nil {
		return ErrCouponNotFound
	}
	if v.Status == model.CouponStatusUsed {
		return ErrCouponUsed
	}
	return ErrCouponExpired
}

// RollbackCoupon 事务内回退（条件更新 used→unused），取消订单回退券。
func (s *couponService) RollbackCoupon(ctx context.Context, tx *gorm.DB, userID, couponID int64) error {
	ok, err := s.store.UserCoupon.Rollback(ctx, tx, userID, couponID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: coupon %d", ErrCouponRollbackFailed, couponID)
	}
	return nil
}

// ---- 校验 ----

func validateTemplate(p *TemplateParams) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 64 {
		return fmt.Errorf("%w: invalid name", ErrInvalidInput)
	}
	if p.Type != model.TemplateTypeDirect && p.Type != model.TemplateTypeThreshold {
		return fmt.Errorf("%w: invalid type", ErrInvalidInput)
	}
	if p.Value <= 0 {
		return fmt.Errorf("%w: invalid value", ErrInvalidInput)
	}
	if p.Type == model.TemplateTypeThreshold {
		if p.MinAmount < p.Value {
			return fmt.Errorf("%w: threshold must be >= value", ErrInvalidInput)
		}
	} else {
		p.MinAmount = 0
	}
	if p.Total < 1 {
		return fmt.Errorf("%w: invalid total", ErrInvalidInput)
	}
	if p.PerUserLimit < 1 {
		return fmt.Errorf("%w: invalid per_user_limit", ErrInvalidInput)
	}
	if !p.ValidFrom.Before(p.ValidUntil) {
		return fmt.Errorf("%w: valid_until must be after valid_from", ErrInvalidInput)
	}
	return nil
}
