package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
)

var ErrPreDeductionStateChanged = errors.New("pre-deduction state changed")

// PreDeductionRepository persists the recoverable flash-sale lifecycle.
type PreDeductionRepository interface {
	Create(ctx context.Context, p *model.PreDeduction) error
	GetByID(ctx context.Context, id int64) (*model.PreDeduction, error)
	GetByIDForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*model.PreDeduction, error)
	EnsureLegacyPendingOrder(ctx context.Context, seed *model.PreDeduction) (*model.PreDeduction, error)
	ReservationTargets(ctx context.Context, activityID, userID int64) (pendingQuantity, userQuantity int, err error)
	ListRecoverable(ctx context.Context, limit int) ([]model.PreDeduction, error)
	ListOrdered(ctx context.Context, limit int) ([]model.PreDeduction, error)
	MarkPreDeducted(ctx context.Context, id int64) error
	SetOrderNo(ctx context.Context, id int64, orderNo string) error
	MarkPendingOrder(ctx context.Context, id int64) error
	RecordPublishFailure(ctx context.Context, id int64, maxAttempts int, detail string) error
	MarkOrdered(ctx context.Context, tx *gorm.DB, id int64) error
	MarkPendingRollback(ctx context.Context, tx *gorm.DB, id int64, detail string) error
	EnsurePendingRollback(ctx context.Context, tx *gorm.DB, seed *model.PreDeduction) (*model.PreDeduction, error)
	MarkRolledBack(ctx context.Context, id int64) error
	MarkReservationReleased(ctx context.Context, id int64) error
	RecordRollbackFailure(ctx context.Context, id int64, detail string) error
	HasUnresolved(ctx context.Context, activityID int64) (bool, error)
}
