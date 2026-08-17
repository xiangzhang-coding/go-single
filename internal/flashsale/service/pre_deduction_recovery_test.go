package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
)

type fakePreDeductions struct {
	mu     sync.Mutex
	nextID int64
	byID   map[int64]*model.PreDeduction
}

func newFakePreDeductions() *fakePreDeductions {
	return &fakePreDeductions{byID: map[int64]*model.PreDeduction{}}
}

func (f *fakePreDeductions) Create(_ context.Context, p *model.PreDeduction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	p.ID = f.nextID
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
		p.UpdatedAt = p.CreatedAt
	}
	copy := *p
	f.byID[p.ID] = &copy
	return nil
}

func (f *fakePreDeductions) GetByID(_ context.Context, id int64) (*model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.byID[id]
	if p == nil {
		return nil, nil
	}
	copy := *p
	return &copy, nil
}

func (f *fakePreDeductions) GetByIDForUpdate(ctx context.Context, _ *gorm.DB, id int64) (*model.PreDeduction, error) {
	return f.GetByID(ctx, id)
}

func (f *fakePreDeductions) ListRecoverable(context.Context, int) ([]model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.PreDeduction
	for _, p := range f.byID {
		switch p.Status {
		case model.PreDeductionStatusPreparing, model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder, model.PreDeductionStatusPendingRollback:
			out = append(out, *p)
		}
	}
	return out, nil
}

func (f *fakePreDeductions) ListOrdered(context.Context, int) ([]model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.PreDeduction
	for _, p := range f.byID {
		if p.Status == model.PreDeductionStatusOrdered {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (f *fakePreDeductions) MarkPreDeducted(_ context.Context, id int64) error {
	return f.update(id, func(p *model.PreDeduction) { p.Status = model.PreDeductionStatusPendingPublish })
}

func (f *fakePreDeductions) SetOrderNo(_ context.Context, id int64, orderNo string) error {
	return f.update(id, func(p *model.PreDeduction) {
		if p.OrderNo == nil {
			p.OrderNo = &orderNo
		}
	})
}

func (f *fakePreDeductions) MarkPendingOrder(_ context.Context, id int64) error {
	return f.update(id, func(p *model.PreDeduction) {
		if p.Status == model.PreDeductionStatusPendingPublish || p.Status == model.PreDeductionStatusPendingOrder {
			p.Status = model.PreDeductionStatusPendingOrder
			p.LastError = ""
		}
	})
}

func (f *fakePreDeductions) RecordPublishFailure(_ context.Context, id int64, maxAttempts int, detail string) error {
	return f.update(id, func(p *model.PreDeduction) {
		p.PublishAttempts++
		p.LastError = detail
		if p.PublishAttempts >= maxAttempts {
			p.Status = model.PreDeductionStatusPendingRollback
		}
	})
}

func (f *fakePreDeductions) MarkOrdered(_ context.Context, _ *gorm.DB, id int64) error {
	return f.update(id, func(p *model.PreDeduction) { p.Status = model.PreDeductionStatusOrdered })
}

func (f *fakePreDeductions) MarkPendingRollback(_ context.Context, _ *gorm.DB, id int64, detail string) error {
	return f.update(id, func(p *model.PreDeduction) {
		switch p.Status {
		case model.PreDeductionStatusPreparing, model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder, model.PreDeductionStatusPendingRollback:
			p.Status = model.PreDeductionStatusPendingRollback
			p.LastError = detail
		}
	})
}

func (f *fakePreDeductions) EnsurePendingRollback(_ context.Context, _ *gorm.DB, seed *model.PreDeduction) (*model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.OrderNumber() == seed.OrderNumber() {
			if p.Status != model.PreDeductionStatusRolledBack {
				p.Status = model.PreDeductionStatusPendingRollback
			}
			copy := *p
			return &copy, nil
		}
	}
	f.nextID++
	seed.ID = f.nextID
	seed.Status = model.PreDeductionStatusPendingRollback
	seed.Legacy = true
	copy := *seed
	f.byID[seed.ID] = &copy
	return seed, nil
}

func (f *fakePreDeductions) MarkRolledBack(_ context.Context, id int64) error {
	return f.update(id, func(p *model.PreDeduction) {
		p.Status = model.PreDeductionStatusRolledBack
		p.LastError = ""
	})
}

func (f *fakePreDeductions) MarkReservationReleased(_ context.Context, id int64) error {
	return f.update(id, func(p *model.PreDeduction) {
		now := time.Now()
		p.ReservationReleasedAt = &now
	})
}

func (f *fakePreDeductions) RecordRollbackFailure(_ context.Context, id int64, detail string) error {
	return f.update(id, func(p *model.PreDeduction) {
		p.RollbackAttempts++
		p.LastError = detail
	})
}

func (f *fakePreDeductions) HasUnresolved(_ context.Context, activityID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.ActivityID == activityID {
			switch p.Status {
			case model.PreDeductionStatusPreparing, model.PreDeductionStatusPendingPublish,
				model.PreDeductionStatusPendingOrder, model.PreDeductionStatusPendingRollback:
				return true, nil
			}
		}
	}
	return false, nil
}

func (f *fakePreDeductions) update(id int64, fn func(*model.PreDeduction)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.byID[id]; p != nil {
		fn(p)
	}
	return nil
}

type recoveryFixture struct {
	svc  Service
	base *fixture
	pd   *fakePreDeductions
}

func newRecoveryFixture(pub *fakePublisher, nos OrderNoGenerator) *recoveryFixture {
	base := newFixture()
	if pub == nil {
		pub = base.pub
	}
	if nos == nil {
		nos = &fakeNos{}
	}
	pd := newFakePreDeductions()
	base.pub = pub
	base.svc = New(repository.Store{Activities: base.acts, PreDeductions: pd}, base.products, base.cache,
		limiter.RedisCounterConfig{}, pub, nos, metrics.New().Business())
	return &recoveryFixture{svc: base.svc, base: base, pd: pd}
}

func (fx *recoveryFixture) publishedActivity(t *testing.T) *model.Activity {
	t.Helper()
	a := fx.base.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	return a
}

func TestSeckillPersistsLifecycleAndPublishesStableIdentity(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID)
	require.NoError(t, err)
	require.NotZero(t, result.PreDeductionID)
	require.NotEmpty(t, result.OrderNo)
	require.Equal(t, model.PreDeductionStatusPendingOrder, result.Status)

	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingOrder, pd.Status)
	require.Equal(t, result.OrderNo, pd.OrderNumber())

	var msg SeckillSuccessMessage
	require.NoError(t, json.Unmarshal(fx.base.pub.body, &msg))
	require.Equal(t, result.PreDeductionID, msg.PreDeductionID)
}

