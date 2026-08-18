// service 层单元测试（中间 seam）：fake 仓储 + fake 类型化缓存（互斥锁模拟原子），
// 覆盖模板校验、领券各失败分支、并发不超发（总量/每人限领）、列表状态派生。
package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/xiangzhang-coding/go-single/internal/coupon/model"
	"github.com/xiangzhang-coding/go-single/internal/coupon/repository"
	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/metrics"
)

// ---- fake 仓储 ----

type fakeTemplates struct {
	byID  map[int64]*model.CouponTemplate
	order int64
}

func newFakeTemplates() *fakeTemplates {
	return &fakeTemplates{byID: map[int64]*model.CouponTemplate{}}
}

func (f *fakeTemplates) Create(_ context.Context, t *model.CouponTemplate) error {
	f.order++
	t.ID = f.order
	t.CreatedAt = time.Now()
	f.byID[t.ID] = t
	return nil
}

func (f *fakeTemplates) Update(_ context.Context, t *model.CouponTemplate) error {
	if v, ok := f.byID[t.ID]; ok {
		v.Name, v.Type, v.Value, v.MinAmount = t.Name, t.Type, t.Value, t.MinAmount
		v.Total, v.PerUserLimit, v.ValidFrom, v.ValidUntil = t.Total, t.PerUserLimit, t.ValidFrom, t.ValidUntil
	}
	return nil
}

func (f *fakeTemplates) GetByID(_ context.Context, id int64) (*model.CouponTemplate, error) {
	return f.byID[id], nil
}

func (f *fakeTemplates) List(context.Context) ([]model.CouponTemplate, error) {
	out := make([]model.CouponTemplate, 0, len(f.byID))
	for _, v := range f.byID {
		out = append(out, *v)
	}
	// 与 GORM 实现一致：id 升序。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type fakeUserCoupons struct {
	byID      map[int64]*model.UserCoupon
	tmpls     *fakeTemplates
	order     int64
	mu        sync.Mutex
	nowFn     func() time.Time
	createErr error
}

func newFakeUserCoupons(tmpls *fakeTemplates) *fakeUserCoupons {
	return &fakeUserCoupons{byID: map[int64]*model.UserCoupon{}, tmpls: tmpls, nowFn: time.Now}
}

func (f *fakeUserCoupons) Claim(_ context.Context, userID, templateID int64) (repository.ClaimOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	template, ok := f.tmpls.byID[templateID]
	if !ok {
		return repository.ClaimOutcome{Result: repository.ClaimTemplateNotFound}, nil
	}
	var claimedCount, perUserCount int64
	for _, coupon := range f.byID {
		if coupon.TemplateID == templateID {
			claimedCount++
			if coupon.UserID == userID {
				perUserCount++
			}
		}
	}
	outcome := repository.ClaimOutcome{ClaimedCount: claimedCount, PerUserCount: perUserCount}
	now := time.Now()
	switch {
	case now.Before(template.ValidFrom) || now.After(template.ValidUntil):
		outcome.Result = repository.ClaimNotInWindow
	case claimedCount >= int64(template.Total):
		outcome.Result = repository.ClaimSoldOut
	case perUserCount >= int64(template.PerUserLimit):
		outcome.Result = repository.ClaimLimitReached
	default:
		if f.createErr != nil {
			return repository.ClaimOutcome{}, f.createErr
		}
		f.order++
		coupon := &model.UserCoupon{
			ID: f.order, UserID: userID, TemplateID: templateID,
			Status: model.CouponStatusUnused, CreatedAt: now, UpdatedAt: now,
		}
		f.byID[coupon.ID] = coupon
		outcome.Result = repository.ClaimCreated
		outcome.Coupon = coupon
		outcome.ClaimedCount++
		outcome.PerUserCount++
	}
	return outcome, nil
}

