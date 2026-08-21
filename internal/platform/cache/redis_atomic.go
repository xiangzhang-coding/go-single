package cache

import (
	"context"
	"fmt"
	"time"
)

func (r *redisCache) AcquireIdempotency(ctx context.Context, key, value string, ttl time.Duration) (IdempotencyResult, error) {
	code, err := r.evalInt(ctx, acquireIdempotencyScript, []string{key}, value, int64(ttl.Seconds()))
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return IdempotencyAcquired, nil
	case 0:
		return IdempotencyExists, nil
	default:
		return 0, fmt.Errorf("unexpected idempotency result: %d", code)
	}
}

func (r *redisCache) ReleaseIdempotencyDurably(ctx context.Context, key, value string, timeout time.Duration) error {
	if key == "" || value == "" || timeout <= 0 {
		return fmt.Errorf("durable idempotency release requires key, value, and positive timeout")
	}
	code, err := r.evalIntAndWaitAOF(ctx, timeout, releaseIdempotencyScript, []string{key}, value)
	if err != nil {
		return err
	}
	if code != 1 {
		return fmt.Errorf("idempotency key no longer belongs to reservation")
	}
	return nil
}

func (r *redisCache) ProductDetailVersion(ctx context.Context, keys ProductDetailKeys) (int64, error) {
	if keys.Detail == "" || keys.Version == "" || keys.Mutation == "" {
		return 0, fmt.Errorf("product detail version requires key")
	}
	return r.evalInt(ctx, productDetailVersionScript, []string{keys.Version})
}

func (r *redisCache) SetProductDetailIfVersion(ctx context.Context, keys ProductDetailKeys, version int64, value string, ttl time.Duration) (bool, error) {
	if keys.Detail == "" || keys.Version == "" || keys.Mutation == "" || version < 0 || ttl <= 0 {
		return false, fmt.Errorf("product detail fill requires keys, non-negative version, and positive ttl")
	}
	code, err := r.evalInt(ctx, setProductDetailIfVersionScript, []string{keys.Detail, keys.Version, keys.Mutation}, version, value, ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	switch code {
	case 1:
		return true, nil
	case 0:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected product detail fill result: %d", code)
	}
}

func (r *redisCache) BeginProductDetailMutation(ctx context.Context, keys ProductDetailKeys, token string, ttl, aofTimeout time.Duration) error {
	if keys.Detail == "" || keys.Version == "" || keys.Mutation == "" || token == "" || ttl <= 0 || aofTimeout <= 0 {
		return fmt.Errorf("product detail mutation begin requires keys, token, positive ttl, and AOF timeout")
	}
	code, err := r.evalIntAndWaitAOF(ctx, aofTimeout, beginProductDetailMutationScript,
		[]string{keys.Detail, keys.Version, keys.Mutation}, token, ttl.Milliseconds())
	if err != nil {
		return err
	}
	if code != 1 {
		return fmt.Errorf("unexpected product detail mutation begin result: %d", code)
	}
	return nil
}

func (r *redisCache) FinishProductDetailMutation(ctx context.Context, keys ProductDetailKeys, token string, aofTimeout time.Duration) error {
	if keys.Detail == "" || keys.Version == "" || keys.Mutation == "" || token == "" || aofTimeout <= 0 {
		return fmt.Errorf("product detail mutation finish requires keys, token, and positive AOF timeout")
	}
	code, err := r.evalIntAndWaitAOF(ctx, aofTimeout, finishProductDetailMutationScript,
		[]string{keys.Detail, keys.Version, keys.Mutation}, token)
	if err != nil {
		return err
	}
	if code != 1 {
		return fmt.Errorf("unexpected product detail mutation finish result: %d", code)
	}
	return nil
}

func (r *redisCache) ClaimCoupon(ctx context.Context, p CouponClaimParams) (CouponClaimResult, error) {
	if p.ClaimedKey == "" || p.PerUserKey == "" || p.VersionKey == "" || p.PerUserVersionKey == "" || p.ClaimedCount < 0 || p.PerUserCount < 0 {
		return 0, fmt.Errorf("coupon claim requires keys and non-negative database counts")
	}
	code, err := r.evalInt(ctx, claimCouponScript, []string{p.ClaimedKey, p.PerUserKey, p.VersionKey, p.PerUserVersionKey},
		p.Now.UnixMilli(), p.Total, p.ValidFrom.UnixMilli(), p.ValidUntil.UnixMilli(), p.PerUserLimit,
		p.ClaimedCount, p.PerUserCount)
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return CouponClaimed, nil
	case 0:
		return CouponSoldOut, nil
	case -1:
		return CouponNotInWindow, nil
	case -2:
		return CouponLimitReached, nil
	default:
		return 0, fmt.Errorf("unexpected coupon claim result: %d", code)
	}
}

