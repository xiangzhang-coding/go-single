// Package repository 定义 payment 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 支付成功路径为单事务（支付流水 + 订单状态迁移），事务由 TxRunner 开启，
// 订单状态迁移经 order 模块 service 的 tx 参数汇入同一事务（跨模块写约定）。
package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/payment/model"
)

// ErrPaymentDuplicate 同一 payment_id 已存在（重复回调，唯一键冲突）。
var ErrPaymentDuplicate = errors.New("payment already processed")

// TxRunner 事务运行器：开启支付事务（流水落库 + 订单 待支付→已支付），
// fn 内任一错误整体回滚（流水与订单状态保持一致）。
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}

// PaymentRepository 支付流水数据访问接口。
type PaymentRepository interface {
	// Create 事务内创建支付流水；payment_id 唯一键冲突返回 ErrPaymentDuplicate。
	Create(ctx context.Context, tx *gorm.DB, p *model.Payment) error
	// GetByPaymentID 按支付流水号读取（幂等键）；不存在返回 (nil, nil)。
	GetByPaymentID(ctx context.Context, paymentID string) (*model.Payment, error)
}

// Store 聚合仓储，作为 service 的构造入参。
type Store struct {
	Payments PaymentRepository
	Tx       TxRunner
}