// ListByUser 模拟 GORM 实现：JOIN 模板 + 派生状态（used → used；未用过期 → expired）。
func (f *fakeUserCoupons) ListByUser(_ context.Context, userID int64, status string, offset, limit int) ([]model.UserCouponView, int64, error) {
	now := f.nowFn()
	matched := make([]model.UserCouponView, 0, len(f.byID))
	for _, c := range f.byID {
		if c.UserID != userID {
			continue
		}
		t, ok := f.tmpls.byID[c.TemplateID]
		if !ok {
			continue
		}
		derived := c.Status
		if c.Status == model.CouponStatusUnused && now.After(t.ValidUntil) {
			derived = model.CouponStatusExpired
		}
		if status != "" && derived != status {
			continue
		}
		matched = append(matched, model.UserCouponView{
			ID:         c.ID,
			TemplateID: t.ID,
			Name:       t.Name,
			Type:       t.Type,
			Value:      t.Value,
			MinAmount:  t.MinAmount,
			Status:     derived,
			ValidFrom:  t.ValidFrom,
			ValidUntil: t.ValidUntil,
			UsedAt:     c.UsedAt,
			CreatedAt:  c.CreatedAt,
		})
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID > matched[j].ID })
	return slicePage(matched, offset, limit), int64(len(matched)), nil
}

func (f *fakeUserCoupons) CountByTemplate(_ context.Context, templateID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, c := range f.byID {
		if c.TemplateID == templateID {
			n++
		}
	}
	return n, nil
}

func (f *fakeUserCoupons) CountUserByTemplate(_ context.Context, userID, templateID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, c := range f.byID {
		if c.UserID == userID && c.TemplateID == templateID {
			n++
		}
	}
	return n, nil
}

// viewOf 组装 UserCouponView（与 ListByUser 相同的 JOIN 语义）。
func (f *fakeUserCoupons) viewOf(c *model.UserCoupon) *model.UserCouponView {
	t, ok := f.tmpls.byID[c.TemplateID]
	if !ok {
		return nil
	}
	now := f.nowFn()
	derived := c.Status
	if c.Status == model.CouponStatusUnused && now.After(t.ValidUntil) {
		derived = model.CouponStatusExpired
	}
	return &model.UserCouponView{
		ID:         c.ID,
		TemplateID: t.ID,
		Name:       t.Name,
		Type:       t.Type,
		Value:      t.Value,
		MinAmount:  t.MinAmount,
		Status:     derived,
		ValidFrom:  t.ValidFrom,
		ValidUntil: t.ValidUntil,
		UsedAt:     c.UsedAt,
		CreatedAt:  c.CreatedAt,
	}
}

// GetViewByID 单张券（归属过滤）：不存在返回 (nil, nil)。
func (f *fakeUserCoupons) GetViewByID(_ context.Context, userID, couponID int64) (*model.UserCouponView, error) {
	c, ok := f.byID[couponID]
	if !ok || c.UserID != userID {
		return nil, nil
	}
	return f.viewOf(c), nil
}

// Use 条件核销：unused→used + 有效期窗口（镜像 GORM 实现）；tx 忽略（单测无真实事务）。
func (f *fakeUserCoupons) Use(_ context.Context, _ *gorm.DB, userID, couponID int64) (bool, error) {
	c, ok := f.byID[couponID]
	if !ok || c.UserID != userID || c.Status != model.CouponStatusUnused {
		return false, nil
	}
	t, ok := f.tmpls.byID[c.TemplateID]
	if !ok {
		return false, nil
	}
	now := f.nowFn()
	if now.Before(t.ValidFrom) || now.After(t.ValidUntil) {
		return false, nil
	}
	c.Status = model.CouponStatusUsed
	at := f.nowFn()
	c.UsedAt = &at
	return true, nil
}

// Rollback 条件回退：used→unused。
func (f *fakeUserCoupons) Rollback(_ context.Context, _ *gorm.DB, userID, couponID int64) (bool, error) {
	c, ok := f.byID[couponID]
	if !ok || c.UserID != userID || c.Status != model.CouponStatusUsed {
		return false, nil
	}
	c.Status = model.CouponStatusUnused
	c.UsedAt = nil
	return true, nil
}

func slicePage[T any](in []T, offset, limit int) []T {
	if offset > len(in) {
		return nil
	}
	end := offset + limit
	if end > len(in) {
		end = len(in)
	}
	return in[offset:end]
}

// ---- fake 类型化缓存（互斥锁模拟原子性）----

