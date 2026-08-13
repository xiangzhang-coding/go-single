// Package retry 提供有限重试 + 指数退避（T20 容错收尾）。
//
// 使用约束（与 DESIGN.md 容错约定一致）：仅幂等操作可重试——重试会重复执行
// fn，非幂等操作（加购、发动态、好友申请等）不得使用本包；下单/支付/消息发布
// 等幂等操作（唯一约束/幂等键去重）可用本包吸收瞬时基础设施故障。
package retry

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// Config 重试参数；零值 = 单次执行（不重试）。
type Config struct {
	// Attempts 总尝试次数（含首次）；<=1 表示不重试。
	Attempts int
	// InitialBackoff 首次重试前的等待时长；每次重试翻倍。
	InitialBackoff time.Duration
	// MaxBackoff 退避上限（指数增长封顶）。
	MaxBackoff time.Duration
	// Jitter 每次退避叠加的最大随机抖动（避免惊群；0 = 无抖动）。
	Jitter time.Duration
}

// Do 按配置执行 fn：失败（非 nil 错误）且未耗尽 Attempts 时退避后重试；
// 任一时刻 ctx 取消（超时/关闭）立即返回 ctx.Err()，不再发起下一次尝试。
// 返回 Stop 包装的错误时不重试并原样返回（业务拒绝等不可重试错误）。
// 其余情况返回最后一次 fn 的错误。
func Do(ctx context.Context, cfg Config, fn func(ctx context.Context) error) error {
	if cfg.Attempts <= 1 {
		return fn(ctx)
	}

	var err error
	for attempt := 0; attempt < cfg.Attempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = fn(ctx); err == nil {
			return nil
		}
		var se stopError
		if errors.As(err, &se) {
			return se.err // 不可重试：原样返回业务错误
		}
		if attempt == cfg.Attempts-1 {
			break
		}
		// 退避等待可被取消：ctx 到点返回 ctx.Err()，不再发起下一次尝试。
		if err := sleep(ctx, backoff(cfg, attempt)); err != nil {
			return err
		}
	}
	return err
}

// stopError 不可重试标记：Do 遇到时立即停止并还原原始错误。
type stopError struct{ err error }

func (e stopError) Error() string { return e.err.Error() }

func (e stopError) Unwrap() error { return e.err }

// Stop 标记错误为不可重试：业务拒绝等错误重试也不会成功（如库存不足、券不可用），
// 幂等操作的调用方在 fn 内用它终止重试，避免无意义的重复执行。
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return stopError{err}
}

// backoff 第 attempt 次失败后的退避：InitialBackoff × 2^attempt，封顶 MaxBackoff，
// 叠加 [0, Jitter) 随机抖动（Jitter <= 0 不抖动）。
func backoff(cfg Config, attempt int) time.Duration {
	d := cfg.InitialBackoff
	if cfg.MaxBackoff > 0 && d > cfg.MaxBackoff {
		d = cfg.MaxBackoff
	}
	for i := 0; i < attempt; i++ {
		d *= 2
		if cfg.MaxBackoff > 0 && d > cfg.MaxBackoff {
			d = cfg.MaxBackoff
			break
		}
	}
	if cfg.Jitter > 0 {
		d += time.Duration(rand.Int63n(int64(cfg.Jitter)))
	}
	return d
}

// sleep 可被 ctx 取消的等待：ctx 到点立即返回（由 Do 透传给调用方快速失败）。
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
