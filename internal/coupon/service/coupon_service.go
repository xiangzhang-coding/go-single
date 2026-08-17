// Package service 承载 coupon 模块业务：admin 发布券模板，
// 用户浏览可领券并领取（缓存原子能力防超发），查看我的券（未用/已用/过期派生）。
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
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
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
	// ListTemplates 后台券模板列表（T25 富化）：全量模板 + 已领数（claimed_count，
	// 供后台展示 已领/总量 核销进度）。
	ListTemplates(ctx context.Context) ([]model.CouponTemplateView, error)

	// ---- 用户 ----
	// ListClaimable 可领券列表（含当前用户视角的领取状态）。
	ListClaimable(ctx context.Context, userID int64) ([]model.CouponTemplateView, error)
	// Claim 领取：缓存原子校验（有效期/总量/每人限领）后 DB 落库为最终态。
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
	store   repository.Store
	cache   cache.CouponStore
	metrics *metrics.Business
}

// New 构造优惠券服务。
func New(store repository.Store, c cache.CouponStore, m *metrics.Business) Service {
	return &couponService{store: store, cache: c, metrics: m}
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

func (s *couponService) ListTemplates(ctx context.Context) ([]model.CouponTemplateView, error) {
	templates, err := s.store.Template.List(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]model.CouponTemplateView, 0, len(templates))
	for i := range templates {
		t := &templates[i]
		claimed, err := s.store.UserCoupon.CountByTemplate(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, model.CouponTemplateView{CouponTemplate: *t, ClaimedCount: claimed})
	}
	return views, nil
}

// ---- 用户 ----

// ListClaimable 状态判定：
// not_started（未开始）/ ended（已结束）/ sold_out（总量已领完）/
// limit_reached（用户已达每人限领）/ claimable（可领）。
// 计数取 DB 已领数，仅作展示；防超发的强制校验由缓存适配器原子完成。
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

// Claim 领取流程：DB 读模板（含有效期快照）→ 缓存原子计数 → DB 落库最终态。
// 模板过期/不存在由 DB 校验，其余并发条件由缓存适配器原子强制。
func (s *couponService) Claim(ctx context.Context, userID, templateID int64) (*model.UserCoupon, error) {
	t, err := s.store.Template.GetByID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTemplateNotFound
	}

	now := time.Now()
	result, err := s.cache.ClaimCoupon(ctx, cache.CouponClaimParams{
		ClaimedKey: claimedKey(templateID), PerUserKey: perUserKey(templateID, userID),
		Now: now, Total: t.Total, ValidFrom: t.ValidFrom,
		ValidUntil: t.ValidUntil, PerUserLimit: t.PerUserLimit,
	})
	if err != nil {
		return nil, err
	}

	switch result {
	case cache.CouponClaimed:
	case cache.CouponSoldOut:
		return nil, ErrSoldOut
	case cache.CouponNotInWindow:
		return nil, ErrNotInWindow
	case cache.CouponLimitReached:
		return nil, ErrClaimLimitReached
	default:
		return nil, fmt.Errorf("%w: unexpected claim result %d", ErrInvalidInput, result)
	}

	c := &model.UserCoupon{UserID: userID, TemplateID: templateID, Status: model.CouponStatusUnused}
	if err := s.store.UserCoupon.Create(ctx, c); err != nil {
		return nil, err
	}
	// 发放打点（T19c）：领取成功落库后计数。
	s.metrics.CouponIssued()
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
		// 核销打点（T19c）：条件更新命中即计数；调用方（order）事务回滚时乐观多计（可接受）。
		s.metrics.CouponRedeemed()
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
