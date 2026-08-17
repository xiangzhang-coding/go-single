package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xiangzhang-coding/go-single/internal/testsupport"
)

func newAtomicRedis(t *testing.T) Client {
	t.Helper()
	c, err := NewRedis("127.0.0.1:6379", "", redisTestDB)
	testsupport.RequireDependency(t, "Redis", err)
	require.NoError(t, c.(*redisCache).client.ConfigSet(context.Background(), "appendonly", "yes").Err())
	require.NoError(t, c.(*redisCache).client.ConfigSet(context.Background(), "appendfsync", "always").Err())
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c
}

func atomicTestKey(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("cache_test:atomic:%d:%s", time.Now().UnixNano(), suffix)
}

func TestAcquireIdempotency(t *testing.T) {
	c := newAtomicRedis(t)
	ctx := context.Background()
	key := atomicTestKey(t, "idem")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })

	result, err := c.AcquireIdempotency(ctx, key, "order-1", 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, IdempotencyAcquired, result)

	value, err := c.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "order-1", value)

	ttl, err := c.(*redisCache).client.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 28*time.Second)
	require.LessOrEqual(t, ttl, 30*time.Second)

	result, err = c.AcquireIdempotency(ctx, key, "order-2", time.Minute)
	require.NoError(t, err)
	require.Equal(t, IdempotencyExists, result)
	value, err = c.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "order-1", value, "重复抢占不得覆盖原值")
}

func TestAcquireIdempotencyPropagatesRedisError(t *testing.T) {
	c := newAtomicRedis(t)
	key := atomicTestKey(t, "idem-error")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.AcquireIdempotency(ctx, key, "order-1", time.Minute)
	require.ErrorIs(t, err, context.Canceled)

	_, err = c.Get(context.Background(), key)
	require.ErrorIs(t, err, ErrMiss, "失败的抢占不得留下幂等键")
}

func TestReleaseIdempotencyDurablyDeletesOnlyOwnedKey(t *testing.T) {
	c := newAtomicRedis(t)
	key := atomicTestKey(t, "idem-release")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })
	require.NoError(t, c.Set(context.Background(), key, "reservation-42", time.Minute))

	err := c.ReleaseIdempotencyDurably(context.Background(), key, "reservation-42", 2*time.Second)
	require.NoError(t, err)
	requireCacheState(t, c, key, "", false)
}

func TestClaimCouponResultsAndState(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name          string
		seedClaimed   string
		seedPerUser   string
		mutate        func(*CouponClaimParams)
		want          CouponClaimResult
		wantClaimed   string
		wantPerUser   string
		claimedExists bool
		perUserExists bool
	}{
		{
			name: "claimed", want: CouponClaimed,
			wantClaimed: "1", wantPerUser: "1", claimedExists: true, perUserExists: true,
		},
		{
			name: "sold out", seedClaimed: "2", want: CouponSoldOut,
			wantClaimed: "2", claimedExists: true,
		},
		{
			name: "not in window", want: CouponNotInWindow,
			mutate: func(p *CouponClaimParams) {
				p.ValidFrom = now.Add(time.Minute)
				p.ValidUntil = now.Add(time.Hour)
			},
		},
		{
			name: "limit reached", seedPerUser: "1", want: CouponLimitReached,
			wantPerUser: "1", perUserExists: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newAtomicRedis(t)
			claimedKey := atomicTestKey(t, "coupon-claimed")
			perUserKey := atomicTestKey(t, "coupon-per-user")
			t.Cleanup(func() {
				require.NoError(t, c.Del(context.Background(), claimedKey))
				require.NoError(t, c.Del(context.Background(), perUserKey))
			})
			if tc.seedClaimed != "" {
				require.NoError(t, c.Set(context.Background(), claimedKey, tc.seedClaimed, 0))
			}
			if tc.seedPerUser != "" {
				require.NoError(t, c.Set(context.Background(), perUserKey, tc.seedPerUser, 0))
			}

			params := CouponClaimParams{
				ClaimedKey: claimedKey, PerUserKey: perUserKey,
				Now: now, Total: 2, ValidFrom: now.Add(-time.Minute),
				ValidUntil: now.Add(time.Hour), PerUserLimit: 1,
			}
			if tc.mutate != nil {
				tc.mutate(&params)
			}
			result, err := c.ClaimCoupon(context.Background(), params)
			require.NoError(t, err)
			require.Equal(t, tc.want, result)
			requireCacheState(t, c, claimedKey, tc.wantClaimed, tc.claimedExists)
			requireCacheState(t, c, perUserKey, tc.wantPerUser, tc.perUserExists)
		})
	}
}