type fakeClaimCache struct {
	mu         sync.Mutex
	claimed    map[string]int
	perUser    map[string]int
	versions   map[string]int
	err        error
	claimDelay time.Duration
}

func newFakeClaimCache() *fakeClaimCache {
	return &fakeClaimCache{claimed: map[string]int{}, perUser: map[string]int{}, versions: map[string]int{}}
}

func (f *fakeClaimCache) Ping(context.Context) error { return nil }
func (f *fakeClaimCache) Close() error               { return nil }
func (f *fakeClaimCache) Get(context.Context, string) (string, error) {
	return "", cache.ErrMiss
}
func (f *fakeClaimCache) Set(context.Context, string, string, time.Duration) error { return nil }
func (f *fakeClaimCache) Del(context.Context, string) error                        { return nil }

func (f *fakeClaimCache) ClaimCoupon(ctx context.Context, p cache.CouponClaimParams) (cache.CouponClaimResult, error) {
	if f.claimDelay > 0 {
		select {
		case <-time.After(f.claimDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	if p.Now.Before(p.ValidFrom) || p.Now.After(p.ValidUntil) {
		return cache.CouponNotInWindow, nil
	}
	if f.claimed[p.ClaimedKey] < int(p.ClaimedCount) {
		f.claimed[p.ClaimedKey] = int(p.ClaimedCount)
	}
	if f.perUser[p.PerUserKey] < int(p.PerUserCount) {
		f.perUser[p.PerUserKey] = int(p.PerUserCount)
	}
	if f.versions[p.VersionKey] < int(p.ClaimedCount) {
		f.versions[p.VersionKey] = int(p.ClaimedCount)
	}
	if f.versions[p.PerUserVersionKey] < int(p.PerUserCount) {
		f.versions[p.PerUserVersionKey] = int(p.PerUserCount)
	}
	if f.claimed[p.ClaimedKey] >= p.Total {
		return cache.CouponSoldOut, nil
	}
	if f.perUser[p.PerUserKey] >= p.PerUserLimit {
		return cache.CouponLimitReached, nil
	}
	f.claimed[p.ClaimedKey]++
	f.perUser[p.PerUserKey]++
	return cache.CouponClaimed, nil
}

func (f *fakeClaimCache) SyncCouponCounts(_ context.Context, p cache.CouponCountParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if int(p.ClaimedCount) >= f.versions[p.VersionKey] {
		f.claimed[p.ClaimedKey] = int(p.ClaimedCount)
		f.versions[p.VersionKey] = int(p.ClaimedCount)
	}
	if int(p.PerUserCount) >= f.versions[p.PerUserVersionKey] {
		f.perUser[p.PerUserKey] = int(p.PerUserCount)
		f.versions[p.PerUserVersionKey] = int(p.PerUserCount)
	}
	return nil
}

// ---- 测试夹具 ----

type fixture struct {
	svc   Service
	tmpls *fakeTemplates
	coups *fakeUserCoupons
	cache *fakeClaimCache
}

func newFixture() *fixture {
	tmpls := newFakeTemplates()
	coups := newFakeUserCoupons(tmpls)
	fc := newFakeClaimCache()
	svc := New(repository.Store{Template: tmpls, UserCoupon: coups}, fc, metrics.New().Business())
	return &fixture{svc: svc, tmpls: tmpls, coups: coups, cache: fc}
}

// 有效窗口模板：开始于 1 分钟前，结束于 1 小时后。
func (fx *fixture) createTemplate(t *testing.T, mutate func(*TemplateParams)) *model.CouponTemplate {
	t.Helper()
	p := TemplateParams{
		Name:         "满100减20",
		Type:         model.TemplateTypeThreshold,
		Value:        2000,
		MinAmount:    10000,
		Total:        100,
		PerUserLimit: 1,
		ValidFrom:    time.Now().Add(-time.Minute),
		ValidUntil:   time.Now().Add(time.Hour),
	}
	if mutate != nil {
		mutate(&p)
	}
	tmpl, err := fx.svc.CreateTemplate(context.Background(), p)
	require.NoError(t, err)
	return tmpl
}

// ---- 模板（admin）----

func TestCreateTemplateValidation(t *testing.T) {
	fx := newFixture()
	base := TemplateParams{
		Name: "新人券", Type: model.TemplateTypeDirect, Value: 500, Total: 10,
		PerUserLimit: 1, ValidFrom: time.Now(), ValidUntil: time.Now().Add(time.Hour),
	}

	cases := []struct {
		name   string
		mutate func(*TemplateParams)
	}{
		{"空白名称", func(p *TemplateParams) { p.Name = "  " }},
		{"非法类型", func(p *TemplateParams) { p.Type = "random" }},
		{"面额为 0", func(p *TemplateParams) { p.Value = 0 }},
		{"满减门槛低于面额", func(p *TemplateParams) { p.Type = model.TemplateTypeThreshold; p.MinAmount = p.Value - 1 }},
		{"总量为 0", func(p *TemplateParams) { p.Total = 0 }},
		{"每人限领为 0", func(p *TemplateParams) { p.PerUserLimit = 0 }},
		{"有效期倒置", func(p *TemplateParams) { p.ValidUntil = p.ValidFrom.Add(-time.Hour) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			_, err := fx.svc.CreateTemplate(context.Background(), p)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}

	// 直减强制门槛为 0。
	tmpl, err := fx.svc.CreateTemplate(context.Background(), base)
	require.NoError(t, err)
	assert.Equal(t, int64(0), tmpl.MinAmount, "直减券门槛应强制为 0")

	// 名称 trim。
	assert.Equal(t, "新人券", tmpl.Name)
}

func TestUpdateTemplate(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, nil)

	p := TemplateParams{
		Name: "改名券", Type: model.TemplateTypeDirect, Value: 300, Total: 50,
		PerUserLimit: 2, ValidFrom: time.Now(), ValidUntil: time.Now().Add(2 * time.Hour),
	}
	require.NoError(t, fx.svc.UpdateTemplate(context.Background(), tmpl.ID, p))
	got, err := fx.svc.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "改名券", got[0].Name)
	assert.Equal(t, 2, got[0].PerUserLimit)
	assert.Equal(t, int64(0), got[0].ClaimedCount, "后台列表应携带已领数")

	// 领券后后台列表已领数随之更新。
	_, err = fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err)
	got, err = fx.svc.ListTemplates(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), got[0].ClaimedCount)

	// 不存在的模板 → 404 语义。
	require.ErrorIs(t, fx.svc.UpdateTemplate(context.Background(), 999, p), ErrTemplateNotFound)
}

// ---- 领券 ----

func TestClaimSuccess(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, nil)

	uc, err := fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(42), uc.UserID)
	assert.Equal(t, tmpl.ID, uc.TemplateID)
	assert.Equal(t, model.CouponStatusUnused, uc.Status)

	// 计数已入 Redis。
	assert.Equal(t, 1, fx.cache.claimed[claimedKey(tmpl.ID)])
	assert.Equal(t, 1, fx.cache.perUser[perUserKey(tmpl.ID, 42)])
}

