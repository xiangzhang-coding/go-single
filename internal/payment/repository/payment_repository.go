// Package repository 定义 payment 模块的仓储 seam（ADR-0003：GORM 之上再包一层接口）。
// 支付回调为单事务：成功流水与订单状态迁移原子提交，失败流水与订单状态重判
// 原子提交；跨模块订单能力经 transaction.Handle 汇入同一事务。
package repository

import (
	"context"
	"errors"

	"github.com/xiangzhang-coding/go-single/internal/payment/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// ErrPaymentDuplicate 同一 payment_id 已存在（重复回调，唯一键冲突）。
var ErrPaymentDuplicate = errors.New("payment already processed")

// TxRunner 事务运行器：开启支付事务；fn 内任一错误整体回滚。
type TxRunner = transaction.Runner

// PaymentRepository 支付流水数据访问接口。
type PaymentRepository interface {
	// Create 事务内创建支付流水；payment_id 唯一键冲突返回 ErrPaymentDuplicate。
	Create(ctx context.Context, tx *transaction.Handle, p *model.Payment) error
	// GetByPaymentID 按支付流水号读取（幂等键）；不存在返回 (nil, nil)。
	GetByPaymentID(ctx context.Context, paymentID string) (*model.Payment, error)
}

// Store 聚合仓储，作为 service 的构造入参。
type Store struct {
	Payments PaymentRepository
	Tx       TxRunner
}