func TestClaimCouponPropagatesRedisError(t *testing.T) {
	c := newAtomicRedis(t)
	claimedKey := atomicTestKey(t, "coupon-error-claimed")
	perUserKey := atomicTestKey(t, "coupon-error-per-user")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), claimedKey))
		require.NoError(t, c.Del(context.Background(), perUserKey))
	})

	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ClaimCoupon(ctx, CouponClaimParams{
		ClaimedKey: claimedKey, PerUserKey: perUserKey,
		Now: now, Total: 2, ValidFrom: now.Add(-time.Minute),
		ValidUntil: now.Add(time.Hour), PerUserLimit: 1,
	})
	require.ErrorIs(t, err, context.Canceled)
	requireCacheState(t, c, claimedKey, "", false)
	requireCacheState(t, c, perUserKey, "", false)
}

func TestWarmFlashSaleStockResultsAndTTL(t *testing.T) {
	tests := []struct {
		name      string
		seed      string
		seedTTL   time.Duration
		stock     int
		ttl       time.Duration
		want      FlashSaleWarmResult
		wantStock string
		minTTL    time.Duration
		maxTTL    time.Duration
	}{
		{
			name: "missing stock is warmed", stock: 10, ttl: 30 * time.Second,
			want: FlashSaleStockUpdated, wantStock: "10", minTTL: 28 * time.Second, maxTTL: 30 * time.Second,
		},
		{
			name: "lower live stock is retained", seed: "4", seedTTL: 2 * time.Minute,
			stock: 10, ttl: 30 * time.Second, want: FlashSaleStockRetained,
			wantStock: "4", minTTL: 110 * time.Second, maxTTL: 2 * time.Minute,
		},
		{
			name: "configured decrease replaces live stock", seed: "20", seedTTL: 2 * time.Minute,
			stock: 10, ttl: 30 * time.Second, want: FlashSaleStockUpdated,
			wantStock: "10", minTTL: 28 * time.Second, maxTTL: 30 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newAtomicRedis(t)
			key := atomicTestKey(t, "flashsale-warm")
			t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })
			if tc.seed != "" {
				require.NoError(t, c.Set(context.Background(), key, tc.seed, tc.seedTTL))
			}

			result, err := c.WarmFlashSaleStock(context.Background(), FlashSaleWarmParams{
				StockKey: key, Stock: tc.stock, TTL: tc.ttl,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, result)
			requireCacheState(t, c, key, tc.wantStock, true)

			ttl, err := c.(*redisCache).client.TTL(context.Background(), key).Result()
			require.NoError(t, err)
			require.Greater(t, ttl, tc.minTTL)
			require.LessOrEqual(t, ttl, tc.maxTTL)
		})
	}
}

func TestWarmFlashSaleStockPropagatesRedisError(t *testing.T) {
	c := newAtomicRedis(t)
	key := atomicTestKey(t, "flashsale-warm-error")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.WarmFlashSaleStock(ctx, FlashSaleWarmParams{StockKey: key, Stock: 10, TTL: time.Minute})
	require.ErrorIs(t, err, context.Canceled)
	requireCacheState(t, c, key, "", false)
}

