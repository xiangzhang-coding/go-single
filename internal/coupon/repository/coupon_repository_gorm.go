package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiangzhang-coding/go-single/internal/coupon/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// derivedStatusExpr 派生状态 SQL 片段：used 落库优先；未用且已过有效期 → expired。
// 有效期上界经 now 参数传入（DATETIME 按 Go 本地墙钟写入，与 MySQL 服务器
// 时区解耦，见 Use 注释）；SQL 内不再使用 NOW(3)。
func derivedStatusExpr(now time.Time) string {
	return "CASE WHEN uc.status = '" + model.CouponStatusUsed + "' THEN '" + model.CouponStatusUsed +
		"' WHEN t.valid_until < ? THEN '" + model.CouponStatusExpired + "' ELSE '" + model.CouponStatusUnused + "' END"
}

// listByUser 查询条件（含派生状态筛选）：now 为 Go 当前时间。
func applyStatusFilters(q *gorm.DB, status string, now time.Time) *gorm.DB {
	if status != "" {
		if status == model.CouponStatusUnused || status == model.CouponStatusExpired {
			q = q.Where("uc.status = ?", model.CouponStatusUnused)
		} else {
			q = q.Where("uc.status = ?", status)
		}
	}
	switch status {
	case model.CouponStatusUnused:
		q = q.Where("t.valid_until >= ?", now)
	case model.CouponStatusExpired:
		q = q.Where("t.valid_until < ?", now)
	}
	return q
}

// GORMCouponTemplateRepository 券模板仓储（GORM 实现）。
type GORMCouponTemplateRepository struct {
	db *gorm.DB
}

// NewGORMCouponTemplate 构造券模板仓储。
func NewGORMCouponTemplate(db *gorm.DB) *GORMCouponTemplateRepository {
	return &GORMCouponTemplateRepository{db: db}
}

func (r *GORMCouponTemplateRepository) Create(ctx context.Context, t *model.CouponTemplate) error {
	return r.db.WithContext(ctx).Create(t).Error
}

func (r *GORMCouponTemplateRepository) Update(ctx context.Context, t *model.CouponTemplate) error {
	return r.db.WithContext(ctx).Model(t).Select("name", "type", "value", "min_amount", "total", "per_user_limit", "valid_from", "valid_until").Updates(t).Error
}

func (r *GORMCouponTemplateRepository) GetByID(ctx context.Context, id int64) (*model.CouponTemplate, error) {
	var t model.CouponTemplate
	if err := r.db.WithContext(ctx).First(&t, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

func (r *GORMCouponTemplateRepository) List(ctx context.Context) ([]model.CouponTemplate, error) {
	var list []model.CouponTemplate
	if err := r.db.WithContext(ctx).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// GORMUserCouponRepository 用户券仓储（GORM 实现）。
type GORMUserCouponRepository struct {
	db *gorm.DB
}

// NewGORMUserCoupon 构造用户券仓储。
func NewGORMUserCoupon(db *gorm.DB) *GORMUserCouponRepository {
	return &GORMUserCouponRepository{db: db}
}

func (r *GORMUserCouponRepository) Claim(ctx context.Context, userID, templateID int64) (ClaimOutcome, error) {
	var outcome ClaimOutcome
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var template model.CouponTemplate
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&template, templateID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			outcome.Result = ClaimTemplateNotFound
			return nil
		}
		if err != nil {
			return err
		}

		if err := tx.Model(&model.UserCoupon{}).Where("template_id = ?", templateID).Count(&outcome.ClaimedCount).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.UserCoupon{}).
			Where("user_id = ? AND template_id = ?", userID, templateID).
			Count(&outcome.PerUserCount).Error; err != nil {
			return err
		}

		now := time.Now()
		switch {
		case now.Before(template.ValidFrom) || now.After(template.ValidUntil):
			outcome.Result = ClaimNotInWindow
		case outcome.ClaimedCount >= int64(template.Total):
			outcome.Result = ClaimSoldOut
		case outcome.PerUserCount >= int64(template.PerUserLimit):
			outcome.Result = ClaimLimitReached
		default:
			coupon := &model.UserCoupon{UserID: userID, TemplateID: templateID, Status: model.CouponStatusUnused}
			if err := tx.Create(coupon).Error; err != nil {
				return err
			}
			outcome.Result = ClaimCreated
			outcome.Coupon = coupon
			outcome.ClaimedCount++
			outcome.PerUserCount++
		}
		return nil
	})
	return outcome, err
}

