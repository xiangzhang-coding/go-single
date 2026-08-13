// 有限重试 + 退避单元测试：重试直至成功、次数上限、ctx 取消快速失败、
// 零值配置单次执行（非幂等操作不重试的默认行为）。
package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

func TestDoRetriesUntilSuccess(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{
		Attempts:       4,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}, func(ctx context.Context) error {
		calls++
		if calls < 3 { // 前两次失败，第三次成功
			return errBoom
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, calls, "失败两次后第三次成功，不应继续重试")
}

func TestDoGivesUpAfterAttempts(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{
		Attempts:       3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}, func(ctx context.Context) error {
		calls++
		return errBoom
	})
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 3, calls, "达到次数上限后停止，重试次数受限")
}

func TestDoSingleShotOnZeroConfig(t *testing.T) {
	// 零值配置 = 单次执行（调用方不启用重试时的默认行为，非幂等操作走此路径）。
	calls := 0
	err := Do(context.Background(), Config{}, func(ctx context.Context) error {
		calls++
		return errBoom
	})
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 1, calls)
}

func TestDoRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	err := Do(ctx, Config{
		Attempts:       10,
		InitialBackoff: time.Hour, // 退避远超测试时长：若未感知取消会挂起
		MaxBackoff:     time.Hour,
	}, func(ctx context.Context) error {
		calls++
		cancel() // 首次失败后取消 ctx，退避应立即中断
		return errBoom
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls, "ctx 取消后不再发起下一次尝试")
}

func TestDoReturnsContextErrorWhenExpiredBeforeAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	calls := 0
	err := Do(ctx, Config{Attempts: 3, InitialBackoff: time.Millisecond}, func(ctx context.Context) error {
		calls++
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 0, calls)
}

func TestBackoffCapsAtMaxAndJitters(t *testing.T) {
	cfg := Config{InitialBackoff: time.Second, MaxBackoff: time.Second, Jitter: 10 * time.Millisecond}
	for attempt := 0; attempt < 5; attempt++ {
		d := backoff(cfg, attempt)
		require.LessOrEqual(t, d, time.Second+10*time.Millisecond, "退避封顶 MaxBackoff + 抖动")
		require.GreaterOrEqual(t, d, time.Second, "退避不小于 InitialBackoff")
	}
}

// Stop 标记的业务错误不重试：即使 Attempts 充足也立即返回原错误（重试无意义）。
func TestDoStopsOnStoppedError(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{
		Attempts:       5,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}, func(ctx context.Context) error {
		calls++
		return Stop(errBoom) // 业务拒绝：不重试
	})
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 1, calls, "Stop 标记的错误应立即返回，不发起重试")
}
