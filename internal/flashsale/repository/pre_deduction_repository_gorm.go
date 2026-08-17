package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
)

type GORMPreDeductionRepository struct {
	db *gorm.DB
}

func NewGORMPreDeduction(db *gorm.DB) *GORMPreDeductionRepository {
	return &GORMPreDeductionRepository{db: db}
}

func (r *GORMPreDeductionRepository) Create(ctx context.Context, p *model.PreDeduction) error {
	return r.db.WithContext(ctx).Create(p).Error
}

func (r *GORMPreDeductionRepository) GetByID(ctx context.Context, id int64) (*model.PreDeduction, error) {
	return r.getByID(r.db.WithContext(ctx), id)
}

func (r *GORMPreDeductionRepository) GetByIDForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*model.PreDeduction, error) {
	return r.getByID(r.exec(ctx, tx).Clauses(clause.Locking{Strength: "UPDATE"}), id)
}

func (r *GORMPreDeductionRepository) getByID(db *gorm.DB, id int64) (*model.PreDeduction, error) {
	var p model.PreDeduction
	if err := db.First(&p, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (r *GORMPreDeductionRepository) ListRecoverable(ctx context.Context, limit int) ([]model.PreDeduction, error) {
	var list []model.PreDeduction
	query := r.db.WithContext(ctx).
		Where("status IN ?", []model.PreDeductionStatus{
			model.PreDeductionStatusPreparing,
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).Order("updated_at ASC, id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	err := query.Find(&list).Error
	return list, err
}

func (r *GORMPreDeductionRepository) ListOrdered(ctx context.Context, limit int) ([]model.PreDeduction, error) {
	query := r.db.WithContext(ctx).
		Where("status = ? AND reservation_released_at IS NULL", model.PreDeductionStatusOrdered).
		Order("updated_at ASC, id ASC")
	if limit > 0 {
		query = query.Limit(limit)
	}
	var list []model.PreDeduction
	err := query.Find(&list).Error
	return list, err
}

func (r *GORMPreDeductionRepository) MarkPreDeducted(ctx context.Context, id int64) error {
	return requirePreDeductionTransition(r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status = ?", id, model.PreDeductionStatusPreparing).
		Updates(map[string]any{"status": model.PreDeductionStatusPendingPublish, "last_error": ""}))
}

func (r *GORMPreDeductionRepository) SetOrderNo(ctx context.Context, id int64, orderNo string) error {
	return requirePreDeductionTransition(r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND order_no IS NULL AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
		}).Update("order_no", orderNo))
}

func (r *GORMPreDeductionRepository) MarkPendingOrder(ctx context.Context, id int64) error {
	return requirePreDeductionTransition(r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
		}).Updates(map[string]any{"status": model.PreDeductionStatusPendingOrder, "last_error": ""}))
}

func (r *GORMPreDeductionRepository) RecordPublishFailure(ctx context.Context, id int64, maxAttempts int, detail string) error {
	return requirePreDeductionTransition(r.db.WithContext(ctx).Exec(`
		UPDATE flashsale_pre_deductions
		SET status = CASE WHEN publish_attempts + 1 >= ? THEN ? ELSE status END,
		    publish_attempts = publish_attempts + 1,
		    last_error = ?, updated_at = ?
		WHERE id = ? AND status IN ?`,
		maxAttempts, model.PreDeductionStatusPendingRollback, detail, time.Now(), id,
		[]model.PreDeductionStatus{model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder},
	))
}

func (r *GORMPreDeductionRepository) MarkOrdered(ctx context.Context, tx *gorm.DB, id int64) error {
	now := time.Now()
	return requirePreDeductionTransition(r.exec(ctx, tx).Model(&model.PreDeduction{}).
		Where("id = ? AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
		}).Updates(map[string]any{
		"status": model.PreDeductionStatusOrdered, "ordered_at": now, "last_error": "",
	}))
}

func (r *GORMPreDeductionRepository) MarkPendingRollback(ctx context.Context, tx *gorm.DB, id int64, detail string) error {
	return r.exec(ctx, tx).Model(&model.PreDeduction{}).
		Where("id = ? AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPreparing,
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).
		Updates(map[string]any{"status": model.PreDeductionStatusPendingRollback, "last_error": detail}).Error
}

func (r *GORMPreDeductionRepository) EnsurePendingRollback(ctx context.Context, tx *gorm.DB, seed *model.PreDeduction) (*model.PreDeduction, error) {
	db := r.exec(ctx, tx)
	var p model.PreDeduction
	err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_no = ?", seed.OrderNumber()).First(&p).Error
	if err == nil {
		if err := db.Model(&model.PreDeduction{}).
			Where("id = ? AND status <> ?", p.ID, model.PreDeductionStatusRolledBack).
			Updates(map[string]any{
				"status": model.PreDeductionStatusPendingRollback, "last_error": seed.LastError,
			}).Error; err != nil {
			return nil, err
		}
		p.Status = model.PreDeductionStatusPendingRollback
		p.LastError = seed.LastError
		return &p, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	seed.Status = model.PreDeductionStatusPendingRollback
	seed.Legacy = true
	if err := db.Create(seed).Error; err != nil {
		return nil, err
	}
	return seed, nil
}

func (r *GORMPreDeductionRepository) MarkRolledBack(ctx context.Context, id int64) error {
	now := time.Now()
	return requirePreDeductionTransition(r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPreparing, model.PreDeductionStatusPendingRollback,
		}).Updates(map[string]any{
		"status": model.PreDeductionStatusRolledBack, "rolled_back_at": now, "last_error": "",
	}))
}

func (r *GORMPreDeductionRepository) MarkReservationReleased(ctx context.Context, id int64) error {
	return requirePreDeductionTransition(r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status = ? AND reservation_released_at IS NULL", id, model.PreDeductionStatusOrdered).
		Update("reservation_released_at", time.Now()))
}

func (r *GORMPreDeductionRepository) RecordRollbackFailure(ctx context.Context, id int64, detail string) error {
	return r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status = ?", id, model.PreDeductionStatusPendingRollback).
		Updates(map[string]any{
			"rollback_attempts": gorm.Expr("rollback_attempts + 1"), "last_error": detail,
		}).Error
}

func (r *GORMPreDeductionRepository) HasUnresolved(ctx context.Context, activityID int64) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("activity_id = ? AND status IN ?", activityID, []model.PreDeductionStatus{
			model.PreDeductionStatusPreparing,
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).Count(&count).Error
	return count > 0, err
}

func (r *GORMPreDeductionRepository) exec(ctx context.Context, tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx.WithContext(ctx)
	}
	return r.db.WithContext(ctx)
}

func requirePreDeductionTransition(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrPreDeductionStateChanged
	}
	return nil
}

var _ PreDeductionRepository = (*GORMPreDeductionRepository)(nil)
