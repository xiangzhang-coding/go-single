package repository

import (
	"context"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

type GORMPreDeductionRepository struct {
	db *gorm.DB
}

func NewGORMPreDeduction(db *gorm.DB) *GORMPreDeductionRepository {
	return &GORMPreDeductionRepository{db: db}
}

func (r *GORMPreDeductionRepository) Create(ctx context.Context, p *model.PreDeduction) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(p).Error; err != nil {
			return err
		}
		if p.PurchaseSlot == 0 {
			p.PurchaseSlot = p.ID
			return tx.Model(p).Update("purchase_slot", p.PurchaseSlot).Error
		}
		return nil
	})
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return ErrPreDeductionDuplicate
	}
	return err
}

func (r *GORMPreDeductionRepository) GetByID(ctx context.Context, id int64) (*model.PreDeduction, error) {
	return r.getByID(r.db.WithContext(ctx), id)
}

func (r *GORMPreDeductionRepository) GetByRequestID(ctx context.Context, userID, activityID int64, requestID string) (*model.PreDeduction, error) {
	var p model.PreDeduction
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND activity_id = ? AND client_request_id = ?", userID, activityID, requestID).
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *GORMPreDeductionRepository) GetByIDForUpdate(ctx context.Context, handle *transaction.Handle, id int64) (*model.PreDeduction, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return nil, err
	}
	return r.getByID(tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}), id)
}

func (r *GORMPreDeductionRepository) EnsureLegacyPendingOrder(ctx context.Context, seed *model.PreDeduction) (*model.PreDeduction, error) {
	db := r.db.WithContext(ctx)
	var result *model.PreDeduction
	err := db.Transaction(func(tx *gorm.DB) error {
		var existing model.PreDeduction
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", seed.OrderNumber()).First(&existing).Error
		if err == nil {
			if existing.PurchaseSlot == 0 {
				existing.PurchaseSlot = existing.ID
				if err := tx.Model(&existing).Update("purchase_slot", existing.PurchaseSlot).Error; err != nil {
					return err
				}
			}
			result = &existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		seed.Status = model.PreDeductionStatusPendingOrder
		seed.Legacy = true
		if err := tx.Create(seed).Error; err != nil {
			return err
		}
		seed.PurchaseSlot = seed.ID
		if err := tx.Model(seed).Update("purchase_slot", seed.PurchaseSlot).Error; err != nil {
			return err
		}
		result = seed
		return nil
	})
	if err == nil {
		return result, nil
	}
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
		return nil, err
	}
	var existing model.PreDeduction
	if getErr := db.Where("order_no = ?", seed.OrderNumber()).First(&existing).Error; getErr == nil {
		if existing.PurchaseSlot == 0 {
			return nil, errors.New("legacy pre-deduction purchase slot is not initialized")
		}
		return &existing, nil
	}
	return nil, err
}

func (r *GORMPreDeductionRepository) ReservationTargets(ctx context.Context, activityID, userID int64) (int, int, error) {
	var pending, user int
	db := r.db.WithContext(ctx).Model(&model.PreDeduction{})
	if err := db.Select("COALESCE(SUM(quantity), 0)").
		Where("activity_id = ? AND status IN ?", activityID, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).Scan(&pending).Error; err != nil {
		return 0, 0, err
	}
	if err := db.Select("COUNT(*)").
		Where("activity_id = ? AND user_id = ? AND status IN ?", activityID, userID, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusOrdered, model.PreDeductionStatusPendingRollback,
		}).Scan(&user).Error; err != nil {
		return 0, 0, err
	}
	return pending, user, nil
}

func (r *GORMPreDeductionRepository) PendingReservationQuantityForUpdate(ctx context.Context, handle *transaction.Handle, activityID int64) (int, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return 0, err
	}
	var rows []model.PreDeduction
	err = tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "quantity").
		Where("activity_id = ? AND status IN ?", activityID, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).Find(&rows).Error
	if err != nil {
		return 0, err
	}
	total := 0
	for i := range rows {
		total += rows[i].Quantity
	}
	return total, nil
}

func (r *GORMPreDeductionRepository) HasAcceptedReservationForUpdate(ctx context.Context, handle *transaction.Handle, activityID int64) (bool, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return false, err
	}
	var rows []model.PreDeduction
	err = tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("activity_id = ? AND status IN ?", activityID, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusOrdered,
			model.PreDeductionStatusPendingRollback,
		}).Limit(1).Find(&rows).Error
	return len(rows) > 0, err
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

func (r *GORMPreDeductionRepository) ListRecoverableByActivity(ctx context.Context, activityID int64) ([]model.PreDeduction, error) {
	var list []model.PreDeduction
	err := r.db.WithContext(ctx).
		Where("activity_id = ? AND status IN ?", activityID, []model.PreDeductionStatus{
			model.PreDeductionStatusPreparing,
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).Order("updated_at ASC, id ASC").Find(&list).Error
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
		SET status = CASE WHEN status = ? AND publish_attempts + 1 >= ? THEN ? ELSE status END,
		    publish_attempts = publish_attempts + 1,
		    last_error = ?, updated_at = ?
		WHERE id = ? AND status IN ?`,
		model.PreDeductionStatusPendingPublish, maxAttempts, model.PreDeductionStatusPendingRollback,
		detail, time.Now(), id,
		[]model.PreDeductionStatus{model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder},
	))
}

func (r *GORMPreDeductionRepository) MarkOrdered(ctx context.Context, handle *transaction.Handle, id int64) error {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return err
	}
	now := time.Now()
	return requirePreDeductionTransition(tx.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
		}).Updates(map[string]any{
		"status": model.PreDeductionStatusOrdered, "ordered_at": now, "last_error": "",
	}))
}

func (r *GORMPreDeductionRepository) MarkPendingRollback(ctx context.Context, id int64, detail string) error {
	return r.db.WithContext(ctx).Model(&model.PreDeduction{}).
		Where("id = ? AND status IN ?", id, []model.PreDeductionStatus{
			model.PreDeductionStatusPreparing,
			model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusPendingRollback,
		}).
		Updates(map[string]any{"status": model.PreDeductionStatusPendingRollback, "last_error": detail}).Error
}

func (r *GORMPreDeductionRepository) EnsurePendingRollback(ctx context.Context, handle *transaction.Handle, seed *model.PreDeduction) (*model.PreDeduction, error) {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return nil, err
	}
	db := tx.WithContext(ctx)
	var p model.PreDeduction
	err = db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_no = ?", seed.OrderNumber()).First(&p).Error
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
