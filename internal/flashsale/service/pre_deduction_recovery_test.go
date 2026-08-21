package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/flashsale/model"
	"github.com/xiangzhang-coding/go-single/internal/flashsale/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/limiter"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
	"github.com/xiangzhang-coding/go-single/internal/platform/transaction"
)

type fakePreDeductions struct {
	mu                   sync.Mutex
	nextID               int64
	byID                 map[int64]*model.PreDeduction
	markPreDeductedError error
	markReleasedError    error
}

func newFakePreDeductions() *fakePreDeductions {
	return &fakePreDeductions{byID: map[int64]*model.PreDeduction{}}
}

func (f *fakePreDeductions) Create(_ context.Context, p *model.PreDeduction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.ClientRequestID != "" {
		for _, existing := range f.byID {
			if existing.UserID == p.UserID && existing.ActivityID == p.ActivityID &&
				existing.ClientRequestID == p.ClientRequestID {
				return repository.ErrPreDeductionDuplicate
			}
		}
	}
	f.nextID++
	p.ID = f.nextID
	if p.PurchaseSlot == 0 {
		p.PurchaseSlot = p.ID
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
		p.UpdatedAt = p.CreatedAt
	}
	copy := *p
	f.byID[p.ID] = &copy
	return nil
}

func (f *fakePreDeductions) GetByRequestID(_ context.Context, userID, activityID int64, requestID string) (*model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.UserID == userID && p.ActivityID == activityID && p.ClientRequestID == requestID {
			copy := *p
			return &copy, nil
		}
	}
	return nil, nil
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

func (f *fakePreDeductions) GetByIDForUpdate(ctx context.Context, _ *transaction.Handle, id int64) (*model.PreDeduction, error) {
	return f.GetByID(ctx, id)
}

func (f *fakePreDeductions) EnsureLegacyPendingOrder(_ context.Context, seed *model.PreDeduction) (*model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pd := range f.byID {
		if pd.OrderNumber() == seed.OrderNumber() {
			if pd.PurchaseSlot == 0 {
				pd.PurchaseSlot = pd.ID
			}
			copy := *pd
			return &copy, nil
		}
	}
	f.nextID++
	seed.ID = f.nextID
	seed.PurchaseSlot = seed.ID
	seed.Status = model.PreDeductionStatusPendingOrder
	seed.Legacy = true
	seed.CreatedAt = time.Now()
	seed.UpdatedAt = seed.CreatedAt
	copy := *seed
	f.byID[seed.ID] = &copy
	return seed, nil
}

func (f *fakePreDeductions) ReservationTargets(_ context.Context, activityID, userID int64) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pending, user int
	for _, pd := range f.byID {
		if pd.ActivityID != activityID {
			continue
		}
		if pd.Status == model.PreDeductionStatusPendingPublish || pd.Status == model.PreDeductionStatusPendingOrder ||
			pd.Status == model.PreDeductionStatusPendingRollback {
			pending += pd.Quantity
		}
		if pd.UserID == userID && (pd.Status == model.PreDeductionStatusPendingPublish ||
			pd.Status == model.PreDeductionStatusPendingOrder || pd.Status == model.PreDeductionStatusOrdered ||
			pd.Status == model.PreDeductionStatusPendingRollback) {
			user++
		}
	}
	return pending, user, nil
}

func (f *fakePreDeductions) PendingReservationQuantityForUpdate(ctx context.Context, _ *transaction.Handle, activityID int64) (int, error) {
	pending, _, err := f.ReservationTargets(ctx, activityID, 0)
	return pending, err
}

