package cache

import (
	"context"
	"fmt"
	"time"
)

// IdempotencyResult reports whether an idempotency reservation was created.
type IdempotencyResult uint8

const (
	IdempotencyAcquired IdempotencyResult = iota + 1
	IdempotencyExists
)

// IdempotencyStore atomically reserves idempotency keys without exposing Lua
// or Redis return codes to business modules.
type IdempotencyStore interface {
	AcquireIdempotency(ctx context.Context, key, value string, ttl time.Duration) (IdempotencyResult, error)
}

type DurableIdempotencyStore interface {
	ReleaseIdempotencyDurably(ctx context.Context, key, value string, timeout time.Duration) error
}

// CouponClaimResult is the complete outcome set of an atomic coupon claim.
type CouponClaimResult uint8

const (
	CouponClaimed CouponClaimResult = iota + 1
	CouponSoldOut
	CouponNotInWindow
	CouponLimitReached
)

type CouponClaimParams struct {
	ClaimedKey   string
	PerUserKey   string
	Now          time.Time
	Total        int
	ValidFrom    time.Time
	ValidUntil   time.Time
	PerUserLimit int
}

// CouponStore atomically enforces a coupon template's total and per-user limit.
type CouponStore interface {
	ClaimCoupon(ctx context.Context, p CouponClaimParams) (CouponClaimResult, error)
}

// FlashSaleWarmResult reports whether live Redis stock was updated or kept.
type FlashSaleWarmResult uint8

const (
	FlashSaleStockUpdated FlashSaleWarmResult = iota + 1
	FlashSaleStockRetained
)

type FlashSaleWarmParams struct {
	StockKey string
	Stock    int
	TTL      time.Duration
}

// FlashSalePreDeductResult is the complete outcome set of an atomic pre-deduct.
type FlashSalePreDeductResult uint8

const (
	FlashSalePreDeducted FlashSalePreDeductResult = iota + 1
	FlashSaleAlreadyPreDeducted
	FlashSaleSoldOut
	FlashSaleNotInWindow
	FlashSaleLimitReached
	FlashSaleOffline
)

type FlashSalePreDeductParams struct {
	StockKey         string
	CountKey         string
	ReservationKey   string
	ReservationToken string
	ReservationTTL   time.Duration
	IdempotencyKey   string
	IdempotencyTTL   time.Duration
	Now              time.Time
	StartAt          time.Time
	EndAt            time.Time
	OnSale           bool
	PerUserLimit     int
}

type FlashSaleRestoreParams struct {
	StockKey                 string
	CountKey                 string
	IdempotencyKey           string
	ReservationKey           string
	ReservationToken         string
	AllowIdempotencyFallback bool
	AllowMissingReservation  bool
	Quantity                 int
	FallbackStock            int
	StockTTL                 time.Duration
}

type FlashSaleRestoreResult uint8

const (
	FlashSaleRestored FlashSaleRestoreResult = iota + 1
	FlashSaleAlreadyRestored
	FlashSaleReservationMissing
)

type FlashSaleEnsureReservationResult uint8

const (
	FlashSaleReservationPresent FlashSaleEnsureReservationResult = iota + 1
	FlashSaleReservationReinstated
)

type FlashSaleEnsureReservationParams struct {
	StockKey         string
	CountKey         string
	IdempotencyKey   string
	ReservationKey   string
	ReservationToken string
	IdempotencyTTL   time.Duration
	Quantity         int
	FallbackStock    int
	StockTTL         time.Duration
}

type FlashSaleEnsureOrderedReservationParams struct {
	StockKey         string
	CountKey         string
	IdempotencyKey   string
	ReservationKey   string
	ReservationToken string
	IdempotencyTTL   time.Duration
	Quantity         int
	FallbackStock    int
	StockTTL         time.Duration
}

type FlashSaleAdoptLegacyReservationParams struct {
	StockKey         string
	CountKey         string
	IdempotencyKey   string
	ReservationKey   string
	ReservationToken string
	IdempotencyTTL   time.Duration
	TargetStock      int
	TargetUserCount  int
	StockTTL         time.Duration
}

