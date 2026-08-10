package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/coupon/model"
)

// 派生状态 SQL 片段：used 落库优先；未用且已过有效期 → expired。
const derivedStatusExpr = "CASE WHEN uc.status = '" + model.CouponStatusUsed + "' THEN '" + model.CouponStatusUsed +
	"' WHEN t.valid_until < NOW(3) THEN '" + model.CouponStatusExpired + "' ELSE '" + model.CouponStatusUnused + "' END"

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

func (r *GORMUserCouponRepository) Create(ctx context.Context, c *model.UserCoupon) error {
	return r.db.WithContext(ctx).Create(c).Error
}

// ListByUser 单条 SQL 完成 JOIN + 派生状态 + 筛选，避免服务层 N+1 查询模板。
func (r *GORMUserCouponRepository) ListByUser(ctx context.Context, userID int64, status string, offset, limit int) ([]model.UserCouponView, int64, error) {
	q := r.db.WithContext(ctx).
		Table("user_coupons AS uc").
		Joins("JOIN coupon_templates AS t ON t.id = uc.template_id").
		Where("uc.user_id = ?", userID)

	if status != "" {
		if status == model.CouponStatusUnused || status == model.CouponStatusExpired {
			// 未用/过期需结合有效期判定（expired = 未用且已过期）。
			q = q.Where("uc.status = ?", model.CouponStatusUnused)
		} else {
			q = q.Where("uc.status = ?", status)
		}
	}

	// 派生状态筛选：unused 排除已过期；expired 仅未用且过期；used 无需额外条件。
	switch status {
	case model.CouponStatusUnused:
		q = q.Where("t.valid_until >= NOW(3)")
	case model.CouponStatusExpired:
		q = q.Where("t.valid_until < NOW(3)")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	rows, err := q.Select(
		"uc.id, uc.template_id, t.name, t.type, t.value, t.min_amount, " +
			derivedStatusExpr + " AS status, t.valid_from, t.valid_until, uc.used_at, uc.created_at",
	).Order("uc.id DESC").Offset(offset).Limit(limit).Rows()
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
	var v model.UserCouponView
	err := r.db.WithContext(ctx).
		Table("user_coupons AS uc").
		Joins("JOIN coupon_templates AS t ON t.id = uc.template_id").
		Select(
			"uc.id, uc.template_id, t.name, t.type, t.value, t.min_amount, "+
				derivedStatusExpr+" AS status, t.valid_from, t.valid_until, uc.used_at, uc.created_at").
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
// 窗口上界经 Go 传入而非 NOW(3)：DATETIME 按 Go 本地墙钟写入（go-sql-driver
// 的 loc=Local 行为），与 MySQL 服务器时区解耦，任何时区组合都自洽。
func (r *GORMUserCouponRepository) Use(ctx context.Context, tx *gorm.DB, userID, couponID int64) (bool, error) {
	now := time.Now()
	res := tx.WithContext(ctx).Model(&model.UserCoupon{}).
		Where("id = ? AND user_id = ? AND status = ?", couponID, userID, model.CouponStatusUnused).
		Where("EXISTS (SELECT 1 FROM coupon_templates t WHERE t.id = user_coupons.template_id "+
			"AND t.valid_from <= ? AND t.valid_until >= ?)", now, now).
		Updates(map[string]any{"status": model.CouponStatusUsed, "used_at": gorm.Expr("NOW(3)")})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// Rollback 条件回退：used→unused，清空 used_at。
func (r *GORMUserCouponRepository) Rollback(ctx context.Context, tx *gorm.DB, userID, couponID int64) (bool, error) {
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
