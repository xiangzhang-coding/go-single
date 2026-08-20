// Package service 承载 payment 模块业务：模拟支付回调（成功/失败）。
//
// 成功路径：状态机校验（仅 待支付→已支付）+ 支付流水唯一约束（payment_id）+
// 金额核对（回调金额 = 订单应付金额），流水落库与订单状态迁移在支付模块开启的
// 单事务内完成（跨模块写经 order service 的 tx 参数汇入同一事务）。
// 失败路径：订单停留待支付，仅记录失败流水（审计留档），客户端以新 payment_id 重试。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	paymentmodel "github.com/xiangzhang-coding/go-single/internal/payment/model"
	"github.com/xiangzhang-coding/go-single/internal/payment/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/retry"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// 业务错误：handler 据此映射 HTTP 状态码。
var (
	ErrInvalidInput      = errors.New("invalid input")
	ErrOrderNotFound     = errors.New("order not found")
	ErrOrderForbidden    = errors.New("order does not belong to user")
	ErrPaymentDuplicate  = errors.New("payment already processed")
	ErrAmountMismatch    = errors.New("payment amount mismatch")
	ErrIllegalTransition = errors.New("illegal order status transition")
	ErrOrderChanged      = errors.New("order status changed, retry")
)

// ---- 跨模块最小接口（进程内调用，面向接口非 HTTP；实现见 main 装配） ----

// OrderService order 模块最小接口：读取订单（owner 校验）+ 支付成功状态迁移。
type OrderService interface {
	GetDetail(ctx context.Context, userID int64, orderNo string) (*ordermodel.OrderView, error)
	MarkPaid(ctx context.Context, tx *transaction.Handle, orderNo string, payAmount int64) (bool, error)
}

// PayParams 模拟支付回调参数。
type PayParams struct {
	OrderNo   string // 雪花订单号
	PaymentID string // 支付流水号（客户端生成，唯一约束防重复回调）
	Amount    int64  // 回调申报金额（分），成功回调与订单应付核对
	Result    string // success: 支付成功 / fail: 支付失败
}

// Service payment 模块的业务接口。
type Service interface {
	// MockPay 处理模拟支付回调：成功驱动订单 待支付→已支付；
	// 失败仅记录流水，订单停留待支付可重付。
	MockPay(ctx context.Context, userID int64, p PayParams) (*paymentmodel.Payment, error)
}

type paymentService struct {
	store    repository.Store
	orders   OrderService
	metrics  *metrics.Business
	retryCfg retry.Config // 幂等操作有限重试（T20）：仅模拟支付回调 MockPay
}

// New 构造支付服务；retryCfg 为回调处理重试配置（T20 有限重试；省略 = 不重试）。
func New(store repository.Store, orders OrderService, m *metrics.Business, retryCfg ...retry.Config) Service {
	cfg := retry.OrDefault(retryCfg...)
	return &paymentService{store: store, orders: orders, metrics: m, retryCfg: cfg}
}

// MockPay 支付回调流程（幂等接口有限重试，T20）：
//  1. 参数校验（order_no / payment_id / amount / result）
//  2. 读取订单（owner 校验，防 IDOR——他人订单先于流水检查拒绝，不泄露流水信息）
//  3. 幂等检查：payment_id 已存在 → 重复回调拒绝（唯一约束 + 落库 1062 兜底）
//  4. 状态机校验：仅待支付订单可发起支付（成功/失败回调一致）；成功回调另做金额核对
//  5. 单事务：创建支付流水 → 成功则条件更新订单 待支付→已支付（WHERE 同时
//     校验 status 与 pay_amount，失败 = 并发状态已变，回滚整体拒绝）
//
// 回调幂等（payment_id 唯一约束）且事务原子：基础设施瞬时故障（DB 抖动）有限
// 重试 + 退避吸收；业务拒绝（重复回调/金额不符/非法跃迁等）retry.Stop 不重试。
func (s *paymentService) MockPay(ctx context.Context, userID int64, p PayParams) (*paymentmodel.Payment, error) {
	var payment *paymentmodel.Payment
	err := retry.Do(ctx, s.retryCfg, func(ctx context.Context) error {
		var attemptErr error
		payment, attemptErr = s.mockPay(ctx, userID, p)
		if attemptErr != nil && isBusinessError(attemptErr) {
			return retry.Stop(attemptErr)
		}
		return attemptErr
	})
	return payment, err
}