func TestClaimFailures(t *testing.T) {
	t.Run("模板不存在", func(t *testing.T) {
		fx := newFixture()
		_, err := fx.svc.Claim(context.Background(), 1, 999)
		require.ErrorIs(t, err, ErrTemplateNotFound)
	})

	t.Run("已抢光", func(t *testing.T) {
		fx := newFixture()
		tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.Total = 1 })
		_, err := fx.svc.Claim(context.Background(), 1, tmpl.ID)
		require.NoError(t, err)
		_, err = fx.svc.Claim(context.Background(), 2, tmpl.ID)
		require.ErrorIs(t, err, ErrSoldOut)
	})

	t.Run("超过每人限领", func(t *testing.T) {
		fx := newFixture()
		tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.PerUserLimit = 1 })
		_, err := fx.svc.Claim(context.Background(), 1, tmpl.ID)
		require.NoError(t, err)
		_, err = fx.svc.Claim(context.Background(), 1, tmpl.ID)
		require.ErrorIs(t, err, ErrClaimLimitReached)
	})

	t.Run("未开始", func(t *testing.T) {
		fx := newFixture()
		tmpl := fx.createTemplate(t, func(p *TemplateParams) {
			p.ValidFrom = time.Now().Add(time.Hour)
			p.ValidUntil = time.Now().Add(2 * time.Hour)
		})
		_, err := fx.svc.Claim(context.Background(), 1, tmpl.ID)
		require.ErrorIs(t, err, ErrNotInWindow)
	})

	t.Run("已过期", func(t *testing.T) {
		fx := newFixture()
		tmpl := fx.createTemplate(t, func(p *TemplateParams) {
			p.ValidFrom = time.Now().Add(-2 * time.Hour)
			p.ValidUntil = time.Now().Add(-time.Minute)
		})
		_, err := fx.svc.Claim(context.Background(), 1, tmpl.ID)
		require.ErrorIs(t, err, ErrNotInWindow)
	})

	t.Run("缓存故障降级到数据库领取", func(t *testing.T) {
		fx := newFixture()
		tmpl := fx.createTemplate(t, nil)
		fx.cache.err = context.DeadlineExceeded
		coupon, err := fx.svc.Claim(context.Background(), 1, tmpl.ID)
		require.NoError(t, err)
		require.NotNil(t, coupon)
	})
}

