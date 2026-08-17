// 秒杀异步落单消费者单元测试（中间 seam）：fake 活动仓储 + fake order/user 服务，
// 覆盖消息解析、活动/默认地址缺失（永久失败 → 死信）、落单错误分类
// （业务永久 / 基础设施瞬时 → 重投）与成功路径。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	ordersvc "github.com/xiangzhang-coding/go-single/internal/order/service"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
	usermodel "github.com/xiangzhang-coding/go-single/internal/user/model"
)

// ---- fake 消费者依赖 ----

type fakeOrderService struct {
	created  []ordersvc.SeckillCreateParams
	inserted bool
	err      error
}

func (f *fakeOrderService) CreateSeckillInTx(_ context.Context, _ *gorm.DB, p ordersvc.SeckillCreateParams) (bool, error) {
	f.created = append(f.created, p)
	return f.inserted, f.err
}

type fakeUserService struct {
	addresses map[int64]*usermodel.Address // userID → 默认地址
	err       error
}

func newFakeUserService() *fakeUserService {
	return &fakeUserService{addresses: map[int64]*usermodel.Address{}}
}

func (f *fakeUserService) GetDefaultAddress(_ context.Context, userID int64) (*usermodel.Address, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.addresses[userID], nil
}

// seedAddress 为用户种默认地址（与 order 单测同构的快照数据）。
func (f *fakeUserService) seedAddress(userID int64) {
	f.addresses[userID] = &usermodel.Address{
		ID: 1, UserID: userID, Receiver: "张三", Phone: "13800138000",
		Province: "广东省", City: "深圳市", District: "南山区", Detail: "科技园 1 号",
	}
}

// consumerFixture 消费者测试夹具。
type consumerFixture struct {
	consumer *SeckillOrderConsumer
	orders   *fakeOrderService
	users    *fakeUserService
	acts     *fakeActivities
}

func newConsumerFixture() *consumerFixture {
	acts := newFakeActivities()
	orders := &fakeOrderService{inserted: true}
	users := newFakeUserService()
	return &consumerFixture{
		consumer: NewSeckillOrderConsumer(acts, fakeSeckillTx{}, orders, users, metrics.New().Business(), zap.NewNop()),
		orders:   orders,
		users:    users,
		acts:     acts,
	}
}

// seed 种活动（进行中、秒杀价 9900）与用户默认地址，返回活动。
func (fx *consumerFixture) seed(t *testing.T, userID int64) *model.Activity {
	t.Helper()
	fx.users.seedAddress(userID)
	a := &model.Activity{ID: 1, SKUID: 5, Title: "限时秒杀", Price: 9900, Stock: 10, Status: "on_sale"}
	require.NoError(t, fx.acts.Create(context.Background(), a))
	return a
}

func marshalMsg(t *testing.T, m SeckillSuccessMessage) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// ---- 测试 ----

// 成功路径：消费者落单（活动 SKU/秒杀价 + 默认地址快照固化），返回 nil（Ack）。
func TestConsumerHappyPath(t *testing.T) {
	fx := newConsumerFixture()
	a := fx.seed(t, 42)

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: a.ID}))
	require.NoError(t, err)

	require.Len(t, fx.orders.created, 1)
	p := fx.orders.created[0]
	require.Equal(t, "10001", p.OrderNo)
	require.Equal(t, int64(42), p.UserID)
	require.Equal(t, a.ID, p.ActivityID)
	require.Equal(t, a.SKUID, p.SKUID)
	require.Equal(t, a.Price, p.Price, "订单应固化秒杀价")
	require.Equal(t, 1, p.Quantity)
	require.Equal(t, "张三", p.Address.Receiver, "地址快照应取自默认地址")
	require.Equal(t, 9, a.Stock, "落单应在同一事务中扣减活动库存")
}

// 非法消息体 / 字段缺失：永久失败（死信），不触碰落单。
func TestConsumerInvalidMessagePermanent(t *testing.T) {
	fx := newConsumerFixture()

	err := fx.consumer.Handle(context.Background(), []byte("not-json"))
	require.ErrorIs(t, err, mq.ErrPermanent)
	require.Empty(t, fx.orders.created)

	err = fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "", UserID: 42, ActivityID: 1}))
	require.ErrorIs(t, err, mq.ErrPermanent)
	require.Empty(t, fx.orders.created)
}