func TestPreDeductFlashSaleResultsAndState(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		stock     int
		count     int
		mutate    func(*FlashSalePreDeductParams)
		want      FlashSalePreDeductResult
		wantStock int
		wantCount int
	}{
		{name: "deducted", stock: 5, want: FlashSalePreDeducted, wantStock: 4, wantCount: 1},
		{
			name: "sold out", stock: 0, want: FlashSaleSoldOut,
			wantStock: 0, wantCount: 0,
		},
		{
			name: "not in window", stock: 5, want: FlashSaleNotInWindow,
			wantStock: 5, wantCount: 0,
			mutate: func(p *FlashSalePreDeductParams) {
				p.StartAt = now.Add(time.Minute)
				p.EndAt = now.Add(time.Hour)
			},
		},
		{
			name: "limit reached", stock: 5, count: 2, want: FlashSaleLimitReached,
			wantStock: 5, wantCount: 2,
		},
		{
			name: "offline", stock: 5, want: FlashSaleOffline,
			wantStock: 5, wantCount: 0,
			mutate: func(p *FlashSalePreDeductParams) { p.OnSale = false },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newAtomicRedis(t)
			stockKey := atomicTestKey(t, "flashsale-stock")
			countKey := atomicTestKey(t, "flashsale-count")
			t.Cleanup(func() {
				require.NoError(t, c.Del(context.Background(), stockKey))
				require.NoError(t, c.Del(context.Background(), countKey))
			})
			require.NoError(t, c.Set(context.Background(), stockKey, fmt.Sprint(tc.stock), 0))
			if tc.count > 0 {
				require.NoError(t, c.Set(context.Background(), countKey, fmt.Sprint(tc.count), 0))
			}

			params := FlashSalePreDeductParams{
				StockKey: stockKey, CountKey: countKey, Now: now,
				StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Hour),
				OnSale: true, PerUserLimit: 2,
			}
			if tc.mutate != nil {
				tc.mutate(&params)
			}
			result, err := c.PreDeductFlashSale(context.Background(), params)
			require.NoError(t, err)
			require.Equal(t, tc.want, result)
			requireCacheState(t, c, stockKey, fmt.Sprint(tc.wantStock), true)
			if tc.wantCount == 0 {
				requireCacheState(t, c, countKey, "", false)
			} else {
				requireCacheState(t, c, countKey, fmt.Sprint(tc.wantCount), true)
			}
		})
	}
}

func TestPreDeductFlashSaleIsAtomicUnderContention(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-concurrent-stock")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), stockKey)) })
	require.NoError(t, c.Set(context.Background(), stockKey, "5", 0))

	now := time.Now()
	results := make([]FlashSalePreDeductResult, 20)
	errs := make([]error, 20)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			countKey := atomicTestKey(t, fmt.Sprintf("flashsale-concurrent-count-%d", i))
			t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), countKey)) })
			results[i], errs[i] = c.PreDeductFlashSale(context.Background(), FlashSalePreDeductParams{
				StockKey: stockKey, CountKey: countKey, Now: now,
				StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Hour),
				OnSale: true, PerUserLimit: 1,
			})
		}(i)
	}
	wg.Wait()

	deducted := 0
	for i, err := range errs {
		require.NoError(t, err)
		if results[i] == FlashSalePreDeducted {
			deducted++
		} else {
			require.Equal(t, FlashSaleSoldOut, results[i])
		}
	}
	require.Equal(t, 5, deducted)
	requireCacheState(t, c, stockKey, "0", true)
}

func TestPreDeductFlashSaleReservationIsIdempotent(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-reserved-stock")
	countKey := atomicTestKey(t, "flashsale-reserved-count")
	reservationKey := atomicTestKey(t, "flashsale-reservation")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), reservationKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "5", 0))

	now := time.Now()
	params := FlashSalePreDeductParams{
		StockKey: stockKey, CountKey: countKey,
		ReservationKey: reservationKey, ReservationToken: "reservation-42",
		Now: now, StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Hour),
		OnSale: true, PerUserLimit: 1,
	}
	result, err := c.PreDeductFlashSaleDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSalePreDeducted, result)

	result, err = c.PreDeductFlashSaleDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleAlreadyPreDeducted, result)
	requireCacheState(t, c, stockKey, "4", true)
	requireCacheState(t, c, countKey, "1", true)
	requireCacheState(t, c, reservationKey, "reservation-42", true)
	ttl, err := c.(*redisCache).client.TTL(context.Background(), reservationKey).Result()
	require.NoError(t, err)
	require.Equal(t, time.Duration(-1), ttl, "reservation remains until compensation is no longer possible")
}