func TestClaimDatabaseFailureRestoresQuota(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, nil)
	dbErr := errors.New("insert failed")
	fx.coups.createErr = dbErr

	_, err := fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.ErrorIs(t, err, dbErr)
	require.Equal(t, 0, fx.cache.claimed[claimedKey(tmpl.ID)])
	require.Equal(t, 0, fx.cache.perUser[perUserKey(tmpl.ID, 42)])

	fx.coups.createErr = nil
	_, err = fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err, "数据库恢复后不得被泄漏的 Redis 额度永久阻塞")
}

func TestClaimRebuildsLostCacheFromDatabaseFacts(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, nil)

	_, err := fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err)
	fx.cache.claimed = map[string]int{}
	fx.cache.perUser = map[string]int{}
	fx.cache.versions = map[string]int{}

	_, err = fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.ErrorIs(t, err, ErrClaimLimitReached)
	require.Len(t, fx.coups.byID, 1)
	require.Equal(t, 1, fx.cache.claimed[claimedKey(tmpl.ID)])
	require.Equal(t, 1, fx.cache.perUser[perUserKey(tmpl.ID, 42)])
}

func TestClaimRepairsStaleCacheEvenWhenBothStoresAccept(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.PerUserLimit = 2 })

	_, err := fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err)
	fx.cache.claimed[claimedKey(tmpl.ID)] = 2

	_, err = fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err)
	require.Equal(t, 2, fx.cache.claimed[claimedKey(tmpl.ID)], "成功路径也应按最新数据库事实修复泄漏额度")
	require.Equal(t, 2, fx.cache.perUser[perUserKey(tmpl.ID, 42)])
}

func TestClaimConcurrentDatabaseGuardWhenCacheUnavailable(t *testing.T) {
	const (
		workers = 20
		total   = 5
	)
	fx := newFixture()
	tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.Total = total })
	fx.cache.err = context.DeadlineExceeded

	var wg sync.WaitGroup
	results := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = fx.svc.Claim(context.Background(), int64(100+i), tmpl.ID)
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, ErrSoldOut)
		}
	}
	require.Equal(t, total, succeeded)
	require.Len(t, fx.coups.byID, total)
}

func TestClaimPreservesDatabaseBudgetWhenCacheIsSlow(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, nil)
	fx.cache.claimDelay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	started := time.Now()
	coupon, err := fx.svc.Claim(ctx, 42, tmpl.ID)
	require.NoError(t, err)
	require.NotNil(t, coupon)
	require.Less(t, time.Since(started), 700*time.Millisecond, "Redis 不得耗尽留给 MySQL 的请求预算")
}

// 并发领券不超发：总量与每人限领均原子强制（fake 缓存以锁模拟原子性）。
func TestClaimConcurrentNotOversell(t *testing.T) {
	const (
		workers = 20
		total   = 5
	)
	fx := newFixture()
	tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.Total = total })

	// 20 个不同用户并发抢 total=5：恰好 5 人成功，15 人 sold out。
	var wg sync.WaitGroup
	results := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = fx.svc.Claim(context.Background(), int64(100+i), tmpl.ID)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, ErrSoldOut)
		}
	}
	require.Equal(t, total, ok, "并发领券不得超发")
	require.Equal(t, total, fx.cache.claimed[claimedKey(tmpl.ID)])
	require.Equal(t, total, len(fx.coups.byID), "DB 落库条数应等于成功数")
}