func (f *fakePreDeductions) HasAcceptedReservationForUpdate(_ context.Context, _ *transaction.Handle, activityID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pd := range f.byID {
		if pd.ActivityID != activityID {
			continue
		}
		switch pd.Status {
		case model.PreDeductionStatusPendingPublish, model.PreDeductionStatusPendingOrder,
			model.PreDeductionStatusOrdered, model.PreDeductionStatusPendingRollback:
			return true, nil
		}
	}
	return false, nil
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

func (f *fakePreDeductions) ListRecoverableByActivity(ctx context.Context, activityID int64) ([]model.PreDeduction, error) {
	rows, err := f.ListRecoverable(ctx, 0)
	if err != nil {
		return nil, err
	}
	filtered := rows[:0]
	for i := range rows {
		if rows[i].ActivityID == activityID {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered, nil
}

func (f *fakePreDeductions) ListOrdered(context.Context, int) ([]model.PreDeduction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.PreDeduction
	for _, p := range f.byID {
		if p.Status == model.PreDeductionStatusOrdered && p.ReservationReleasedAt == nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (f *fakePreDeductions) MarkPreDeducted(_ context.Context, id int64) error {
	if f.markPreDeductedError != nil {
		return f.markPreDeductedError
	}
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
		if p.Status == model.PreDeductionStatusPendingPublish && p.PublishAttempts >= maxAttempts {
			p.Status = model.PreDeductionStatusPendingRollback
		}
	})
}

func (f *fakePreDeductions) MarkOrdered(_ context.Context, _ *transaction.Handle, id int64) error {
	return f.update(id, func(p *model.PreDeduction) { p.Status = model.PreDeductionStatusOrdered })
}

func (f *fakePreDeductions) MarkPendingRollback(_ context.Context, id int64, detail string) error {
	return f.update(id, func(p *model.PreDeduction) {
		switch p.Status {
		case model.PreDeductionStatusPreparing, model.PreDeductionStatusPendingPublish,
			model.PreDeductionStatusPendingOrder, model.PreDeductionStatusPendingRollback:
			p.Status = model.PreDeductionStatusPendingRollback
			p.LastError = detail
		}
	})
}

func (f *fakePreDeductions) EnsurePendingRollback(_ context.Context, _ *transaction.Handle, seed *model.PreDeduction) (*model.PreDeduction, error) {
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
	if f.markReleasedError != nil {
		return f.markReleasedError
	}
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

type racingConfirmPublisher struct {
	mu            sync.Mutex
	calls         int
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
}

func newRacingConfirmPublisher() *racingConfirmPublisher {
	return &racingConfirmPublisher{
		firstStarted: make(chan struct{}), secondStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
}

func (p *racingConfirmPublisher) Publish(context.Context, string, []byte) error {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.firstStarted)
		<-p.releaseFirst
		return nil
	}
	if call == 2 {
		close(p.secondStarted)
	}
	return errors.New("later confirm failed")
}

func (p *racingConfirmPublisher) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
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
	base.svc = asTestFlashSaleService(New(
		repository.Store{Activities: base.acts, PreDeductions: pd, Tx: base.acts}, base.products, base.cache,
		limiter.RedisCounterConfig{}, pub, nos, metrics.New().Business(),
	))
	return &recoveryFixture{svc: base.svc, base: base, pd: pd}
}

func (fx *recoveryFixture) publishedActivity(t *testing.T) *model.Activity {
	t.Helper()
	a := fx.base.createActivity(t, nil)
	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	return a
}

func TestUpdateOfflineActivityRejectsAcceptedUnsettledReservations(t *testing.T) {
	tests := []struct {
		name       string
		publishErr error
		wantStatus model.PreDeductionStatus
	}{
		{name: "pending publish", publishErr: errors.New("confirm unavailable"), wantStatus: model.PreDeductionStatusPendingPublish},
		{name: "pending order", wantStatus: model.PreDeductionStatusPendingOrder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := &fakePublisher{err: tt.publishErr}
			fx := newRecoveryFixture(pub, nil)
			a := fx.publishedActivity(t)
			result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "accepted-before-edit")
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, result.Status)
			require.NoError(t, fx.svc.UnpublishActivity(context.Background(), a.ID))
			pub.err = nil

			err = fx.svc.UpdateActivity(context.Background(), a.ID, ActivityParams{
				SKUID: a.SKUID, Title: a.Title, Price: a.Price, Stock: a.Stock,
				PerUserLimit: a.PerUserLimit,
				StartAt:      time.Now().Add(time.Hour),
				EndAt:        time.Now().Add(2 * time.Hour),
			})
			require.ErrorIs(t, err, ErrReservationsUnsettled)

			stored, getErr := fx.base.acts.GetByID(context.Background(), a.ID)
			require.NoError(t, getErr)
			require.True(t, stored.StartAt.Before(time.Now()), "rejected edit must not move the activity window")
			require.NotContains(t, fx.base.cache.stock, stockKey(a.ID), "rejected edit must not rewarm full stock")
		})
	}
}

func TestRepublishOfflineActivitySubtractsAcceptedReservations(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "accepted-before-republish")
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingOrder, result.Status)
	require.NoError(t, fx.svc.UnpublishActivity(context.Background(), a.ID))

	require.NoError(t, fx.svc.PublishActivity(context.Background(), a.ID))
	require.Equal(t, a.Stock-1, fx.base.cache.stock[stockKey(a.ID)],
		"republish must not expose stock reserved by an accepted pre-deduction")
}

