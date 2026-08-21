// service 层单元测试（中间 seam）：fake 支付仓储/跨模块订单服务，
// 覆盖模拟支付成功/失败、重复回调（流水唯一 + 状态机）、金额核对、
// 并发状态变更回滚与 owner 校验（防 IDOR）。
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	ordermodel "github.com/xiangzhang-coding/go-single/internal/order/model"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	paymentmodel "github.com/xiangzhang-coding/go-single/internal/payment/model"
	"github.com/xiangzhang-coding/go-single/internal/payment/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

// ---- fake 支付仓储 ----

type fakePayments struct {
	byID    map[string]*paymentmodel.Payment
	byOrder map[string][]paymentmodel.Payment
	create  func(p *paymentmodel.Payment) error
}

func newFakePayments() *fakePayments {
	return &fakePayments{
		byID:    map[string]*paymentmodel.Payment{},
		byOrder: map[string][]paymentmodel.Payment{},
	}
}

func (f *fakePayments) Create(_ context.Context, _ *transaction.Handle, p *paymentmodel.Payment) error {
	if f.create != nil {
		if err := f.create(p); err != nil {
			return err
		}
	}
	if _, dup := f.byID[p.PaymentID]; dup {
		return repository.ErrPaymentDuplicate
	}
	f.byID[p.PaymentID] = p
	f.byOrder[p.OrderNo] = append(f.byOrder[p.OrderNo], *p)
	return nil
}

func (f *fakePayments) GetByPaymentID(_ context.Context, paymentID string) (*paymentmodel.Payment, error) {
	return f.byID[paymentID], nil
}

// ---- fake 订单服务（payment 模块最小接口）----

type fakeOrderSvc struct {
	views                  map[string]*ordermodel.OrderView
	getErr                 error
	markPaid               func(orderNo string, amount int64) (bool, error)
	canRecordFailedPayment func(orderNo string) (bool, error)
}

func newFakeOrderSvc() *fakeOrderSvc { return &fakeOrderSvc{views: map[string]*ordermodel.OrderView{}} }

func (f *fakeOrderSvc) GetDetail(_ context.Context, userID int64, orderNo string) (*ordermodel.OrderView, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	view, ok := f.views[orderNo]
	if !ok {
		return nil, ordersvc.ErrOrderNotFound
	}
	if view.UserID != userID {
		return nil, ordersvc.ErrOrderForbidden
	}
	return view, nil
}

func (f *fakeOrderSvc) MarkPaid(_ context.Context, _ *transaction.Handle, orderNo string, payAmount int64) (bool, error) {
	if f.markPaid != nil {
		return f.markPaid(orderNo, payAmount)
	}
	view, ok := f.views[orderNo]
	if !ok || view.Status != ordermodel.OrderStatusPendingPayment || view.PayAmount != payAmount ||
		!view.ExpireAt.After(time.Now()) {
		return false, nil
	}
	view.Status = ordermodel.OrderStatusPaid
	return true, nil
}

func (f *fakeOrderSvc) CanRecordFailedPayment(_ context.Context, _ *transaction.Handle, orderNo string) (bool, error) {
	if f.canRecordFailedPayment != nil {
		return f.canRecordFailedPayment(orderNo)
	}
	view, ok := f.views[orderNo]
	return ok && view.Status == ordermodel.OrderStatusPendingPayment && view.ExpireAt.After(time.Now()), nil
}

// ---- fixture ----

type fixture struct {
	svc   Service
	pays  *fakePayments
	order *fakeOrderSvc
}

func newFixture() *fixture {
	f := &fixture{pays: newFakePayments(), order: newFakeOrderSvc()}
	f.svc = New(repository.Store{Payments: f.pays, Tx: fixtureTx{f}}, f.order, metrics.New().Business())
	return f
}

// fixtureTx 单测事务运行器：执行回调；回调报错时回滚 fake 支付仓储的写入
// （镜像 GORM Transaction 语义），验证流水与订单状态保持一致。
type fixtureTx struct{ f *fixture }

func (t fixtureTx) WithinTx(_ context.Context, fn func(tx *transaction.Handle) error) error {
	byID := make(map[string]*paymentmodel.Payment, len(t.f.pays.byID))
	for k, v := range t.f.pays.byID {
		cp := *v
		byID[k] = &cp
	}
	byOrder := make(map[string][]paymentmodel.Payment, len(t.f.pays.byOrder))
	for k, v := range t.f.pays.byOrder {
		byOrder[k] = append([]paymentmodel.Payment(nil), v...)
	}
	if err := fn(nil); err != nil {
		t.f.pays.byID = byID
		t.f.pays.byOrder = byOrder
		return err
	}
	return nil
}