func (r *redisCache) SyncCouponCounts(ctx context.Context, p CouponCountParams) error {
	if p.ClaimedKey == "" || p.PerUserKey == "" || p.VersionKey == "" || p.PerUserVersionKey == "" || p.ClaimedCount < 0 || p.PerUserCount < 0 {
		return fmt.Errorf("coupon count sync requires keys and non-negative counts")
	}
	code, err := r.evalInt(ctx, syncCouponCountsScript, []string{p.ClaimedKey, p.PerUserKey, p.VersionKey, p.PerUserVersionKey}, p.ClaimedCount, p.PerUserCount)
	if err != nil {
		return err
	}
	if code != 0 && code != 1 {
		return fmt.Errorf("unexpected coupon count sync result: %d", code)
	}
	return nil
}

func (r *redisCache) WarmFlashSaleStock(ctx context.Context, p FlashSaleWarmParams) (FlashSaleWarmResult, error) {
	code, err := r.evalInt(ctx, warmFlashSaleStockScript, []string{p.StockKey}, p.Stock, int64(p.TTL.Seconds()))
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSaleStockUpdated, nil
	case 0:
		return FlashSaleStockRetained, nil
	default:
		return 0, fmt.Errorf("unexpected flash-sale warm result: %d", code)
	}
}

func (r *redisCache) DecreaseFlashSaleStockDurably(ctx context.Context, p FlashSaleDecreaseParams, timeout time.Duration) error {
	if p.StockKey == "" || p.Delta <= 0 || timeout <= 0 {
		return fmt.Errorf("durable flash-sale stock decrease requires key, positive delta, and timeout")
	}
	code, err := r.evalIntAndWaitAOF(ctx, timeout, decreaseFlashSaleStockScript, []string{p.StockKey}, p.Delta)
	if err != nil {
		return err
	}
	if code != 1 {
		return fmt.Errorf("unexpected flash-sale stock decrease result: %d", code)
	}
	return nil
}

func (r *redisCache) PauseFlashSaleStockDurably(ctx context.Context, p FlashSalePauseParams, timeout time.Duration) (int, error) {
	if p.StockKey == "" || p.PauseKey == "" || p.Token == "" || p.TTL < time.Second || timeout <= 0 {
		return 0, fmt.Errorf("durable flash-sale pause requires keys, TTL, and timeout")
	}
	stock, err := r.evalIntAndWaitAOF(ctx, timeout, pauseFlashSaleStockScript,
		[]string{p.StockKey, p.PauseKey}, int64(p.TTL.Seconds()), p.Token)
	return int(stock), err
}

func (r *redisCache) ReleaseFlashSalePauseDurably(ctx context.Context, pauseKey, token string, timeout time.Duration) error {
	if pauseKey == "" || timeout <= 0 {
		return fmt.Errorf("durable flash-sale pause release requires key and timeout")
	}
	_, err := r.evalIntAndWaitAOF(ctx, timeout, releaseFlashSalePauseScript, []string{pauseKey}, token)
	return err
}