// mockPay 单次支付回调处理（真实执行体，见 MockPay 流程说明）。
func (s *paymentService) mockPay(ctx context.Context, userID int64, p PayParams) (*paymentmodel.Payment, error) {
	if err := validateParams(userID, p); err != nil {
		return nil, err
	}

	view, err := s.orders.GetDetail(ctx, userID, p.OrderNo)
	if err != nil {
		return nil, translateOrderError(err)
	}
	order := view.Order

	// 重复回调：同一 payment_id 只处理一次（幂等键）。
	existing, err := s.store.Payments.GetByPaymentID(ctx, p.PaymentID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrPaymentDuplicate
	}

	// 状态机校验：仅 待支付 可发起支付（成功/失败一致，失败回调不得污染已流转订单）。
	if order.Status != ordermodel.OrderStatusPendingPayment {
		return nil, fmt.Errorf("%w: %s → %s", ErrIllegalTransition, order.Status, ordermodel.OrderStatusPaid)
	}

	// 成功回调：金额核对（回调金额 = 订单应付金额）。
	if p.Result == paymentmodel.PaymentResultSuccess && order.PayAmount != p.Amount {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrAmountMismatch, p.Amount, order.PayAmount)
	}

	payment := &paymentmodel.Payment{
		PaymentID: p.PaymentID,
		OrderNo:   p.OrderNo,
		UserID:    userID,
		Amount:    p.Amount,
		Result:    p.Result,
	}
	err = s.store.Tx.WithinTx(ctx, func(tx *transaction.Handle) error {
		if err := s.store.Payments.Create(ctx, tx, payment); err != nil {
			return translateRepoError(err)
		}
		if p.Result == paymentmodel.PaymentResultSuccess {
			ok, err := s.orders.MarkPaid(ctx, tx, p.OrderNo, p.Amount)
			if err != nil {
				return err
			}
			// 预检通过仍条件更新失败：并发下状态已变（已支付/已取消），整体回滚。
			if !ok {
				return ErrOrderChanged
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 支付回调结果打点（T19c）：流水落库且事务提交后计数。
	s.metrics.PaymentResult(p.Result == paymentmodel.PaymentResultSuccess)
	return payment, nil
}

// isBusinessError 支付回调的业务拒绝分支：重试无法改变结果（参数/归属/重复/
// 状态机/金额），不重试；其余（基础设施/未知）错误才可重试。
func isBusinessError(err error) bool {
	switch {
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrOrderNotFound),
		errors.Is(err, ErrOrderForbidden), errors.Is(err, ErrPaymentDuplicate),
		errors.Is(err, ErrAmountMismatch), errors.Is(err, ErrIllegalTransition),
		errors.Is(err, ErrOrderChanged):
		return true
	}
	return false
}

func validateParams(userID int64, p PayParams) error {
	if userID <= 0 {
		return fmt.Errorf("%w: invalid user id", ErrInvalidInput)
	}
	if p.OrderNo == "" || len(p.OrderNo) > 20 {
		return fmt.Errorf("%w: invalid order_no", ErrInvalidInput)
	}
	pid := p.PaymentID
	if strings.TrimSpace(pid) == "" || utf8.RuneCountInString(pid) > 64 {
		return fmt.Errorf("%w: invalid payment_id", ErrInvalidInput)
	}
	if p.Amount < 0 {
		return fmt.Errorf("%w: invalid amount", ErrInvalidInput)
	}
	if p.Result != paymentmodel.PaymentResultSuccess && p.Result != paymentmodel.PaymentResultFail {
		return fmt.Errorf("%w: invalid result", ErrInvalidInput)
	}
	return nil
}

// translateOrderError 跨模块错误翻译为本模块业务错误（handler 据此映射 HTTP 状态码）。
func translateOrderError(err error) error {
	switch {
	case errors.Is(err, ordersvc.ErrOrderNotFound):
		return ErrOrderNotFound
	case errors.Is(err, ordersvc.ErrOrderForbidden):
		return ErrOrderForbidden
	}
	return err
}

// translateRepoError 仓储层错误翻译：唯一键冲突（并发重复回调兜底）。
func translateRepoError(err error) error {
	if errors.Is(err, repository.ErrPaymentDuplicate) {
		return ErrPaymentDuplicate
	}
	return err
}
