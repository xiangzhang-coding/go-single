// Package service 承载 coupon 模块业务：admin 发布券模板，
// 用户浏览可领券并领取（缓存原子能力防超发），查看我的券（未用/已用/过期派生）。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/coupon/model"
	"github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrTemplateNotFound  = errors.New("coupon template not found")
	ErrInvalidInput      = errors.New("invalid input")
	ErrNotInWindow       = errors.New("coupon not in valid period")
	ErrSoldOut           = errors.New("coupon sold out")
	ErrClaimLimitReached = errors.New("claim limit reached")

	ErrCouponNotFound        = errors.New("coupon not found")
	ErrCouponUsed            = errors.New("coupon already used")
	ErrCouponExpired         = errors.New("coupon is not valid at this time")
	ErrCouponThresholdNotMet = errors.New("coupon threshold not met")
	ErrCouponRollbackFailed  = errors.New("coupon not in used state, rollback failed")
)

// 可领券列表的状态取值。
const (
	stateClaimable      = "claimable"
	stateNotStarted     = "not_started"
	stateEnded          = "ended"
	stateSoldOut        = "sold_out"
	stateLimitReached   = "limit_reached"
	couponCacheTimeout  = 250 * time.Millisecond
	couponRepairTimeout = 2 * time.Second
)