// seedOrder 预置一张待支付订单，返回其订单视图。
func (f *fixture) seedOrder(userID int64, orderNo string, payAmount int64) *ordermodel.OrderView {
	view := &ordermodel.OrderView{
		Order: ordermodel.Order{
			OrderNo:   orderNo,
			UserID:    userID,
			Status:    ordermodel.OrderStatusPendingPayment,
			PayAmount: payAmount,
			ExpireAt:  time.Now().Add(time.Hour),
		},
	}
	f.order.views[orderNo] = view
	return view
}

func params(paymentID string) PayParams {
	return PayParams{OrderNo: "1001", PaymentID: paymentID, Amount: 9900, Result: paymentmodel.PaymentResultSuccess}
}

// ---- 成功 / 失败 ----

func TestMockPaySuccessTransitionsOrderToPaid(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)

	p, err := fx.svc.MockPay(context.Background(), 42, params("p1"))
	require.NoError(t, err)
	require.Equal(t, paymentmodel.PaymentResultSuccess, p.Result)
	require.Equal(t, int64(9900), p.Amount)
	require.Equal(t, ordermodel.OrderStatusPaid, fx.order.views["1001"].Status, "支付成功订单应进入已支付")
	require.Equal(t, 1, len(fx.pays.byID), "成功回调应落一条流水")
}

func TestMockPayFailKeepsOrderPendingAndRecordsLedger(t *testing.T) {
	fx := newFixture()
	view := fx.seedOrder(42, "1001", 9900)

	p := params("p2")
	p.Result = paymentmodel.PaymentResultFail
	got, err := fx.svc.MockPay(context.Background(), 42, p)
	require.NoError(t, err)
	require.Equal(t, paymentmodel.PaymentResultFail, got.Result)
	require.Equal(t, ordermodel.OrderStatusPendingPayment, view.Status, "支付失败订单应停留待支付")
	require.Equal(t, 1, len(fx.pays.byID), "失败回调应记录失败流水（审计留档）")

	// 失败后以新 payment_id 重试成功：可重付。
	p2 := params("p3")
	p2.Result = paymentmodel.PaymentResultFail
	_, err = fx.svc.MockPay(context.Background(), 42, p2)
	require.NoError(t, err)
	require.Equal(t, ordermodel.OrderStatusPendingPayment, view.Status)

	got2, err := fx.svc.MockPay(context.Background(), 42, params("p4"))
	require.NoError(t, err)
	require.Equal(t, paymentmodel.PaymentResultSuccess, got2.Result)
	require.Equal(t, ordermodel.OrderStatusPaid, view.Status)
	require.Equal(t, 3, len(fx.pays.byID))
}

// ---- 重复回调 ----

func TestMockPayRejectsDuplicatePaymentID(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)

	_, err := fx.svc.MockPay(context.Background(), 42, params("p1"))
	require.NoError(t, err)

	// 同一 payment_id 重复回调：流水唯一约束拒绝。
	_, err = fx.svc.MockPay(context.Background(), 42, params("p1"))
	require.ErrorIs(t, err, ErrPaymentDuplicate)
	require.Equal(t, ordermodel.OrderStatusPaid, fx.order.views["1001"].Status)

	// 新 payment_id 但订单已支付：状态机校验拒绝（重复回调第二道防线）。
	_, err = fx.svc.MockPay(context.Background(), 42, params("p5"))
	require.ErrorIs(t, err, ErrIllegalTransition)
	require.Equal(t, 1, len(fx.pays.byID), "被拒回调不得落流水")
}

func TestMockPayRejectsDuplicateAtUniqueKeyRace(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)

	// 并发预检均通过后，落库唯一键冲突（仓储层 1062 → ErrPaymentDuplicate）。
	fx.pays.create = func(p *paymentmodel.Payment) error {
		if _, dup := fx.pays.byID[p.PaymentID]; dup {
			return repository.ErrPaymentDuplicate
		}
		return nil
	}
	_, err := fx.svc.MockPay(context.Background(), 42, params("race1"))
	require.NoError(t, err)
	_, err = fx.svc.MockPay(context.Background(), 42, params("race1"))
	require.ErrorIs(t, err, ErrPaymentDuplicate)
	require.Equal(t, ordermodel.OrderStatusPaid, fx.order.views["1001"].Status)
}

// ---- 金额核对 ----

func TestMockPayRejectsAmountMismatch(t *testing.T) {
	fx := newFixture()
	view := fx.seedOrder(42, "1001", 9900)

	p := params("p6")
	p.Amount = 9901
	_, err := fx.svc.MockPay(context.Background(), 42, p)
	require.ErrorIs(t, err, ErrAmountMismatch)
	require.Equal(t, ordermodel.OrderStatusPendingPayment, view.Status, "金额不符订单不得流转")
	require.Empty(t, fx.pays.byID)
}

// ---- 状态机 ----