func TestClaimConcurrentPerUserLimit(t *testing.T) {
	const workers = 10
	fx := newFixture()
	tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.PerUserLimit = 2 })

	var wg sync.WaitGroup
	results := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = fx.svc.Claim(context.Background(), 7, tmpl.ID)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, ErrClaimLimitReached)
		}
	}
	require.Equal(t, 2, ok, "同一用户并发领取不得超过每人限领")
	require.Equal(t, 2, fx.cache.perUser[perUserKey(tmpl.ID, 7)])
}

// ---- 可领券列表 ----

func TestListClaimableStates(t *testing.T) {
	fx := newFixture()
	now := time.Now()

	fx.createTemplate(t, func(p *TemplateParams) { p.Name = "可领" })
	fx.createTemplate(t, func(p *TemplateParams) {
		p.Name = "未开始"
		p.ValidFrom = now.Add(time.Hour)
		p.ValidUntil = now.Add(2 * time.Hour)
	})
	fx.createTemplate(t, func(p *TemplateParams) {
		p.Name = "已结束"
		p.ValidFrom = now.Add(-2 * time.Hour)
		p.ValidUntil = now.Add(-time.Minute)
	})
	fx.createTemplate(t, func(p *TemplateParams) { p.Name = "已抢光"; p.Total = 1 })

	// 用户 1 领取"可领"与"已抢光"各一张。
	userID := int64(42)
	for _, name := range []string{"可领", "已抢光"} {
		for _, tmpl := range fx.tmpls.byID {
			if tmpl.Name == name {
				_, err := fx.svc.Claim(context.Background(), userID, tmpl.ID)
				require.NoError(t, err)
			}
		}
	}

	// 用户 2 无领取记录，但"已抢光"总量已满。
	views, err := fx.svc.ListClaimable(context.Background(), 999)
	require.NoError(t, err)
	states := map[string]string{}
	for _, v := range views {
		states[v.Name] = v.State
	}
	assert.Equal(t, stateClaimable, states["可领"])
	assert.Equal(t, stateNotStarted, states["未开始"])
	assert.Equal(t, stateEnded, states["已结束"])
	assert.Equal(t, stateSoldOut, states["已抢光"])

	// 用户 1 已达每人限领（per_user_limit=1）。
	views, err = fx.svc.ListClaimable(context.Background(), userID)
	require.NoError(t, err)
	for _, v := range views {
		if v.Name == "可领" {
			assert.Equal(t, stateLimitReached, v.State)
		}
	}
}

// ---- 我的券 ----

func TestListMineStatusAndPagination(t *testing.T) {
	fx := newFixture()
	userID := int64(42)

	valid := fx.createTemplate(t, func(p *TemplateParams) { p.Name = "有效券" })
	expiredT := fx.createTemplate(t, func(p *TemplateParams) { p.Name = "过期券" })

	uc1, err := fx.svc.Claim(context.Background(), userID, valid.ID)
	require.NoError(t, err)
	uc2, err := fx.svc.Claim(context.Background(), userID, expiredT.ID)
	require.NoError(t, err)

	// 模拟有效期已过：模板有效期改到过去（领取记录本身不变）。
	expiredT.ValidUntil = time.Now().Add(-time.Minute)

	// 全部：有效券为 unused，过期券派生为 expired。
	all, total, err := fx.svc.ListMine(context.Background(), userID, "", 1, 20)
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, all, 2)
	assert.Equal(t, model.CouponStatusExpired, all[0].Status, "最新一条为过期券")
	assert.Equal(t, "过期券", all[0].Name)
	assert.Equal(t, model.CouponStatusUnused, all[1].Status)

	// 筛选 expired：仅过期券。
	expiredList, total, err := fx.svc.ListMine(context.Background(), userID, model.CouponStatusExpired, 1, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, expiredList, 1)
	assert.Equal(t, uc2.ID, expiredList[0].ID)

	// 筛选 unused：排除过期券。
	unusedList, total, err := fx.svc.ListMine(context.Background(), userID, model.CouponStatusUnused, 1, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, unusedList, 1)
	assert.Equal(t, uc1.ID, unusedList[0].ID)

	// 分页：page=1, page_size=1 取最新一条，total 仍为 2。
	page1, total, err := fx.svc.ListMine(context.Background(), userID, "", 1, 1)
	require.NoError(t, err)
	require.Len(t, page1, 1)
	assert.Equal(t, int64(2), total)
	assert.Equal(t, uc2.ID, page1[0].ID)
}

