package health

import (
	"context"
	"fmt"
	"time"

	"github.com/xiangzhang-coding/go-single/internal/platform/cache"
	"github.com/xiangzhang-coding/go-single/internal/platform/mq"
)

// DB 满足 *sql.DB 的最小接口，便于测试替身。
type DB interface {
	PingContext(ctx context.Context) error
}

// Checker 汇总各依赖的连通性状态。
type Checker struct {
	MySQL   DB
	Cache   cache.Cache
	MQ      mq.MQ
	Timeout time.Duration
}

// Result 一次健康检查的结果。
type Result struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// 依赖状态取值。
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StateOK        = "ok"
)

// Check 并发探测全部依赖，任何一项失败即 degraded（不因单项拖慢整体）。
func (h *Checker) Check(ctx context.Context) Result {
	ctx, cancel := context.WithTimeout(ctx, h.timeoutOrDefault())
	defer cancel()

	type depResult struct {
		name string
		err  error
	}

	deps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"mysql", h.MySQL.PingContext},
		{"redis", h.Cache.Ping},
		{"mq", h.MQ.Ping},
	}

	results := make(chan depResult, len(deps))
	for _, d := range deps {
		go func() {
			results <- depResult{name: d.name, err: d.fn(ctx)}
		}()
	}

	res := Result{Status: StatusOK, Checks: make(map[string]string, len(deps))}
	for range deps {
		r := <-results
		if r.err != nil {
			res.Checks[r.name] = fmt.Sprintf("unreachable: %v", r.err)
			res.Status = StatusDegraded
		} else {
			res.Checks[r.name] = StateOK
		}
	}
	return res
}

func (h *Checker) timeoutOrDefault() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return 2 * time.Second
}