func TestPreDeductFlashSalePropagatesRedisError(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-deduct-error-stock")
	countKey := atomicTestKey(t, "flashsale-deduct-error-count")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "5", 0))

	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.PreDeductFlashSale(ctx, FlashSalePreDeductParams{
		StockKey: stockKey, CountKey: countKey, Now: now,
		StartAt: now.Add(-time.Minute), EndAt: now.Add(time.Hour),
		OnSale: true, PerUserLimit: 1,
	})
	require.ErrorIs(t, err, context.Canceled)
	requireCacheState(t, c, stockKey, "5", true)
	requireCacheState(t, c, countKey, "", false)
}

func TestEnsureFlashSaleReservationReinstatesLostPreDeduction(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-reinstate-stock")
	countKey := atomicTestKey(t, "flashsale-reinstate-count")
	idemKey := atomicTestKey(t, "flashsale-reinstate-idem")
	reservationKey := atomicTestKey(t, "flashsale-reinstate-reservation")
	t.Cleanup(func() {
		for _, key := range []string{stockKey, countKey, idemKey, reservationKey} {
			require.NoError(t, c.Del(context.Background(), key))
		}
	})
	params := FlashSaleEnsureReservationParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
		ReservationKey: reservationKey, ReservationToken: "reservation-42",
		IdempotencyTTL: 30 * time.Minute, Quantity: 1, FallbackStock: 5, StockTTL: time.Hour,
	}

	result, err := c.EnsureFlashSaleReservationDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleReservationReinstated, result)
	result, err = c.EnsureFlashSaleReservationDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleReservationPresent, result)
	requireCacheState(t, c, stockKey, "4", true)
	requireCacheState(t, c, countKey, "1", true)
	requireCacheState(t, c, idemKey, "reservation-42", true)
	requireCacheState(t, c, reservationKey, "reservation-42", true)
}

func TestEnsureOrderedFlashSaleReservationDurablyRebuildsState(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-ordered-stock")
	countKey := atomicTestKey(t, "flashsale-ordered-count")
	idemKey := atomicTestKey(t, "flashsale-ordered-idem")
	reservationKey := atomicTestKey(t, "flashsale-ordered-reservation")
	t.Cleanup(func() {
		for _, key := range []string{stockKey, countKey, idemKey, reservationKey} {
			require.NoError(t, c.Del(context.Background(), key))
		}
	})
	params := FlashSaleEnsureOrderedReservationParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
		ReservationKey: reservationKey, ReservationToken: "ordered-42",
		IdempotencyTTL: 30 * time.Minute, Quantity: 1, FallbackStock: 9, StockTTL: time.Hour,
	}

	result, err := c.EnsureOrderedFlashSaleReservationDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleReservationReinstated, result)
	requireCacheState(t, c, stockKey, "9", true)
	requireCacheState(t, c, countKey, "1", true)
	requireCacheState(t, c, idemKey, "ordered-42", true)
	requireCacheState(t, c, reservationKey, "ordered-42", true)
}

func TestAdoptLegacyFlashSaleReservationDoesNotDeductAgain(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-legacy-stock")
	countKey := atomicTestKey(t, "flashsale-legacy-count")
	idemKey := atomicTestKey(t, "flashsale-legacy-idem")
	reservationKey := atomicTestKey(t, "flashsale-legacy-reservation")
	t.Cleanup(func() {
		for _, key := range []string{stockKey, countKey, idemKey, reservationKey} {
			require.NoError(t, c.Del(context.Background(), key))
		}
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "9", time.Hour))
	require.NoError(t, c.Set(context.Background(), countKey, "1", 0))
	params := FlashSaleAdoptLegacyReservationParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
		ReservationKey: reservationKey, ReservationToken: "1", IdempotencyTTL: 30 * time.Minute,
		TargetStock: 9, TargetUserCount: 1, StockTTL: time.Hour,
	}

	result, err := c.AdoptLegacyFlashSaleReservationDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleReservationReinstated, result)
	result, err = c.AdoptLegacyFlashSaleReservationDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleReservationPresent, result)
	requireCacheState(t, c, stockKey, "9", true)
	requireCacheState(t, c, countKey, "1", true)
}

