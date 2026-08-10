// Package cron 提供定时任务注册与调度机制（robfig/cron 之上的薄封装）：
// 任务以 (名称, cron 表达式, 回调) 注册，调度器统一启动/优雅停止；
// 回调失败与 panic 均被捕获并记录日志，不中断调度；
// 同一任务重叠执行被跳过（SkipIfStillRunning），单次执行可设超时。
//
// 单体单实例：任务只注册一次（多实例需分布式锁防重复执行，超出本阶段范围）。
package cron

import (
	"context"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Job 一个定时任务：Name 为日志标识，Spec 为 cron 表达式（分精度，
// 如 "* * * * *" 每分钟 / "@every 1m"）；Fn 返回 error 只记录日志，
// 不影响后续触发。
type Job struct {
	Name string
	Spec string
	Fn   func(ctx context.Context) error
}

// Registry 定时任务注册表。
type Registry struct {
	cron    *cron.Cron
	logger  *zap.Logger
	timeout time.Duration // 单次执行超时；0 = 不设超时

	mu      sync.Mutex
	started bool
}

// New 构造注册表；perRunTimeout 为单次执行超时（0 = 不设超时）。
// 超时后任务仍在后台运行，由 SkipIfStillRunning 保证下次触发不叠加。
func New(logger *zap.Logger, perRunTimeout time.Duration) *Registry {
	adapter := zapCronLogger{log: logger}
	c := cron.New(
		// Recover 置于最内层：panic 在 Skip 的令牌归还前被兜底，
		// 避免 panic 导致任务被永久跳过；Skip 在外层防任务重叠。
		cron.WithChain(cron.SkipIfStillRunning(adapter), cron.Recover(adapter)),
	)
	return &Registry{cron: c, logger: logger, timeout: perRunTimeout}
}

// Register 注册任务；Spec 非法（如 "* * *" 缺字段）返回错误。
// 建议在 Start 前一次性注册全部任务（运行期注册虽可用，但不利于排障）。
func (r *Registry) Register(job Job) error {
	_, err := r.cron.AddJob(job.Spec, cron.FuncJob(func() {
		ctx := context.Background()
		if r.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, r.timeout)
			defer cancel()
		}
		start := time.Now()
		if err := job.Fn(ctx); err != nil {
			r.logger.Error("cron 任务执行失败",
				zap.String("job", job.Name), zap.Error(err),
				zap.Duration("cost", time.Since(start)))
			return
		}
		r.logger.Info("cron 任务执行完成",
			zap.String("job", job.Name), zap.Duration("cost", time.Since(start)))
	}))
	if err != nil {
		return err
	}
	r.logger.Info("cron 任务已注册", zap.String("job", job.Name), zap.String("spec", job.Spec))
	return nil
}

// Start 启动调度器；已启动时 no-op。
func (r *Registry) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.cron.Start()
	r.started = true
	r.logger.Info("cron 调度器已启动")
}

// Stop 停止调度并等待执行中的任务结束；ctx 超时返回 ctx.Err()
// （执行中的任务随后自行结束，不影响进程退出）。
func (r *Registry) Stop(ctx context.Context) error {
	r.mu.Lock()
	started := r.started
	r.started = false
	r.mu.Unlock()
	if !started {
		return nil
	}
	done := r.cron.Stop()
	r.logger.Info("cron 调度器已停止，等待执行中的任务")
	select {
	case <-done.Done():
		r.logger.Info("cron 任务全部结束")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// zapCronLogger 适配 robfig/cron 的 Logger 接口（调度事件/panic 记录到 zap）。
type zapCronLogger struct {
	log *zap.Logger
}

func (l zapCronLogger) Info(msg string, keysAndValues ...any) {
	l.log.Info("cron: "+msg, zap.Any("detail", keysAndValues))
}

func (l zapCronLogger) Error(err error, msg string, keysAndValues ...any) {
	l.log.Error("cron: "+msg, zap.Error(err), zap.Any("detail", keysAndValues))
}