// FlashSaleStore owns the scripts that mutate flash-sale Redis state.
type FlashSaleStore interface {
	WarmFlashSaleStock(ctx context.Context, p FlashSaleWarmParams) (FlashSaleWarmResult, error)
	PreDeductFlashSale(ctx context.Context, p FlashSalePreDeductParams) (FlashSalePreDeductResult, error)
	PreDeductFlashSaleDurably(ctx context.Context, p FlashSalePreDeductParams, timeout time.Duration) (FlashSalePreDeductResult, error)
	EnsureFlashSaleReservation(ctx context.Context, p FlashSaleEnsureReservationParams) (FlashSaleEnsureReservationResult, error)
	EnsureFlashSaleReservationDurably(ctx context.Context, p FlashSaleEnsureReservationParams, timeout time.Duration) (FlashSaleEnsureReservationResult, error)
	EnsureOrderedFlashSaleReservation(ctx context.Context, p FlashSaleEnsureOrderedReservationParams) (FlashSaleEnsureReservationResult, error)
	EnsureOrderedFlashSaleReservationDurably(ctx context.Context, p FlashSaleEnsureOrderedReservationParams, timeout time.Duration) (FlashSaleEnsureReservationResult, error)
	AdoptLegacyFlashSaleReservationDurably(ctx context.Context, p FlashSaleAdoptLegacyReservationParams, timeout time.Duration) (FlashSaleEnsureReservationResult, error)
	RestoreFlashSale(ctx context.Context, p FlashSaleRestoreParams) (FlashSaleRestoreResult, error)
	RestoreFlashSaleDurably(ctx context.Context, p FlashSaleRestoreParams, timeout time.Duration) (FlashSaleRestoreResult, error)
}

// FixedWindowStore atomically increments a counter whose TTL is set only on
// the first increment, preserving a fixed rather than sliding window.
type FixedWindowStore interface {
	IncrementFixedWindow(ctx context.Context, key string, window time.Duration) (int64, error)
}

// Client is the complete Redis adapter capability surface used at wiring
// boundaries. Business modules should depend on narrower composed interfaces.
type Client interface {
	Cache
	IdempotencyStore
	DurableIdempotencyStore
	CouponStore
	FlashSaleStore
	FixedWindowStore
}

const acquireIdempotencyScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
    return 1
end
return 0
`

const releaseIdempotencyScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
    redis.call('DEL', KEYS[1])
    return 1
end
return 0
`

const claimCouponScript = `
if ARGV[1] < ARGV[3] or ARGV[1] > ARGV[4] then
    return -1
end
local claimed = tonumber(redis.call('GET', KEYS[1]) or '0')
if claimed >= tonumber(ARGV[2]) then
    return 0
end
local per_user = tonumber(redis.call('GET', KEYS[2]) or '0')
if per_user >= tonumber(ARGV[5]) then
    return -2
end
redis.call('INCR', KEYS[1])
redis.call('INCR', KEYS[2])
return 1
`

const warmFlashSaleStockScript = `
local cur = tonumber(redis.call('GET', KEYS[1]) or '-1')
if cur < 0 or tonumber(ARGV[1]) < cur then
    redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return 1
end
return 0
`

const preDeductFlashSaleScript = `
if KEYS[3] ~= nil and KEYS[3] ~= '' then
    local reservation = redis.call('GET', KEYS[3])
    if reservation == ARGV[6] then
        local stock_value = redis.call('GET', KEYS[1])
        local count_value = redis.call('GET', KEYS[2])
        if stock_value == false or count_value == false then
            return redis.error_reply('flash-sale reserved state is incomplete')
        end
        local stock_ttl = redis.call('PTTL', KEYS[1])
        if stock_ttl > 0 then
            redis.call('SET', KEYS[1], stock_value, 'PX', stock_ttl)
        else
            redis.call('SET', KEYS[1], stock_value)
        end
        redis.call('SET', KEYS[2], count_value)
        redis.call('SET', KEYS[3], ARGV[6])
        if KEYS[4] ~= nil and KEYS[4] ~= '' then
            redis.call('SET', KEYS[4], ARGV[6], 'EX', ARGV[8])
        end
        return 2
    end
    if reservation ~= false then
        return redis.error_reply('flash-sale reservation token mismatch')
    end
end
if ARGV[4] ~= '1' then
    return -3
end
if ARGV[1] < ARGV[2] or ARGV[1] > ARGV[3] then
    return -1
end
local stock = tonumber(redis.call('GET', KEYS[1]) or '0')
if stock <= 0 then
    return 0
end
local per_user = tonumber(redis.call('GET', KEYS[2]) or '0')
if per_user >= tonumber(ARGV[5]) then
    return -2
end
redis.call('DECR', KEYS[1])
redis.call('INCR', KEYS[2])
if KEYS[3] ~= nil and KEYS[3] ~= '' then
    if tonumber(ARGV[7]) > 0 then
        redis.call('SET', KEYS[3], ARGV[6], 'EX', ARGV[7])
    else
        redis.call('SET', KEYS[3], ARGV[6])
    end
end
if KEYS[4] ~= nil and KEYS[4] ~= '' then
    redis.call('SET', KEYS[4], ARGV[6], 'EX', ARGV[8])
end
return 1
`