func TestAdoptLegacyFlashSaleReservationRebuildsExpiredKeys(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-legacy-expired-stock")
	countKey := atomicTestKey(t, "flashsale-legacy-expired-count")
	idemKey := atomicTestKey(t, "flashsale-legacy-expired-idem")
	reservationKey := atomicTestKey(t, "flashsale-legacy-expired-reservation")
	t.Cleanup(func() {
		for _, key := range []string{stockKey, countKey, idemKey, reservationKey} {
			require.NoError(t, c.Del(context.Background(), key))
		}
	})
	params := FlashSaleAdoptLegacyReservationParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
		ReservationKey: reservationKey, ReservationToken: "1", IdempotencyTTL: 30 * time.Minute,
		TargetStock: 9, TargetUserCount: 1, StockTTL: time.Hour,
	}

	result, err := c.AdoptLegacyFlashSaleReservationDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleReservationReinstated, result)
	requireCacheState(t, c, stockKey, "9", true)
	requireCacheState(t, c, countKey, "1", true)
}

func TestAdoptMultipleLegacyReservationsConvergesFromFacts(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-legacy-multi-stock")
	countKey := atomicTestKey(t, "flashsale-legacy-multi-count")
	idemKey := atomicTestKey(t, "flashsale-legacy-multi-idem")
	reservation1 := atomicTestKey(t, "flashsale-legacy-multi-r1")
	reservation2 := atomicTestKey(t, "flashsale-legacy-multi-r2")
	t.Cleanup(func() {
		for _, key := range []string{stockKey, countKey, idemKey, reservation1, reservation2} {
			require.NoError(t, c.Del(context.Background(), key))
		}
	})
	base := FlashSaleAdoptLegacyReservationParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
		ReservationToken: "1", IdempotencyTTL: 30 * time.Minute, StockTTL: time.Hour,
	}
	first := base
	first.ReservationKey, first.TargetStock, first.TargetUserCount = reservation1, 9, 1
	second := base
	second.ReservationKey, second.TargetStock, second.TargetUserCount = reservation2, 8, 2
	_, err := c.AdoptLegacyFlashSaleReservationDurably(context.Background(), first, 2*time.Second)
	require.NoError(t, err)
	_, err = c.AdoptLegacyFlashSaleReservationDurably(context.Background(), second, 2*time.Second)
	require.NoError(t, err)
	requireCacheState(t, c, stockKey, "8", true)
	requireCacheState(t, c, countKey, "2", true)

	for _, reservationKey := range []string{reservation1, reservation2} {
		_, err = c.RestoreFlashSaleDurably(context.Background(), FlashSaleRestoreParams{
			StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
			ReservationKey: reservationKey, ReservationToken: "1", Quantity: 1,
		}, 2*time.Second)
		require.NoError(t, err)
	}
	requireCacheState(t, c, stockKey, "10", true)
	requireCacheState(t, c, countKey, "0", true)
}

func TestRestoreFlashSaleState(t *testing.T) {
	tests := []struct {
		name        string
		seedStock   bool
		seedCount   bool
		wantStock   string
		wantCount   string
		stockExists bool
		countExists bool
	}{
		{name: "all state", seedStock: true, seedCount: true, wantStock: "10", wantCount: "0", stockExists: true, countExists: true},
		{name: "missing stock", seedCount: true, wantCount: "0", countExists: true},
		{name: "missing count", seedStock: true, wantStock: "10", stockExists: true},
		{name: "missing stock and count"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newAtomicRedis(t)
			stockKey := atomicTestKey(t, "flashsale-restore-stock")
			countKey := atomicTestKey(t, "flashsale-restore-count")
			idemKey := atomicTestKey(t, "flashsale-restore-idem")
			t.Cleanup(func() {
				require.NoError(t, c.Del(context.Background(), stockKey))
				require.NoError(t, c.Del(context.Background(), countKey))
				require.NoError(t, c.Del(context.Background(), idemKey))
			})
			if tc.seedStock {
				require.NoError(t, c.Set(context.Background(), stockKey, "8", 0))
			}
			if tc.seedCount {
				require.NoError(t, c.Set(context.Background(), countKey, "2", 0))
			}
			require.NoError(t, c.Set(context.Background(), idemKey, "1", time.Minute))

			result, err := c.RestoreFlashSale(context.Background(), FlashSaleRestoreParams{
				StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey, Quantity: 2,
			})
			require.NoError(t, err)
			require.Equal(t, FlashSaleRestored, result)
			requireCacheState(t, c, stockKey, tc.wantStock, tc.stockExists)
			requireCacheState(t, c, countKey, tc.wantCount, tc.countExists)
			requireCacheState(t, c, idemKey, "", false)
		})
	}
}