// Redis key 约定：coupon:claimed:{template_id}（总量计数）/
// coupon:peruser:{template_id}:{user_id}（每人限领计数）/
// coupon:version:{template_id}（总计数的 MySQL 版本）/
// coupon:peruser-version:{template_id}:{user_id}（用户计数的 MySQL 版本）。
func claimedKey(templateID int64) string { return fmt.Sprintf("coupon:claimed:%d", templateID) }
func perUserKey(templateID, userID int64) string {
	return fmt.Sprintf("coupon:peruser:%d:%d", templateID, userID)
}
func versionKey(templateID int64) string { return fmt.Sprintf("coupon:version:%d", templateID) }
func perUserVersionKey(templateID, userID int64) string {
	return fmt.Sprintf("coupon:peruser-version:%d:%d", templateID, userID)
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
	// Claim 领取：Redis 快速计数，MySQL 事务作为总量与每人限领的最终约束。
	Claim(ctx context.Context, userID, templateID int64) (*model.UserCoupon, error)
	// ListMine 我的券（status 空 = 全部；unused/used/expired 筛选）。
	ListMine(ctx context.Context, userID int64, status string, page, pageSize int) ([]model.UserCouponView, int64, error)
	// RedeemForOrder locks user_coupon + coupon_template in the order transaction,
	// validates ownership/status/window/threshold, redeems the coupon, and returns
	// the exact value snapshot used by that order's amount calculation.
	RedeemForOrder(ctx context.Context, tx *transaction.Handle, userID, couponID, totalAmount int64) (model.CouponRedemption, error)
	// RollbackCoupon 事务内条件回退（used→unused），供 order 模块取消订单调用。
	RollbackCoupon(ctx context.Context, tx *transaction.Handle, userID, couponID int64) error
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
// 计数取 DB 已领数；领取时同一数据库事实也作为缓存重建基线和最终约束。
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

// Claim 领取流程：读取 DB 事实作为 Redis 重建基线 → 缓存原子计数 →
// MySQL 锁定模板并在同一事务内重查总量、每人限领与有效期后落库。
// Redis 的拒绝或故障不单独决定结果，避免缓存丢失或陈旧造成超发/虚假售罄。
func (s *couponService) Claim(ctx context.Context, userID, templateID int64) (*model.UserCoupon, error) {
	t, err := s.store.Template.GetByID(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrTemplateNotFound
	}
	claimedCount, err := s.store.UserCoupon.CountByTemplate(ctx, templateID)
	if err != nil {
		return nil, err
	}
	perUserCount, err := s.store.UserCoupon.CountUserByTemplate(ctx, userID, templateID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	cacheCtx, cancelCache := context.WithTimeout(ctx, couponCacheTimeout)
	_, _ = s.cache.ClaimCoupon(cacheCtx, cache.CouponClaimParams{
		ClaimedKey: claimedKey(templateID), PerUserKey: perUserKey(templateID, userID),
		VersionKey: versionKey(templateID), PerUserVersionKey: perUserVersionKey(templateID, userID),
		Now: now, Total: t.Total, ValidFrom: t.ValidFrom,
		ValidUntil: t.ValidUntil, PerUserLimit: t.PerUserLimit,
		ClaimedCount: claimedCount, PerUserCount: perUserCount,
	})
	cancelCache()

	outcome, err := s.store.UserCoupon.Claim(ctx, userID, templateID)
	if err != nil {
		s.repairCouponCounts(ctx, templateID, userID)
		return nil, err
	}
	s.syncCouponCounts(ctx, templateID, userID, outcome.ClaimedCount, outcome.PerUserCount)

	switch outcome.Result {
	case repository.ClaimCreated:
		// 发放打点（T19c）：领取成功落库后计数。
		s.metrics.CouponIssued()
		return outcome.Coupon, nil
	case repository.ClaimTemplateNotFound:
		return nil, ErrTemplateNotFound
	case repository.ClaimSoldOut:
		return nil, ErrSoldOut
	case repository.ClaimNotInWindow:
		return nil, ErrNotInWindow
	case repository.ClaimLimitReached:
		return nil, ErrClaimLimitReached
	default:
		return nil, fmt.Errorf("%w: unexpected claim result %d", ErrInvalidInput, outcome.Result)
	}
}

func (s *couponService) syncCouponCounts(parent context.Context, templateID, userID, claimedCount, perUserCount int64) {
	ctx, cancel := context.WithTimeout(parent, couponRepairTimeout)
	defer cancel()
	_ = s.cache.SyncCouponCounts(ctx, cache.CouponCountParams{
		ClaimedKey: claimedKey(templateID), PerUserKey: perUserKey(templateID, userID),
		VersionKey: versionKey(templateID), PerUserVersionKey: perUserVersionKey(templateID, userID),
		ClaimedCount: claimedCount, PerUserCount: perUserCount,
	})
}

func (s *couponService) repairCouponCounts(parent context.Context, templateID, userID int64) {
	ctx, cancel := context.WithTimeout(parent, couponRepairTimeout)
	defer cancel()
	claimedCount, err := s.store.UserCoupon.CountByTemplate(ctx, templateID)
	if err != nil {
		return
	}
	perUserCount, err := s.store.UserCoupon.CountUserByTemplate(ctx, userID, templateID)
	if err != nil {
		return
	}
	_ = s.cache.SyncCouponCounts(ctx, cache.CouponCountParams{
		ClaimedKey: claimedKey(templateID), PerUserKey: perUserKey(templateID, userID),
		VersionKey: versionKey(templateID), PerUserVersionKey: perUserVersionKey(templateID, userID),
		ClaimedCount: claimedCount, PerUserCount: perUserCount,
	})
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

func (s *couponService) RedeemForOrder(ctx context.Context, tx *transaction.Handle, userID, couponID, totalAmount int64) (model.CouponRedemption, error) {
	facts, err := s.store.UserCoupon.GetRedemptionForUpdate(ctx, tx, couponID)
	if err != nil {
		return model.CouponRedemption{}, err
	}
	if facts == nil || facts.UserID != userID {
		return model.CouponRedemption{}, ErrCouponNotFound
	}
	if facts.Status != model.CouponStatusUnused {
		return model.CouponRedemption{}, ErrCouponUsed
	}
	now := time.Now()
	if now.Before(facts.ValidFrom) || now.After(facts.ValidUntil) {
		return model.CouponRedemption{}, ErrCouponExpired
	}
	if totalAmount < facts.MinAmount {
		return model.CouponRedemption{}, ErrCouponThresholdNotMet
	}
	ok, err := s.store.UserCoupon.Use(ctx, tx, userID, couponID)
	if err != nil {
		return model.CouponRedemption{}, err
	}
	if !ok {
		return model.CouponRedemption{}, ErrCouponExpired
	}
	// 条件更新命中即计数；调用方事务回滚时乐观多计（可接受）。
	s.metrics.CouponRedeemed()
	return model.CouponRedemption{CouponID: facts.CouponID, Value: facts.Value}, nil
}

// RollbackCoupon 事务内回退（条件更新 used→unused），取消订单回退券。
func (s *couponService) RollbackCoupon(ctx context.Context, tx *transaction.Handle, userID, couponID int64) error {
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
