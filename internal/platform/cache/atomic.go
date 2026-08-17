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
	FlashSaleSoldOut
	FlashSaleNotInWindow
	FlashSaleLimitReached
	FlashSaleOffline
)

type FlashSalePreDeductParams struct {
	StockKey     string
	CountKey     string
	Now          time.Time
	StartAt      time.Time
	EndAt        time.Time
	OnSale       bool
	PerUserLimit int
}

type FlashSaleRestoreParams struct {
	StockKey       string
	CountKey       string
	IdempotencyKey string
	Quantity       int
}

// FlashSaleStore owns the scripts that mutate flash-sale Redis state.
type FlashSaleStore interface {
	WarmFlashSaleStock(ctx context.Context, p FlashSaleWarmParams) (FlashSaleWarmResult, error)
	PreDeductFlashSale(ctx context.Context, p FlashSalePreDeductParams) (FlashSalePreDeductResult, error)
	RestoreFlashSale(ctx context.Context, p FlashSaleRestoreParams) error
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
return 1
`

const restoreFlashSaleScript = `
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
redis.call('DEL', KEYS[3])
return 1
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
	onSale := 0
	if p.OnSale {
		onSale = 1
	}
	code, err := r.evalInt(ctx, preDeductFlashSaleScript, []string{p.StockKey, p.CountKey},
		p.Now.UnixMilli(), p.StartAt.UnixMilli(), p.EndAt.UnixMilli(), onSale, p.PerUserLimit)
	if err != nil {
		return 0, err
	}
	switch code {
	case 1:
		return FlashSalePreDeducted, nil
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

func (r *redisCache) RestoreFlashSale(ctx context.Context, p FlashSaleRestoreParams) error {
	if p.Quantity <= 0 {
		return fmt.Errorf("flash-sale restore quantity must be positive: %d", p.Quantity)
	}
	code, err := r.evalInt(ctx, restoreFlashSaleScript,
		[]string{p.StockKey, p.CountKey, p.IdempotencyKey}, p.Quantity)
	if err != nil {
		return err
	}
	if code != 1 {
		return fmt.Errorf("unexpected flash-sale restore result: %d", code)
	}
	return nil
}

func (r *redisCache) IncrementFixedWindow(ctx context.Context, key string, window time.Duration) (int64, error) {
	return r.evalInt(ctx, incrementFixedWindowScript, []string{key}, int64(window.Seconds()))
}