func TestRestoreFlashSaleReservationIsIdempotentAndTokenScoped(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-idempotent-restore-stock")
	countKey := atomicTestKey(t, "flashsale-idempotent-restore-count")
	idemKey := atomicTestKey(t, "flashsale-idempotent-restore-idem")
	reservationKey := atomicTestKey(t, "flashsale-idempotent-restore-reservation")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), idemKey))
		require.NoError(t, c.Del(context.Background(), reservationKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "8", 0))
	require.NoError(t, c.Set(context.Background(), countKey, "2", 0))
	require.NoError(t, c.Set(context.Background(), idemKey, "newer-reservation", time.Minute))
	require.NoError(t, c.Set(context.Background(), reservationKey, "reservation-42", time.Minute))

	params := FlashSaleRestoreParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
		ReservationKey: reservationKey, ReservationToken: "reservation-42", Quantity: 1,
	}
	result, err := c.RestoreFlashSaleDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleRestored, result)
	result, err = c.RestoreFlashSaleDurably(context.Background(), params, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, FlashSaleAlreadyRestored, result)

	requireCacheState(t, c, stockKey, "9", true)
	requireCacheState(t, c, countKey, "1", true)
	requireCacheState(t, c, reservationKey, "rolled_back:reservation-42", true)
	requireCacheState(t, c, idemKey, "newer-reservation", true,
		"delayed compensation must not delete a newer request's idempotency key")
}

func TestRestoreFlashSalePropagatesRedisError(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-restore-error-stock")
	countKey := atomicTestKey(t, "flashsale-restore-error-count")
	idemKey := atomicTestKey(t, "flashsale-restore-error-idem")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), idemKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "8", 0))
	require.NoError(t, c.Set(context.Background(), countKey, "2", 0))
	require.NoError(t, c.Set(context.Background(), idemKey, "1", time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.RestoreFlashSale(ctx, FlashSaleRestoreParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey, Quantity: 2,
	})
	require.ErrorIs(t, err, context.Canceled)
	requireCacheState(t, c, stockKey, "8", true)
	requireCacheState(t, c, countKey, "2", true)
	requireCacheState(t, c, idemKey, "1", true)
}

func TestRestoreFlashSaleRejectsInvalidStateBeforeMutation(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-restore-invalid-stock")
	countKey := atomicTestKey(t, "flashsale-restore-invalid-count")
	idemKey := atomicTestKey(t, "flashsale-restore-invalid-idem")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), idemKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "8", 0))
	require.NoError(t, c.(*redisCache).client.HSet(context.Background(), countKey, "invalid", "state").Err())
	require.NoError(t, c.Set(context.Background(), idemKey, "1", time.Minute))

	_, err := c.RestoreFlashSale(context.Background(), FlashSaleRestoreParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey, Quantity: 2,
	})
	require.ErrorContains(t, err, "WRONGTYPE")
	requireCacheState(t, c, stockKey, "8", true)
	requireCacheState(t, c, idemKey, "1", true)
	typ, typeErr := c.(*redisCache).client.Type(context.Background(), countKey).Result()
	require.NoError(t, typeErr)
	require.Equal(t, "hash", typ)
}