func (r *redisCache) HoldFlashSalePauseDurably(ctx context.Context, pauseKey string, timeout time.Duration) error {
	if pauseKey == "" || timeout <= 0 {
		return fmt.Errorf("durable fail-closed flash-sale pause requires key and timeout")
	}
	_, err := r.evalIntAndWaitAOF(ctx, timeout, holdFlashSalePauseScript, []string{pauseKey})
	return err
}

func (r *redisCache) PreDeductFlashSale(ctx context.Context, p FlashSalePreDeductParams) (FlashSalePreDeductResult, error) {
	return r.preDeductFlashSale(ctx, p, 0)
}

func (r *redisCache) PreDeductFlashSaleDurably(ctx context.Context, p FlashSalePreDeductParams, timeout time.Duration) (FlashSalePreDeductResult, error) {
	if timeout <= 0 {
		return 0, fmt.Errorf("flash-sale durable pre-deduction timeout must be positive")
	}
	return r.preDeductFlashSale(ctx, p, timeout)
}

func (r *redisCache) preDeductFlashSale(ctx context.Context, p FlashSalePreDeductParams, aofTimeout time.Duration) (FlashSalePreDeductResult, error) {
	keys := []string{p.StockKey, p.CountKey, "", "", p.PauseKey}
	if p.ReservationKey != "" {
		if p.ReservationToken == "" {
			return 0, fmt.Errorf("flash-sale reservation token is required")
		}
		if p.ReservationTTL < 0 {
			return 0, fmt.Errorf("flash-sale reservation TTL cannot be negative")
		}
		keys[2] = p.ReservationKey
		if p.IdempotencyKey != "" {
			if p.IdempotencyTTL < time.Second {
				return 0, fmt.Errorf("flash-sale idempotency TTL must be at least one second")
			}
			keys[3] = p.IdempotencyKey
		}
	}
	onSale := 0
	if p.OnSale {
		onSale = 1
	}
	code, err := r.evalFlashSaleInt(ctx, aofTimeout, preDeductFlashSaleScript, keys,
		p.Now.UnixMilli(), p.StartAt.UnixMilli(), p.EndAt.UnixMilli(), onSale, p.PerUserLimit,
		p.ReservationToken, int64(p.ReservationTTL.Seconds()), int64(p.IdempotencyTTL.Seconds()))
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSalePreDeducted, nil
	case 2:
		return FlashSaleAlreadyPreDeducted, nil
	case 0:
		return FlashSaleSoldOut, nil
	case -1:
		return FlashSaleNotInWindow, nil
	case -2:
		return FlashSaleLimitReached, nil
	case -3:
		return FlashSaleOffline, nil
	case -4:
		return FlashSalePaused, nil
	default:
		return 0, fmt.Errorf("unexpected flash-sale pre-deduct result: %d", code)
	}
}

func (r *redisCache) RestoreFlashSale(ctx context.Context, p FlashSaleRestoreParams) (FlashSaleRestoreResult, error) {
	return r.restoreFlashSale(ctx, p, 0)
}

func (r *redisCache) RestoreFlashSaleDurably(ctx context.Context, p FlashSaleRestoreParams, timeout time.Duration) (FlashSaleRestoreResult, error) {
	if timeout <= 0 {
		return 0, fmt.Errorf("flash-sale durable restore timeout must be positive")
	}
	return r.restoreFlashSale(ctx, p, timeout)
}