const restoreFlashSaleScript = `
local function rewrite_string(key)
    if redis.call('EXISTS', key) == 0 then
        return
    end
    local value = redis.call('GET', key)
    local ttl = redis.call('PTTL', key)
    if ttl > 0 then
        redis.call('SET', key, value, 'PX', ttl)
    else
        redis.call('SET', key, value)
    end
end
local function parse_safe_integer(value)
    if value ~= '0' and value ~= '-0' and
       string.match(value, '^[1-9]%d*$') == nil and
       string.match(value, '^%-[1-9]%d*$') == nil then
        return nil
    end
    local number = tonumber(value)
    if number == nil or number > 9007199254740991 or number < -9007199254740991 then
        return nil
    end
    return number
end
local quantity = parse_safe_integer(ARGV[1])
if quantity == nil or quantity <= 0 then
    return redis.error_reply('flash-sale restore quantity is not a positive safe integer')
end
local scoped = KEYS[4] ~= nil and KEYS[4] ~= ''
local should_restore = not scoped
if scoped then
    local reservation = redis.call('GET', KEYS[4])
    if reservation == 'rolled_back:' .. ARGV[2] then
        rewrite_string(KEYS[1])
        rewrite_string(KEYS[2])
        redis.call('SET', KEYS[4], reservation)
        return 2
    end
    if reservation ~= false and reservation ~= ARGV[2] then
        return redis.error_reply('flash-sale reservation token mismatch')
    end
    should_restore = reservation == ARGV[2]
    if not should_restore and ARGV[3] == '1' then
        should_restore = redis.call('GET', KEYS[3]) == ARGV[2]
    end
    if not should_restore then
        if redis.call('GET', KEYS[3]) == ARGV[2] then
            redis.call('DEL', KEYS[3])
        end
        if reservation == false and ARGV[4] == '1' then
            if redis.call('EXISTS', KEYS[1]) == 0 then
                redis.call('SET', KEYS[1], ARGV[5], 'EX', ARGV[6])
            end
            redis.call('SET', KEYS[4], 'rolled_back:' .. ARGV[2])
            return 1
        end
        return 0
    end
end
local stock_exists = redis.call('EXISTS', KEYS[1]) == 1
local count_exists = redis.call('EXISTS', KEYS[2]) == 1
if stock_exists and parse_safe_integer(redis.call('GET', KEYS[1])) == nil then
    return redis.error_reply('flash-sale stock is not a safe integer')
end
if count_exists and parse_safe_integer(redis.call('GET', KEYS[2])) == nil then
    return redis.error_reply('flash-sale count is not a safe integer')
end
if stock_exists then
    redis.call('INCRBY', KEYS[1], ARGV[1])
end
if count_exists then
    redis.call('DECRBY', KEYS[2], ARGV[1])
end
if redis.call('GET', KEYS[3]) == ARGV[2] or not scoped then
    redis.call('DEL', KEYS[3])
end
if scoped then
    redis.call('SET', KEYS[4], 'rolled_back:' .. ARGV[2])
end
return 1
`

const ensureFlashSaleReservationScript = `
local reservation = redis.call('GET', KEYS[4])
if reservation ~= false and reservation ~= ARGV[1] then
    return redis.error_reply('flash-sale reservation token mismatch')
end
local idem = redis.call('GET', KEYS[3])
if idem ~= false and idem ~= ARGV[1] then
    return redis.error_reply('flash-sale idempotency token mismatch')
end
local quantity = tonumber(ARGV[2])
local stock_raw = redis.call('GET', KEYS[1])
local stock
if stock_raw == false then
    stock = tonumber(ARGV[4])
    if stock == nil then
        return redis.error_reply('flash-sale fallback stock is invalid during reservation recovery')
    end
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
else
    stock = tonumber(stock_raw)
    if stock == nil then
        return redis.error_reply('flash-sale stock is invalid during reservation recovery')
    end
end
local count = tonumber(redis.call('GET', KEYS[2]) or '0')
if count == nil then
    return redis.error_reply('flash-sale count is invalid during reservation recovery')
end
if reservation == ARGV[1] then
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
    redis.call('SET', KEYS[2], count)
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
    redis.call('SET', KEYS[4], ARGV[1])
    return 1
end
if stock < quantity then
    return redis.error_reply('flash-sale stock is insufficient during reservation recovery')
end
redis.call('DECRBY', KEYS[1], quantity)
redis.call('INCRBY', KEYS[2], quantity)
redis.call('SET', KEYS[4], ARGV[1])
if idem == false then
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
end
return 2
`

