package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

var (
	ErrPreDeductionStateChanged = errors.New("pre-deduction state changed")
	ErrPreDeductionDuplicate    = errors.New("pre-deduction request already exists")
)

// PreDeductionRepository persists the recoverable flash-sale lifecycle.
type PreDeductionRepository interface {
	Create(ctx context.Context, p *model.PreDeduction) error
	GetByID(ctx context.Context, id int64) (*model.PreDeduction, error)
	GetByRequestID(ctx context.Context, userID, activityID int64, requestID string) (*model.PreDeduction, error)
	GetByIDForUpdate(ctx context.Context, tx *transaction.Handle, id int64) (*model.PreDeduction, error)
	EnsureLegacyPendingOrder(ctx context.Context, seed *model.PreDeduction) (*model.PreDeduction, error)
	ReservationTargets(ctx context.Context, activityID, userID int64) (pendingQuantity, userQuantity int, err error)
	PendingReservationQuantityForUpdate(ctx context.Context, tx *transaction.Handle, activityID int64) (int, error)
	HasAcceptedReservationForUpdate(ctx context.Context, tx *transaction.Handle, activityID int64) (bool, error)
	ListRecoverable(ctx context.Context, limit int) ([]model.PreDeduction, error)
	ListRecoverableByActivity(ctx context.Context, activityID int64) ([]model.PreDeduction, error)
	ListOrdered(ctx context.Context, limit int) ([]model.PreDeduction, error)
	MarkPreDeducted(ctx context.Context, id int64) error
	SetOrderNo(ctx context.Context, id int64, orderNo string) error
	MarkPendingOrder(ctx context.Context, id int64) error
	RecordPublishFailure(ctx context.Context, id int64, maxAttempts int, detail string) error
	MarkOrdered(ctx context.Context, tx *transaction.Handle, id int64) error
	MarkPendingRollback(ctx context.Context, id int64, detail string) error
	EnsurePendingRollback(ctx context.Context, tx *transaction.Handle, seed *model.PreDeduction) (*model.PreDeduction, error)
	MarkRolledBack(ctx context.Context, id int64) error
	MarkReservationReleased(ctx context.Context, id int64) error
	RecordRollbackFailure(ctx context.Context, id int64, detail string) error
	HasUnresolved(ctx context.Context, activityID int64) (bool, error)
}
