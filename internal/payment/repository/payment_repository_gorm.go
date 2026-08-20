package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/payment/model"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// isDuplicate MySQL 1062：唯一键冲突（payment_id 重复回调）。
func isDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// GORMPaymentStore 支付流水仓储实现（GORM）：开启事务 + 流水读写。
type GORMPaymentStore struct {
	db *gorm.DB
}

// NewGORMPayment 构造支付流水仓储。
func NewGORMPayment(db *gorm.DB) *GORMPaymentStore {
	return &GORMPaymentStore{db: db}
}

// WithinTx 开启支付事务（流水落库 + 跨模块订单状态迁移）；fn 返回错误则整体回滚。
func (s *GORMPaymentStore) WithinTx(ctx context.Context, fn func(tx *transaction.Handle) error) error {
	return transaction.WithinGORM(ctx, s.db, fn)
}

// Create 事务内创建支付流水；payment_id 唯一键冲突（重复回调）映射为 ErrPaymentDuplicate。
func (s *GORMPaymentStore) Create(ctx context.Context, handle *transaction.Handle, p *model.Payment) error {
	tx, err := transaction.GORM(handle)
	if err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Create(p).Error; err != nil {
		if isDuplicate(err) {
			return ErrPaymentDuplicate
		}
		return err
	}
	return nil
}

func (s *GORMPaymentStore) GetByPaymentID(ctx context.Context, paymentID string) (*model.Payment, error) {
	var p model.Payment
	if err := s.db.WithContext(ctx).First(&p, "payment_id = ?", paymentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// 编译期断言：GORM 实现满足仓储接口。
var _ PaymentRepository = (*GORMPaymentStore)(nil)
var _ TxRunner = (*GORMPaymentStore)(nil)