func TestSeckillPersistsLifecycleAndPublishesStableIdentity(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
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

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
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
	delete(fx.base.cache.idem, slotIdemKey(a.ID, 42, result.PreDeductionID))
	delete(fx.base.cache.idemToken, slotIdemKey(a.ID, 42, result.PreDeductionID))
	delete(fx.base.cache.reservations, reservationKey(result.PreDeductionID))

	pub.err = nil
	restarted := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd, Tx: fx.base.acts}, fx.base.products, fx.base.cache,
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

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	require.Empty(t, result.OrderNo)
	require.Equal(t, model.PreDeductionStatusPendingPublish, result.Status)

	restarted := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd, Tx: fx.base.acts}, fx.base.products, fx.base.cache,
		limiter.RedisCounterConfig{}, fx.base.pub, &fakeNos{}, metrics.New().Business())
	stats, err := restarted.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Published)
}

func TestSeckillReturnsLifecycleWhenPreDeductTransitionFails(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	fx.pd.markPreDeductedError = errors.New("mysql transition failed")

	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	require.NotZero(t, result.PreDeductionID)
	require.Equal(t, model.PreDeductionStatusPreparing, result.Status)
	require.Equal(t, 99, fx.base.cache.stock[stockKey(a.ID)], "Redis pre-deduction already succeeded")
}

func TestRecoverPreDeductionRetriesFailedCancellationCompensation(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	require.NoError(t, fx.pd.MarkPendingRollback(context.Background(), result.PreDeductionID, "order cancelled"))

	fx.base.cache.err = errors.New("redis down")
	stats, err := fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.Failed)
	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingRollback, pd.Status)
	require.Equal(t, 1, pd.RollbackAttempts)

	fx.base.cache.err = nil
	restarted := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd, Tx: fx.base.acts}, fx.base.products, fx.base.cache,
		limiter.RedisCounterConfig{}, fx.base.pub, &fakeNos{}, metrics.New().Business())
	stats, err = restarted.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, stats.RolledBack)
	pd, err = fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, pd.Status)
	require.Equal(t, 100, fx.base.cache.stock[stockKey(a.ID)])
	require.Zero(t, fx.base.cache.count[countKey(a.ID, 42)])
	require.False(t, fx.base.cache.idem[slotIdemKey(a.ID, 42, result.PreDeductionID)])
}

func TestPublishRecoveryExhaustionCompletesRollback(t *testing.T) {
	pub := &fakePublisher{err: errors.New("rabbitmq unavailable")}
	fx := newRecoveryFixture(pub, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
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

func TestPendingOrderRecoveryRepairsReservationWithoutRepublishing(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingOrder, result.Status)
	initialPublishes := fx.base.pub.attemptsCount()
	delete(fx.base.cache.stock, stockKey(a.ID))
	delete(fx.base.cache.count, countKey(a.ID, 42))
	delete(fx.base.cache.idem, slotIdemKey(a.ID, 42, result.PreDeductionID))
	delete(fx.base.cache.idemToken, slotIdemKey(a.ID, 42, result.PreDeductionID))
	delete(fx.base.cache.reservations, reservationKey(result.PreDeductionID))

	stats, err := fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	require.Zero(t, stats.Published)
	require.Zero(t, stats.Failed)
	require.Equal(t, initialPublishes, fx.base.pub.attemptsCount(),
		"a broker-confirmed message must never be republished by recovery")
	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingOrder, pd.Status)
	require.Equal(t, 99, fx.base.cache.stock[stockKey(a.ID)])
	require.Equal(t, 1, fx.base.cache.count[countKey(a.ID, 42)])
	require.Equal(t, pd.ReservationToken(), fx.base.cache.reservations[reservationKey(pd.ID)])
}