// ListByUser 单条 SQL 完成 JOIN + 派生状态 + 筛选，避免服务层 N+1 查询模板。
// 派生状态与有效期筛选共用同一 Go 时间（now），保证一次查询内自洽。
func (r *GORMUserCouponRepository) ListByUser(ctx context.Context, userID int64, status string, offset, limit int) ([]model.UserCouponView, int64, error) {
	now := time.Now()
	q := r.db.WithContext(ctx).
		Table("user_coupons AS uc").
		Joins("JOIN coupon_templates AS t ON t.id = uc.template_id").
		Where("uc.user_id = ?", userID)
	q = applyStatusFilters(q, status, now)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	rows, err := q.Select(
		"uc.id, uc.template_id, t.name, t.type, t.value, t.min_amount, "+
			derivedStatusExpr(now)+" AS status, "+
			"t.valid_from, t.valid_until, uc.used_at, uc.created_at", now).
		Order("uc.id DESC").Offset(offset).Limit(limit).Rows()
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var list []model.UserCouponView
	for rows.Next() {
		var v model.UserCouponView
		if err := r.db.ScanRows(rows, &v); err != nil {
			return nil, 0, err
		}
		list = append(list, v)
	}
	return list, total, rows.Err()
}

func (r *GORMUserCouponRepository) CountByTemplate(ctx context.Context, templateID int64) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.UserCoupon{}).
		Where("template_id = ?", templateID).Count(&n).Error
	return n, err
}

func (r *GORMUserCouponRepository) CountUserByTemplate(ctx context.Context, userID, templateID int64) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.UserCoupon{}).
		Where("user_id = ? AND template_id = ?", userID, templateID).Count(&n).Error
	return n, err
}

// GetViewByID 单张券查询（归属过滤），与 ListByUser 同构的 JOIN + 派生状态。
func (r *GORMUserCouponRepository) GetViewByID(ctx context.Context, userID, couponID int64) (*model.UserCouponView, error) {
	now := time.Now()
	var v model.UserCouponView
	err := r.db.WithContext(ctx).
		Table("user_coupons AS uc").
		Joins("JOIN coupon_templates AS t ON t.id = uc.template_id").
		Select(
			"uc.id, uc.template_id, t.name, t.type, t.value, t.min_amount, "+
				derivedStatusExpr(now)+" AS status, "+
				"t.valid_from, t.valid_until, uc.used_at, uc.created_at", now).
		Where("uc.id = ? AND uc.user_id = ?", couponID, userID).
		Scan(&v).Error
	if err != nil {
		return nil, err
	}
	if v.ID == 0 {
		return nil, nil
	}
	return &v, nil
}

// Use 条件核销：unused→used 单条 UPDATE（含模板有效期窗口，原子校验防
// 结算后券恰好过期仍被核销）；RowsAffected=0 即券已用/过期/不存在。
// 窗口上界与 used_at 均经 Go 传入而非 NOW(3)：DATETIME 按 Go 本地墙钟写入
// （go-sql-driver 的 loc=Local 行为），与 MySQL 服务器时区解耦，任何时区组合都自洽。
func (r *GORMUserCouponRepository) Use(ctx context.Context, handle *transaction.Handle, userID, couponID int64) (bool, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return false, err
	}
	now := time.Now()
	res := tx.WithContext(ctx).Model(&model.UserCoupon{}).
		Where("id = ? AND user_id = ? AND status = ?", couponID, userID, model.CouponStatusUnused).
		Where("EXISTS (SELECT 1 FROM coupon_templates t WHERE t.id = user_coupons.template_id "+
			"AND t.valid_from <= ? AND t.valid_until >= ?)", now, now).
		Updates(map[string]any{"status": model.CouponStatusUsed, "used_at": now})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// Rollback 条件回退：used→unused，清空 used_at。
func (r *GORMUserCouponRepository) Rollback(ctx context.Context, handle *transaction.Handle, userID, couponID int64) (bool, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return false, err
	}
	res := tx.WithContext(ctx).Model(&model.UserCoupon{}).
		Where("id = ? AND user_id = ? AND status = ?", couponID, userID, model.CouponStatusUsed).
		Updates(map[string]any{"status": model.CouponStatusUnused, "used_at": nil})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// 编译期断言：GORM 实现满足仓储接口。
var _ CouponTemplateRepository = (*GORMCouponTemplateRepository)(nil)
var _ UserCouponRepository = (*GORMUserCouponRepository)(nil)