const ensureOrderedFlashSaleReservationScript = `
local reservation = redis.call('GET', KEYS[4])
if reservation ~= false and reservation ~= ARGV[1] then
    return redis.error_reply('flash-sale ordered reservation token mismatch')
end
local idem = redis.call('GET', KEYS[3])
if idem ~= false and idem ~= ARGV[1] then
    return redis.error_reply('flash-sale ordered idempotency token mismatch')
end
local stock_raw = redis.call('GET', KEYS[1])
local stock = tonumber(stock_raw or '-1')
local fallback_stock = tonumber(ARGV[4])
if fallback_stock == nil or stock == nil then
    return redis.error_reply('flash-sale ordered stock is invalid during reservation recovery')
end
if stock_raw == false or fallback_stock < stock then
    redis.call('SET', KEYS[1], fallback_stock, 'EX', ARGV[5])
    stock = fallback_stock
end
local count = tonumber(redis.call('GET', KEYS[2]) or '0')
if count == nil then
    return redis.error_reply('flash-sale ordered count is invalid during reservation recovery')
end
if reservation == ARGV[1] then
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
    redis.call('SET', KEYS[2], count)
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
    redis.call('SET', KEYS[4], ARGV[1])
    return 1
end
redis.call('INCRBY', KEYS[2], ARGV[2])
redis.call('SET', KEYS[4], ARGV[1])
if idem == false then
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
end
return 2
`

const adoptLegacyFlashSaleReservationScript = `
local reservation = redis.call('GET', KEYS[4])
if reservation ~= false and reservation ~= ARGV[1] then
    return redis.error_reply('legacy flash-sale reservation token mismatch')
end
local idem = redis.call('GET', KEYS[3])
if idem ~= false and idem ~= ARGV[1] then
    return redis.error_reply('legacy flash-sale idempotency token mismatch')
end
local stock = redis.call('GET', KEYS[1])
local target_stock = tonumber(ARGV[3])
if target_stock == nil then
    return redis.error_reply('legacy flash-sale target stock is invalid')
end
if stock == false or tonumber(stock) == nil or target_stock < tonumber(stock) then
    stock = target_stock
end
if redis.call('EXISTS', KEYS[1]) == 1 then
    local stock_ttl = redis.call('PTTL', KEYS[1])
    if stock_ttl > 0 then
        redis.call('SET', KEYS[1], stock, 'PX', stock_ttl)
    else
        redis.call('SET', KEYS[1], stock)
    end
else
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
end
local count = redis.call('GET', KEYS[2])
local target_count = tonumber(ARGV[4])
if target_count == nil then
    return redis.error_reply('legacy flash-sale target count is invalid')
end
if count == false or tonumber(count) == nil or tonumber(count) < target_count then
    count = target_count
end
if tonumber(count) == nil then
    return redis.error_reply('legacy flash-sale count is invalid')
end
redis.call('SET', KEYS[2], count)
redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[2])
redis.call('SET', KEYS[4], ARGV[1])
if reservation == ARGV[1] then
    return 1
end
return 2
`

const incrementFixedWindowScript = `
if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], 1, 'EX', ARGV[1])
    return 1
end
return redis.call('INCR', KEYS[1])
`

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

func (r *redisCache) ClaimCoupon(ctx context.Context, p CouponClaimParams) (CouponClaimResult, error) {
	code, err := r.evalInt(ctx, claimCouponScript, []string{p.ClaimedKey, p.PerUserKey},
		p.Now.UnixMilli(), p.Total, p.ValidFrom.UnixMilli(), p.ValidUntil.UnixMilli(), p.PerUserLimit)
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
	keys := []string{p.StockKey, p.CountKey}
	if p.ReservationKey != "" {
		if p.ReservationToken == "" {
			return 0, fmt.Errorf("flash-sale reservation token is required")
		}
		if p.ReservationTTL < 0 {
			return 0, fmt.Errorf("flash-sale reservation TTL cannot be negative")
		}
		keys = append(keys, p.ReservationKey)
		if p.IdempotencyKey != "" {
			if p.IdempotencyTTL < time.Second {
				return 0, fmt.Errorf("flash-sale idempotency TTL must be at least one second")
			}
			keys = append(keys, p.IdempotencyKey)
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