func TestConcurrentRecoverySerializesPublishByPreDeductionID(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	orderNo := "concurrent-confirm"
	pd := &model.PreDeduction{
		UserID: 42, ActivityID: a.ID, OrderNo: &orderNo, SKUID: a.SKUID, Price: a.Price, Quantity: 1,
		Status: model.PreDeductionStatusPendingPublish, PublishAttempts: maxPublishAttempts - 1,
	}
	require.NoError(t, fx.pd.Create(context.Background(), pd))
	fx.base.cache.stock[stockKey(a.ID)] = a.Stock - 1
	fx.base.cache.count[countKey(a.ID, pd.UserID)] = 1
	fx.base.cache.idem[slotIdemKey(a.ID, pd.UserID, pd.PurchaseSlot)] = true
	fx.base.cache.idemToken[slotIdemKey(a.ID, pd.UserID, pd.PurchaseSlot)] = pd.ReservationToken()
	fx.base.cache.reservations[reservationKey(pd.ID)] = pd.ReservationToken()

	pub := newRacingConfirmPublisher()
	svc := New(repository.Store{Activities: fx.base.acts, PreDeductions: fx.pd, Tx: fx.base.acts},
		fx.base.products, fx.base.cache, limiter.RedisCounterConfig{}, pub, &fakeNos{}, metrics.New().Business())
	done := make(chan error, 2)
	go func() { done <- svc.RecoverPreDeduction(context.Background(), pd.ID) }()
	<-pub.firstStarted
	go func() { done <- svc.RecoverPreDeduction(context.Background(), pd.ID) }()

	secondPublished := false
	select {
	case <-pub.secondStarted:
		secondPublished = true
	case <-time.After(200 * time.Millisecond):
	}
	close(pub.releaseFirst)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	require.False(t, secondPublished, "the second recovery must wait, reload pending_order, and skip publish")
	require.Equal(t, 1, pub.callCount())

	stored, err := fx.pd.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingOrder, stored.Status)
	require.Equal(t, maxPublishAttempts-1, stored.PublishAttempts)
}

func TestRecoveryStopsWhenContextIsCancelled(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	_, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = fx.svc.RecoverPreDeductions(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRollbackRecoversWhenUnflushedRedisMutationIsLost(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	require.NoError(t, fx.pd.MarkPendingRollback(context.Background(), result.PreDeductionID, "order cancelled"))
	delete(fx.base.cache.stock, stockKey(a.ID))
	delete(fx.base.cache.count, countKey(a.ID, 42))
	delete(fx.base.cache.idem, slotIdemKey(a.ID, 42, result.PreDeductionID))
	delete(fx.base.cache.idemToken, slotIdemKey(a.ID, 42, result.PreDeductionID))
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

func TestRollbackDoesNotAdvanceWhenAOFConfirmationFails(t *testing.T) {
	fx := newRecoveryFixture(nil, nil)
	a := fx.publishedActivity(t)
	result, err := fx.svc.Seckill(context.Background(), 42, a.ID, "recovery")
	require.NoError(t, err)
	require.NoError(t, fx.pd.MarkPendingRollback(context.Background(), result.PreDeductionID, "order cancelled"))

	fx.base.cache.aofErr = errors.New("WAITAOF timeout")
	_, err = fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	pd, err := fx.pd.GetByID(context.Background(), result.PreDeductionID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPendingRollback, pd.Status)
	require.Equal(t, 100, fx.base.cache.stock[stockKey(a.ID)])

	// Redis acknowledged the restore but crashed before AOF fsync, replaying the
	// earlier deducted reservation. Recovery must apply it again and restart the wait.
	fx.base.cache.stock[stockKey(a.ID)] = 99
	fx.base.cache.count[countKey(a.ID, 42)] = 1
	fx.base.cache.idem[slotIdemKey(a.ID, 42, pd.PurchaseSlot)] = true
	fx.base.cache.idemToken[slotIdemKey(a.ID, 42, pd.PurchaseSlot)] = pd.ReservationToken()
	fx.base.cache.reservations[reservationKey(pd.ID)] = pd.ReservationToken()
	fx.base.cache.aofErr = nil
	_, err = fx.svc.RecoverPreDeductions(context.Background())
	require.NoError(t, err)
	pd, err = fx.pd.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, pd.Status)
	require.Equal(t, 100, fx.base.cache.stock[stockKey(a.ID)])
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
		context.Background(), slotIdemKey(a.ID, 42, pd.PurchaseSlot), pd.ReservationToken(), idemTTL,
	)
	require.NoError(t, err)

	require.NoError(t, fx.svc.RecoverPreDeduction(context.Background(), pd.ID))
	stored, err := fx.pd.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusPreparing, stored.Status)
	require.True(t, fx.base.cache.idem[slotIdemKey(a.ID, 42, pd.PurchaseSlot)], "live request must keep its reservation")

	fx.pd.mu.Lock()
	fx.pd.byID[pd.ID].UpdatedAt = time.Now().Add(-preparingRecoveryDelay)
	fx.pd.mu.Unlock()
	require.NoError(t, fx.svc.RecoverPreDeduction(context.Background(), pd.ID))
	stored, err = fx.pd.GetByID(context.Background(), pd.ID)
	require.NoError(t, err)
	require.Equal(t, model.PreDeductionStatusRolledBack, stored.Status)
}

var _ repository.PreDeductionRepository = (*fakePreDeductions)(nil)