func TestRestoreFlashSaleRejectsNonIntegerBeforeMutation(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-restore-decimal-stock")
	countKey := atomicTestKey(t, "flashsale-restore-decimal-count")
	idemKey := atomicTestKey(t, "flashsale-restore-decimal-idem")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), idemKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "8", 0))
	require.NoError(t, c.Set(context.Background(), countKey, "1.5", 0))
	require.NoError(t, c.Set(context.Background(), idemKey, "1", time.Minute))

	_, err := c.RestoreFlashSale(context.Background(), FlashSaleRestoreParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey, Quantity: 2,
	})
	require.ErrorContains(t, err, "safe integer")
	requireCacheState(t, c, stockKey, "8", true)
	requireCacheState(t, c, countKey, "1.5", true)
	requireCacheState(t, c, idemKey, "1", true)
}

func TestRestoreFlashSaleRejectsOverflowBeforeMutation(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-restore-overflow-stock")
	countKey := atomicTestKey(t, "flashsale-restore-overflow-count")
	idemKey := atomicTestKey(t, "flashsale-restore-overflow-idem")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), idemKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "9223372036854775807", 0))
	require.NoError(t, c.Set(context.Background(), countKey, "2", 0))
	require.NoError(t, c.Set(context.Background(), idemKey, "1", time.Minute))

	_, err := c.RestoreFlashSale(context.Background(), FlashSaleRestoreParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey, Quantity: 2,
	})
	require.ErrorContains(t, err, "safe integer")
	requireCacheState(t, c, stockKey, "9223372036854775807", true)
	requireCacheState(t, c, countKey, "2", true)
	requireCacheState(t, c, idemKey, "1", true)
}

func TestRestoreFlashSaleRejectsInvalidQuantityBeforeMutation(t *testing.T) {
	c := newAtomicRedis(t)
	stockKey := atomicTestKey(t, "flashsale-restore-quantity-stock")
	countKey := atomicTestKey(t, "flashsale-restore-quantity-count")
	idemKey := atomicTestKey(t, "flashsale-restore-quantity-idem")
	t.Cleanup(func() {
		require.NoError(t, c.Del(context.Background(), stockKey))
		require.NoError(t, c.Del(context.Background(), countKey))
		require.NoError(t, c.Del(context.Background(), idemKey))
	})
	require.NoError(t, c.Set(context.Background(), stockKey, "8", 0))
	require.NoError(t, c.Set(context.Background(), countKey, "2", 0))
	require.NoError(t, c.Set(context.Background(), idemKey, "1", time.Minute))

	_, err := c.RestoreFlashSale(context.Background(), FlashSaleRestoreParams{
		StockKey: stockKey, CountKey: countKey, IdempotencyKey: idemKey,
	})
	require.ErrorContains(t, err, "must be positive")
	requireCacheState(t, c, stockKey, "8", true)
	requireCacheState(t, c, countKey, "2", true)
	requireCacheState(t, c, idemKey, "1", true)
}

func TestIncrementFixedWindow(t *testing.T) {
	c := newAtomicRedis(t)
	key := atomicTestKey(t, "fixed-window")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })

	count, err := c.IncrementFixedWindow(context.Background(), key, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	firstTTL, err := c.(*redisCache).client.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	count, err = c.IncrementFixedWindow(context.Background(), key, time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	secondTTL, err := c.(*redisCache).client.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, secondTTL, firstTTL, "后续计数不得重置固定窗口")
}

func TestIncrementFixedWindowPropagatesRedisError(t *testing.T) {
	c := newAtomicRedis(t)
	key := atomicTestKey(t, "fixed-window-error")
	t.Cleanup(func() { require.NoError(t, c.Del(context.Background(), key)) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.IncrementFixedWindow(ctx, key, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	requireCacheState(t, c, key, "", false)
}

func requireCacheState(t *testing.T, c Cache, key, want string, exists bool, msgAndArgs ...any) {
	t.Helper()
	got, err := c.Get(context.Background(), key)
	if !exists {
		require.ErrorIs(t, err, ErrMiss, msgAndArgs...)
		return
	}
	require.NoError(t, err, msgAndArgs...)
	require.Equal(t, want, got, msgAndArgs...)
}