func (r *redisCache) restoreFlashSale(ctx context.Context, p FlashSaleRestoreParams, aofTimeout time.Duration) (FlashSaleRestoreResult, error) {
	if p.Quantity <= 0 {
		return 0, fmt.Errorf("flash-sale restore quantity must be positive: %d", p.Quantity)
	}
	keys := []string{p.StockKey, p.CountKey, p.IdempotencyKey}
	if p.ReservationKey != "" {
		if p.ReservationToken == "" {
			return 0, fmt.Errorf("flash-sale reservation token is required")
		}
		keys = append(keys, p.ReservationKey)
	}
	fallback := 0
	if p.AllowIdempotencyFallback {
		fallback = 1
	}
	allowMissing := 0
	if p.AllowMissingReservation {
		allowMissing = 1
	}
	stockTTLSeconds := int64(p.StockTTL.Seconds())
	if p.AllowMissingReservation && (p.FallbackStock < 0 || stockTTLSeconds <= 0) {
		return 0, fmt.Errorf("flash-sale missing reservation fallback stock/TTL is invalid")
	}
	code, err := r.evalFlashSaleInt(ctx, aofTimeout, restoreFlashSaleScript, keys,
		p.Quantity, p.ReservationToken, fallback, allowMissing, p.FallbackStock, stockTTLSeconds)
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSaleRestored, nil
	case 2:
		return FlashSaleAlreadyRestored, nil
	case 0:
		return FlashSaleReservationMissing, nil
	default:
		return 0, fmt.Errorf("unexpected flash-sale restore result: %d", code)
	}
}

func (r *redisCache) EnsureFlashSaleReservation(ctx context.Context, p FlashSaleEnsureReservationParams) (FlashSaleEnsureReservationResult, error) {
	return r.ensureFlashSaleReservation(ctx, p, 0)
}

func (r *redisCache) EnsureFlashSaleReservationDurably(ctx context.Context, p FlashSaleEnsureReservationParams, timeout time.Duration) (FlashSaleEnsureReservationResult, error) {
	if timeout <= 0 {
		return 0, fmt.Errorf("flash-sale durable reservation timeout must be positive")
	}
	return r.ensureFlashSaleReservation(ctx, p, timeout)
}

func (r *redisCache) ensureFlashSaleReservation(ctx context.Context, p FlashSaleEnsureReservationParams, aofTimeout time.Duration) (FlashSaleEnsureReservationResult, error) {
	if p.ReservationToken == "" {
		return 0, fmt.Errorf("flash-sale reservation token is required")
	}
	if p.Quantity <= 0 {
		return 0, fmt.Errorf("flash-sale reservation quantity must be positive: %d", p.Quantity)
	}
	if p.IdempotencyTTL <= 0 {
		return 0, fmt.Errorf("flash-sale idempotency TTL must be positive")
	}
	if p.FallbackStock < p.Quantity {
		return 0, fmt.Errorf("flash-sale fallback stock %d is below reservation quantity %d", p.FallbackStock, p.Quantity)
	}
	stockTTLSeconds := int64(p.StockTTL.Seconds())
	if stockTTLSeconds <= 0 {
		return 0, fmt.Errorf("flash-sale fallback stock TTL must be at least one second")
	}
	ttlSeconds := int64(p.IdempotencyTTL.Seconds())
	if ttlSeconds <= 0 {
		return 0, fmt.Errorf("flash-sale idempotency TTL must be at least one second")
	}
	code, err := r.evalFlashSaleInt(ctx, aofTimeout, ensureFlashSaleReservationScript,
		[]string{p.StockKey, p.CountKey, p.IdempotencyKey, p.ReservationKey},
		p.ReservationToken, p.Quantity, ttlSeconds, p.FallbackStock, stockTTLSeconds)
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSaleReservationPresent, nil
	case 2:
		return FlashSaleReservationReinstated, nil
	default:
		return 0, fmt.Errorf("unexpected flash-sale ensure reservation result: %d", code)
	}
}

func (r *redisCache) EnsureOrderedFlashSaleReservation(ctx context.Context, p FlashSaleEnsureOrderedReservationParams) (FlashSaleEnsureReservationResult, error) {
	return r.ensureOrderedFlashSaleReservation(ctx, p, 0)
}

func (r *redisCache) EnsureOrderedFlashSaleReservationDurably(ctx context.Context, p FlashSaleEnsureOrderedReservationParams, timeout time.Duration) (FlashSaleEnsureReservationResult, error) {
	if timeout <= 0 {
		return 0, fmt.Errorf("flash-sale durable ordered reservation timeout must be positive")
	}
	return r.ensureOrderedFlashSaleReservation(ctx, p, timeout)
}