func TestRecoverAfterMessageAcceptedButConfirmLostAndRestart(t *testing.T) {
	pub := &fakePublisher{err: errors.New("confirm unknown")}
	fx := newRecoveryFixture(pub, nil)
	a := fx.publishedActivity(t)

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID)
	require.NoError(t, err, "durable pre-deduction remains a queued request")
	require.Equal(t, model.PreDeductionStatusPendingPublish, result.Status)
	firstOrderNo := result.OrderNo
	require.NotEmpty(t, firstOrderNo)
	firstBody := append([]byte(nil), pub.body...)
	// Simulate Redis restarting before AOF fsync: the successful Lua mutation
	// (stock/count/idempotency/marker) is lost as one atomic unit, while MySQL
	// already durably records pending_publish.
	delete(fx.base.cache.stock, stockKey(a.ID))
	delete(fx.base.cache.count, countKey(a.ID, 42))
	delete(fx.base.cache.idem, idemKey(a.ID, 42))
	delete(fx.base.cache.idemToken, idemKey(a.ID, 42))
	delete(fx.base.cache.reservations, reservationKey(result.PreDeductionID))

	pub.err = nil
	restarted := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd}, fx.base.products, fx.base.cache,
		limiter.RedisCounterConfig{}, pub, &fakeNos{}, metrics.New().Business())
	stats, err := restarted.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Published)
	require.Equal(t, 2, pub.attemptsCount(), "unknown confirmation is retried after restart")
	require.Equal(t, firstBody, pub.body, "retry must publish the exact accepted message")

	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingOrder, pd.Status)
	require.Equal(t, firstOrderNo, pd.OrderNumber(), "uncertain confirmation must reuse the stable order number")
	require.Equal(t, 99, fx.base.cache.stock[stockKey(a.ID)])
	require.Equal(t, 1, fx.base.cache.count[countKey(a.ID, 42)])
	require.Equal(t, pd.ReservationToken(), fx.base.cache.reservations[reservationKey(pd.ID)])
}

