// 定时任务注册表单元测试：注册/非法表达式/定时触发/失败与 panic 不中断/
// 重叠跳过/优雅停止等待执行中任务。不依赖外部组件，纯进程内断言。
// 注：robfig/cron 的 @every 小于 1 秒会向上取整为 1 秒，测试统一用 1s 周期。
package cron_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	platformcron "github.com/xiangzhang-coding/go-single/internal/platform/cron"
)

func testLogger() *zap.Logger {
	return zap.NewNop()
}

// 注册非法表达式返回错误。
func TestRegistryRejectsInvalidSpec(t *testing.T) {
	r := platformcron.New(testLogger(), 0)
	err := r.Register(platformcron.Job{Name: "bad", Spec: "* * *", Fn: func(context.Context) error { return nil }})
	require.Error(t, err, "缺字段的表达式应被拒绝")
}

// 按周期触发：任务至少执行一次，成功路径无错误。
func TestRegistryRunsJob(t *testing.T) {
	r := platformcron.New(testLogger(), 0)
	var hits atomic.Int32
	require.NoError(t, r.Register(platformcron.Job{
		Name: "tick",
		Spec: "@every 1s",
		Fn:   func(context.Context) error { hits.Add(1); return nil },
	}))
	r.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, r.Stop(ctx))
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, hits.Load(), int32(1), "任务应在周期内被触发")
}

// 回调失败只记录日志：后续触发继续执行，调度不中断。
func TestRegistrySurvivesJobError(t *testing.T) {
	r := platformcron.New(testLogger(), 0)
	var hits atomic.Int32
	require.NoError(t, r.Register(platformcron.Job{
		Name: "flaky",
		Spec: "@every 1s",
		Fn: func(context.Context) error {
			hits.Add(1)
			return context.DeadlineExceeded
		},
	}))
	r.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, r.Stop(ctx))
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, hits.Load(), int32(2), "失败的任务应被多次触发")
}

// 回调 panic 被兜底（Recover 置于 Skip 内层，token 不丢失）：
// 调度器继续运行，后续触发照常。
func TestRegistrySurvivesPanic(t *testing.T) {
	r := platformcron.New(testLogger(), 0)
	var hits atomic.Int32
	require.NoError(t, r.Register(platformcron.Job{
		Name: "panic",
		Spec: "@every 1s",
		Fn: func(context.Context) error {
			if hits.Add(1) == 1 {
				panic("boom")
			}
			return nil
		},
	}))
	r.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, r.Stop(ctx))
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	require.GreaterOrEqual(t, hits.Load(), int32(2), "panic 后调度不应中断")
}

// 重叠执行被跳过：慢任务未结束时，周期触发不叠加执行。
func TestRegistrySkipsOverlappingRun(t *testing.T) {
	r := platformcron.New(testLogger(), 0)
	var running atomic.Int32
	var maxConcurrent atomic.Int32
	release := make(chan struct{})
	require.NoError(t, r.Register(platformcron.Job{
		Name: "slow",
		Spec: "@every 1s",
		Fn: func(ctx context.Context) error {
			cur := running.Add(1)
			defer running.Add(-1)
			for {
				old := maxConcurrent.Load()
				if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
					break
				}
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}))
	r.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, r.Stop(ctx))
	}()

	// 慢任务持有 1.5s（覆盖第二个触发周期），期间应始终只有 1 个并发。
	time.Sleep(1500 * time.Millisecond)
	require.Equal(t, int32(1), maxConcurrent.Load(), "重叠触发应被跳过")
	close(release)
	time.Sleep(100 * time.Millisecond)
}

// 优雅停止：未启动 Stop 直接返回；已启动等待执行中的任务结束。
func TestRegistryGracefulStop(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan struct{})
	r := platformcron.New(testLogger(), 0)
	require.NoError(t, r.Register(platformcron.Job{
		Name: "block",
		Spec: "@every 1s",
		Fn: func(context.Context) error {
			close(entered)
			<-blocked
			return nil
		},
	}))
	r.Start()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("任务未在周期内开始执行")
	}

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stopped <- r.Stop(ctx)
	}()

	// 任务仍在执行：Stop 应等待，不立即返回。
	select {
	case err := <-stopped:
		t.Fatalf("Stop 不应在任务结束前返回: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(blocked)
	require.NoError(t, <-stopped, "任务结束后 Stop 应正常返回")

	// 未启动的注册表 Stop 直接返回。
	require.NoError(t, platformcron.New(testLogger(), 0).Stop(context.Background()))
}