func TestMockPayRejectsNonPendingOrder(t *testing.T) {
	fx := newFixture()
	view := fx.seedOrder(42, "1001", 9900)
	view.Status = ordermodel.OrderStatusCancelled

	_, err := fx.svc.MockPay(context.Background(), 42, params("p7"))
	require.ErrorIs(t, err, ErrIllegalTransition)
	require.Empty(t, fx.pays.byID)
}

// 失败回调同样受状态机约束：已支付/已取消订单不得再记失败流水（污染已流转订单）。
func TestMockPayFailRejectsNonPendingOrder(t *testing.T) {
	fx := newFixture()
	view := fx.seedOrder(42, "1001", 9900)
	view.Status = ordermodel.OrderStatusPaid

	p := params("p7f")
	p.Result = paymentmodel.PaymentResultFail
	_, err := fx.svc.MockPay(context.Background(), 42, p)
	require.ErrorIs(t, err, ErrIllegalTransition)
	require.Empty(t, fx.pays.byID)
}

// ---- 并发状态变更：条件更新失败整体回滚 ----

func TestMockPayRollsBackOnOrderChanged(t *testing.T) {
	fx := newFixture()
	view := fx.seedOrder(42, "1001", 9900)

	// 预检通过后订单状态被并发变更（如取消），条件更新 RowsAffected=0。
	fx.order.markPaid = func(string, int64) (bool, error) { return false, nil }
	_, err := fx.svc.MockPay(context.Background(), 42, params("p8"))
	require.ErrorIs(t, err, ErrOrderChanged)
	require.Equal(t, ordermodel.OrderStatusPendingPayment, view.Status)
	require.Empty(t, fx.pays.byID, "条件更新失败应回滚流水，不产生孤儿记录")
}

func TestMockPayFailRollsBackOnConcurrentOrderChange(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)
	fx.order.canRecordFailedPayment = func(string) (bool, error) { return false, nil }
	p := params("failed-after-cancel")
	p.Result = paymentmodel.PaymentResultFail

	_, err := fx.svc.MockPay(context.Background(), 42, p)
	require.ErrorIs(t, err, ErrOrderChanged)
	require.Empty(t, fx.pays.byID, "事务内重判失败必须回滚失败流水")
}

func TestMockPayExpiredOrderRollsBackLedger(t *testing.T) {
	fx := newFixture()
	view := fx.seedOrder(42, "1001", 9900)
	view.ExpireAt = time.Now().Add(-time.Second)

	_, err := fx.svc.MockPay(context.Background(), 42, params("expired-payment"))
	require.ErrorIs(t, err, ErrOrderChanged)
	require.Equal(t, ordermodel.OrderStatusPendingPayment, view.Status)
	require.Empty(t, fx.pays.byID, "过期订单拒付必须回滚支付流水")
}

// ---- owner 校验与订单存在性 ----

func TestMockPayRejectsForeignOrderAndMissingOrder(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)

	// 他人订单：防 IDOR——owner 校验先于流水检查，重复他人流水不得泄露订单信息。
	fx.pays.byID["p9"] = &paymentmodel.Payment{PaymentID: "p9", OrderNo: "1001", UserID: 42}
	_, err := fx.svc.MockPay(context.Background(), 7, params("p9"))
	require.ErrorIs(t, err, ErrOrderForbidden)

	// 不存在的订单。
	_, err = fx.svc.MockPay(context.Background(), 42, PayParams{OrderNo: "999", PaymentID: "p10", Amount: 1, Result: paymentmodel.PaymentResultSuccess})
	require.ErrorIs(t, err, ErrOrderNotFound)
	require.Nil(t, fx.pays.byID["p10"], "被拒回调不得落流水")
}

func TestMockPayRejectsOrderSvcInfraError(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)
	fx.order.getErr = errors.New("db down")

	_, err := fx.svc.MockPay(context.Background(), 42, params("p11"))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrOrderNotFound)
}

// ---- 参数校验 ----

func TestMockPayValidatesParams(t *testing.T) {
	fx := newFixture()
	fx.seedOrder(42, "1001", 9900)

	cases := []struct {
		name string
		p    PayParams
	}{
		{"空 payment_id", PayParams{OrderNo: "1001", PaymentID: "", Amount: 1, Result: paymentmodel.PaymentResultSuccess}},
		{"超长 payment_id", PayParams{OrderNo: "1001", PaymentID: string(make([]byte, 65)), Amount: 1, Result: paymentmodel.PaymentResultSuccess}},
		{"非法 result", PayParams{OrderNo: "1001", PaymentID: "p12", Amount: 1, Result: "pending"}},
		{"负金额", PayParams{OrderNo: "1001", PaymentID: "p13", Amount: -1, Result: paymentmodel.PaymentResultSuccess}},
		{"空订单号", PayParams{OrderNo: "", PaymentID: "p14", Amount: 1, Result: paymentmodel.PaymentResultSuccess}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.svc.MockPay(context.Background(), 42, tc.p)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
	require.Empty(t, fx.pays.byID)
}