func (r *redisCache) ensureOrderedFlashSaleReservation(ctx context.Context, p FlashSaleEnsureOrderedReservationParams, aofTimeout time.Duration) (FlashSaleEnsureReservationResult, error) {
	if p.ReservationToken == "" {
		return 0, fmt.Errorf("flash-sale ordered reservation token is required")
	}
	if p.Quantity <= 0 {
		return 0, fmt.Errorf("flash-sale ordered reservation quantity must be positive: %d", p.Quantity)
	}
	if p.FallbackStock < 0 {
		return 0, fmt.Errorf("flash-sale ordered fallback stock cannot be negative: %d", p.FallbackStock)
	}
	ttlSeconds := int64(p.IdempotencyTTL.Seconds())
	if ttlSeconds <= 0 {
		return 0, fmt.Errorf("flash-sale ordered idempotency TTL must be at least one second")
	}
	stockTTLSeconds := int64(p.StockTTL.Seconds())
	if stockTTLSeconds <= 0 {
		return 0, fmt.Errorf("flash-sale ordered stock TTL must be at least one second")
	}
	code, err := r.evalFlashSaleInt(ctx, aofTimeout, ensureOrderedFlashSaleReservationScript,
		[]string{p.StockKey, p.CountKey, p.IdempotencyKey, p.ReservationKey},
		p.ReservationToken, p.Quantity, ttlSeconds, p.FallbackStock, stockTTLSeconds)
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSaleReservationPresent, nil
	case 2:
		return FlashSaleReservationReinstated, nil
	default:
		return 0, fmt.Errorf("unexpected flash-sale ensure ordered reservation result: %d", code)
	}
}

func (r *redisCache) AdoptLegacyFlashSaleReservationDurably(ctx context.Context, p FlashSaleAdoptLegacyReservationParams, timeout time.Duration) (FlashSaleEnsureReservationResult, error) {
	if timeout <= 0 {
		return 0, fmt.Errorf("legacy flash-sale durable reservation timeout must be positive")
	}
	if p.ReservationToken == "" {
		return 0, fmt.Errorf("legacy flash-sale reservation token is required")
	}
	ttlSeconds := int64(p.IdempotencyTTL.Seconds())
	if ttlSeconds <= 0 {
		return 0, fmt.Errorf("legacy flash-sale idempotency TTL must be at least one second")
	}
	stockTTLSeconds := int64(p.StockTTL.Seconds())
	if p.TargetStock < 0 || p.TargetUserCount <= 0 || stockTTLSeconds <= 0 {
		return 0, fmt.Errorf("legacy flash-sale target stock, count, or TTL is invalid")
	}
	code, err := r.evalIntAndWaitAOF(ctx, timeout, adoptLegacyFlashSaleReservationScript,
		[]string{p.StockKey, p.CountKey, p.IdempotencyKey, p.ReservationKey},
		p.ReservationToken, ttlSeconds, p.TargetStock, p.TargetUserCount, stockTTLSeconds)
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSaleReservationPresent, nil
	case 2:
		return FlashSaleReservationReinstated, nil
	default:
		return 0, fmt.Errorf("unexpected legacy flash-sale reservation result: %d", code)
	}
}

func (r *redisCache) evalFlashSaleInt(ctx context.Context, aofTimeout time.Duration,
	script string, keys []string, args ...any) (int64, error) {
	if aofTimeout > 0 {
		return r.evalIntAndWaitAOF(ctx, aofTimeout, script, keys, args...)
	}
	return r.evalInt(ctx, script, keys, args...)
}

func (r *redisCache) IncrementFixedWindow(ctx context.Context, key string, window time.Duration) (int64, error) {
	return r.evalInt(ctx, incrementFixedWindowScript, []string{key}, int64(window.Seconds()))
}