func TestRecoverPreDeductionAfterOrderNumberFailure(t *testing.T) {
	fx := newRecoveryFixture(nil, failingNos{})
	a := fx.publishedActivity(t)

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID)
	require.NoError(t, err)
	require.Empty(t, result.OrderNo)
	require.Equal(t, model.PreDeductionStatusPendingPublish, result.Status)

	restarted := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd}, fx.base.products, fx.base.cache,
		limiter.RedisCounterConfig{}, fx.base.pub, &fakeNos{}, metrics.New().Business())
	stats, err := restarted.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Published)
}

func TestRecoverPreDeductionRetriesFailedCancellationCompensation(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID)
	require.NoError(t, err)
	require.NoError(t, fx.pd.MarkPendingRollback(context.Background(), nil, result.PreDeductionID, "order cancelled"))

	fx.base.cache.err = errors.New("redis down")
	stats, err := fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Failed)
	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingRollback, pd.Status)
	require.Equal(t, 1, pd.RollbackAttempts)

	fx.base.cache.err = nil
	restarted := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd}, fx.base.products, fx.base.cache,
		limiter.RedisCounterConfig{}, fx.base.pub, &fakeNos{}, metrics.New().Business())
	stats, err = restarted.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.RolledBack)
	pd, err = fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, pd.Status)
	require.Equal(t, 100, fx.base.cache.stock[stockKey(a.ID)])
	require.Zero(t, fx.base.cache.count[countKey(a.ID, 42)])
	require.False(t, fx.base.cache.idem[idemKey(a.ID, 42)])
}

func TestPublishRecoveryExhaustionCompletesRollback(t *testing.T) {
	pub := &fakePublisher{err: errors.New("rabbitmq unavailable")}
	fx := newRecoveryFixture(pub, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID)
	require.NoError(t, err)

	for i := 1; i < maxPublishAttempts; i++ {
		_, err = fx.svc.RecoverPreDeductions(context.Background())
		require.NoError(t, err)
	}
	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingRollback, pd.Status)

	stats, err := fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.RolledBack)
	pd, err = fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, pd.Status)
	require.Equal(t, 100, fx.base.cache.stock[stockKey(a.ID)])
}

func TestRollbackRecoversWhenUnflushedRedisMutationIsLost(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID)
	require.NoError(t, err)
	require.NoError(t, fx.pd.MarkPendingRollback(context.Background(), nil, result.PreDeductionID, "order cancelled"))
	delete(fx.base.cache.stock, stockKey(a.ID))
	delete(fx.base.cache.count, countKey(a.ID, 42))
	delete(fx.base.cache.idem, idemKey(a.ID, 42))
	delete(fx.base.cache.idemToken, idemKey(a.ID, 42))
	delete(fx.base.cache.reservations, reservationKey(result.PreDeductionID))

	stats, err := fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.RolledBack)
	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, pd.Status)
	require.Equal(t, 100, fx.base.cache.stock[stockKey(a.ID)])
	require.Zero(t, fx.base.cache.count[countKey(a.ID, 42)])
}

func TestRecoveryDoesNotRollbackLivePreparingRequest(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: a.ID, Quantity: 1,
		Status: model.PreDeductionStatusPreparing,
	}
	require.NoError(t, fx.pd.Create(context.Background(), pd))
	_, err := fx.base.cache.AcquireIdempotency(
		context.Background(), idemKey(a.ID, 42), pd.ReservationToken(), idemTTL,
	)
	require.NoError(t, err)

	require.NoError(t, fx.svc.RecoverPreDeduction(context.Background(), pd.ID))
	stored, err := fx.pd.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPreparing, stored.Status)
	require.True(t, fx.base.cache.idem[idemKey(a.ID, 42)], "live request must keep its reservation")

	fx.pd.mu.Lock()
	fx.pd.byID[pd.ID].UpdatedAt = time.Now().Add(-preparingRecoveryDelay)
	fx.pd.mu.Unlock()
	require.NoError(t, fx.svc.RecoverPreDeduction(context.Background(), pd.ID))
	stored, err = fx.pd.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, stored.Status)
}

var _ repository.PreDeductionRepository = (*fakePreDeductions)(nil)