// 活动不存在：永久失败（死信；对账兜底），不落单。
func TestConsumerActivityMissingPermanent(t *testing.T) {
	fx := newConsumerFixture()
	fx.users.seedAddress(42)

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: 999}))
	require.ErrorIs(t, err, mq.ErrPermanent)
	require.Empty(t, fx.orders.created)
}

// 无默认地址：永久失败（死信；对账兜底），不落单。
func TestConsumerNoDefaultAddressPermanent(t *testing.T) {
	fx := newConsumerFixture()
	a := fx.seed(t, 42)
	fx.users.addresses = map[int64]*usermodel.Address{} // 清掉地址

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: a.ID}))
	require.ErrorIs(t, err, mq.ErrPermanent)
	require.Empty(t, fx.orders.created)
}

// 活动读取 DB 故障：瞬时失败（重投），不落单。
func TestConsumerActivityReadFailureTransient(t *testing.T) {
	fx := newConsumerFixture()
	fx.users.seedAddress(42)

	fx.consumer = NewSeckillOrderConsumer(&failingActivities{}, fakeSeckillTx{}, fx.orders, fx.users, metrics.New().Business(), zap.NewNop())
	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: 1}))
	require.Error(t, err)
	require.NotErrorIs(t, err, mq.ErrPermanent, "DB 故障应重投而非死信")
	require.Empty(t, fx.orders.created)
}

// 默认地址读取 DB 故障：瞬时失败（重投）。
func TestConsumerAddressReadFailureTransient(t *testing.T) {
	fx := newConsumerFixture()
	a := fx.seed(t, 42)
	fx.users.err = errors.New("mysql down")

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: a.ID}))
	require.Error(t, err)
	require.NotErrorIs(t, err, mq.ErrPermanent)
	require.Empty(t, fx.orders.created)
}

// 落单业务失败（活动库存不足等）：永久失败（死信），重投无意义。
func TestConsumerCreatePermanentFailure(t *testing.T) {
	fx := newConsumerFixture()
	a := fx.seed(t, 42)
	a.Stock = 0

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: a.ID}))
	require.ErrorIs(t, err, mq.ErrPermanent, "库存不足应死信（对账兜底）")
}

func TestConsumerDuplicateOrderDoesNotDeductActivityStock(t *testing.T) {
	fx := newConsumerFixture()
	a := fx.seed(t, 42)
	fx.orders.inserted = false

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: a.ID}))

	require.NoError(t, err)
	require.Equal(t, 10, a.Stock, "幂等命中不得重复扣减活动库存")
}

// 落单基础设施故障（DB 超时等）：瞬时失败（重投），at-least-once 不丢消息。
func TestConsumerCreateTransientFailure(t *testing.T) {
	fx := newConsumerFixture()
	a := fx.seed(t, 42)
	fx.orders.err = errors.New("mysql timeout")

	err := fx.consumer.Handle(context.Background(),
		marshalMsg(t, SeckillSuccessMessage{OrderNo: "10001", UserID: 42, ActivityID: a.ID}))
	require.Error(t, err)
	require.NotErrorIs(t, err, mq.ErrPermanent, "基础设施故障应重投而非死信")
}

// failingActivities 活动读取恒失败仓储（模拟 DB 故障）。
type failingActivities struct{}

func (failingActivities) Create(context.Context, *model.Activity) error { return nil }
func (failingActivities) Update(context.Context, *model.Activity) error { return nil }
func (failingActivities) GetByID(context.Context, int64) (*model.Activity, error) {
	return nil, errors.New("mysql down")
}
func (failingActivities) List(context.Context) ([]model.Activity, error) { return nil, nil }
func (failingActivities) UpdateStatus(context.Context, int64, string) error {
	return nil
}
func (failingActivities) DeductStock(context.Context, *gorm.DB, int64, int) (bool, error) {
	return true, nil
}
func (failingActivities) RestoreStock(context.Context, *gorm.DB, int64, int) error { return nil }

var _ repository.ActivityRepository = (*failingActivities)(nil)
