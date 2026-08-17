package model

import (
	"strconv"
	"time"
)

type PreDeductionStatus string

const (
	PreDeductionStatusPreparing       PreDeductionStatus = "preparing"
	PreDeductionStatusPendingPublish  PreDeductionStatus = "pending_publish"
	PreDeductionStatusPendingOrder    PreDeductionStatus = "pending_order"
	PreDeductionStatusOrdered         PreDeductionStatus = "ordered"
	PreDeductionStatusPendingRollback PreDeductionStatus = "pending_rollback"
	PreDeductionStatusRolledBack      PreDeductionStatus = "rolled_back"
)

// PreDeduction is the durable fact that connects one Redis reservation to its
// MQ message, eventual order, and compensation.
type PreDeduction struct {
	ID                    int64              `json:"id,string"`
	UserID                int64              `json:"user_id"`
	ActivityID            int64              `json:"activity_id"`
	OrderNo               *string            `json:"order_no,omitempty"`
	Quantity              int                `json:"quantity"`
	Status                PreDeductionStatus `json:"status"`
	PublishAttempts       int                `json:"publish_attempts"`
	RollbackAttempts      int                `json:"rollback_attempts"`
	LastError             string             `json:"last_error,omitempty"`
	Legacy                bool               `json:"-"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
	OrderedAt             *time.Time         `json:"ordered_at,omitempty"`
	RolledBackAt          *time.Time         `json:"rolled_back_at,omitempty"`
	ReservationReleasedAt *time.Time         `json:"-"`
}

func (PreDeduction) TableName() string { return "flashsale_pre_deductions" }

func (p *PreDeduction) ReservationToken() string {
	if p.Legacy {
		return "1"
	}
	return strconv.FormatInt(p.ID, 10)
}

func (p *PreDeduction) OrderNumber() string {
	if p.OrderNo == nil {
		return ""
	}
	return *p.OrderNo
}
