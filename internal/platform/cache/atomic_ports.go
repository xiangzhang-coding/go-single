package cache

import (
	"context"
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

// ProductDetailStore prevents a cache-miss request from publishing a detail
// snapshot after a product or SKU mutation has invalidated its generation.
type ProductDetailKeys struct {
	Detail   string
	Version  string
	Mutation string
}

type ProductDetailStore interface {
	ProductDetailVersion(ctx context.Context, keys ProductDetailKeys) (int64, error)
	SetProductDetailIfVersion(ctx context.Context, keys ProductDetailKeys, version int64, value string, ttl time.Duration) (bool, error)
	BeginProductDetailMutation(ctx context.Context, keys ProductDetailKeys, token string, ttl, aofTimeout time.Duration) error
	FinishProductDetailMutation(ctx context.Context, keys ProductDetailKeys, token string, aofTimeout time.Duration) error
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
	ClaimedKey        string
	PerUserKey        string
	VersionKey        string
	PerUserVersionKey string
	Now               time.Time
	Total             int
	ValidFrom         time.Time
	ValidUntil        time.Time
	PerUserLimit      int
	ClaimedCount      int64
	PerUserCount      int64
}

type CouponCountParams struct {
	ClaimedKey        string
	PerUserKey        string
	VersionKey        string
	PerUserVersionKey string
	ClaimedCount      int64
	PerUserCount      int64
}

// CouponStore maintains reconstructible coupon counters without exposing Lua
// or Redis return codes to the coupon module.
type CouponStore interface {
	ClaimCoupon(ctx context.Context, p CouponClaimParams) (CouponClaimResult, error)
	SyncCouponCounts(ctx context.Context, p CouponCountParams) error
}

// FlashSaleWarmResult reports whether live Redis stock was updated or kept.
type FlashSaleWarmResult uint8

const (
	FlashSaleStockUpdated FlashSaleWarmResult = iota + 1
	FlashSaleStockRetained
)

type FlashSaleWarmParams struct {
	StockKey  string
	Stock     int
	TTL       time.Duration
	Overwrite bool
}

type FlashSaleDecreaseParams struct {
	StockKey string
	Delta    int
}

type FlashSalePauseParams struct {
	StockKey string
	PauseKey string
	Token    string
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
	FlashSalePaused
)

type FlashSalePreDeductParams struct {
	StockKey         string
	CountKey         string
	PauseKey         string
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
	WarmFlashSaleStockDurably(ctx context.Context, p FlashSaleWarmParams, timeout time.Duration) (FlashSaleWarmResult, error)
	DecreaseFlashSaleStockDurably(ctx context.Context, p FlashSaleDecreaseParams, timeout time.Duration) error
	PauseFlashSaleStockDurably(ctx context.Context, p FlashSalePauseParams, timeout time.Duration) (int, error)
	HoldFlashSalePauseDurably(ctx context.Context, pauseKey string, timeout time.Duration) error
	ReleaseFlashSalePauseDurably(ctx context.Context, pauseKey, token string, timeout time.Duration) error
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
	ProductDetailStore
	CouponStore
	FlashSaleStore
	FixedWindowStore
}