func TestListMineExpiryDerivedFromNow(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.ValidUntil = time.Now().Add(10 * time.Minute) })
	uc, err := fx.svc.Claim(context.Background(), 42, tmpl.ID)
	require.NoError(t, err)

	// 当前未过期 → unused。
	list, _, err := fx.svc.ListMine(context.Background(), 42, "", 1, 20)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, model.CouponStatusUnused, list[0].Status)

	// 时间推进到过期后 → 派生为 expired（不落库）。
	fx.coups.nowFn = func() time.Time { return time.Now().Add(20 * time.Minute) }
	list, _, err = fx.svc.ListMine(context.Background(), 42, "", 1, 20)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, model.CouponStatusExpired, list[0].Status)
	assert.Equal(t, model.CouponStatusUnused, fx.coups.byID[uc.ID].Status, "过期状态不应落库")
}

// ---- 核销与回退（order 模块事务内调用） ----

// UseCoupon：正常核销；已用/不存在/事务内过期被正确区分。
func TestUseCouponClassification(t *testing.T) {
	fx := newFixture()
	// 每人限领 2：同一用户需领两张券。
	tmpl := fx.createTemplate(t, func(p *TemplateParams) { p.PerUserLimit = 2 })
	userID := int64(42)

	// 不存在 → ErrCouponNotFound。
	err := fx.svc.UseCoupon(context.Background(), nil, userID, 999)
	require.ErrorIs(t, err, ErrCouponNotFound)

	// 正常核销。
	uc, err := fx.svc.Claim(context.Background(), userID, tmpl.ID)
	require.NoError(t, err)
	require.NoError(t, fx.svc.UseCoupon(context.Background(), nil, userID, uc.ID))
	require.Equal(t, model.CouponStatusUsed, fx.coups.byID[uc.ID].Status)

	// 已用 → ErrCouponUsed（非"不存在"）。
	err = fx.svc.UseCoupon(context.Background(), nil, userID, uc.ID)
	require.ErrorIs(t, err, ErrCouponUsed)

	// 事务内过期（结算通过后券恰好到期）→ ErrCouponExpired（非 ErrCouponUsed）。
	uc2, err := fx.svc.Claim(context.Background(), userID, tmpl.ID)
	require.NoError(t, err)
	fx.tmpls.byID[tmpl.ID].ValidUntil = time.Now().Add(-time.Minute)
	err = fx.svc.UseCoupon(context.Background(), nil, userID, uc2.ID)
	require.ErrorIs(t, err, ErrCouponExpired)
	require.Equal(t, model.CouponStatusUnused, fx.coups.byID[uc2.ID].Status, "过期核销失败不得改变状态")
}

// RollbackCoupon：正常回退；非 used 状态回退失败。
func TestRollbackCoupon(t *testing.T) {
	fx := newFixture()
	tmpl := fx.createTemplate(t, nil)
	userID := int64(42)

	uc, err := fx.svc.Claim(context.Background(), userID, tmpl.ID)
	require.NoError(t, err)

	// 未核销直接回退 → ErrCouponRollbackFailed。
	err = fx.svc.RollbackCoupon(context.Background(), nil, userID, uc.ID)
	require.ErrorIs(t, err, ErrCouponRollbackFailed)

	// 核销后回退成功。
	require.NoError(t, fx.svc.UseCoupon(context.Background(), nil, userID, uc.ID))
	require.NoError(t, fx.svc.RollbackCoupon(context.Background(), nil, userID, uc.ID))
	require.Equal(t, model.CouponStatusUnused, fx.coups.byID[uc.ID].Status)
	require.Nil(t, fx.coups.byID[uc.ID].UsedAt)
}
