---
sidebar_position: 4
---

# 03 网络

## Q1. HTTP 中间件链：洋葱模型

**答案要点**

- 中间件 = 包一层 Handler：请求进来按装配顺序**先外后内**执行前置逻辑，响应再**由内向外**执行后置逻辑。
- Gin 中 `Use` 的注册顺序即执行顺序；`c.Next()` 控制何时进入下一个。
- 职责划分：日志、鉴权、限流、CORS、超时、recover 各自独立。
- 中间件链顺序很重要：访问日志要在最外（记录所有请求），recover 要包住业务。

**可运行代码**

```go title="interview/ch03_network/q01_middleware/main.go"
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

// 中间件：包一层 http.Handler，处理前后各挂一段逻辑。
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("→ 请求日志:", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
		fmt.Println("← 响应已写出")
	})
}

func metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("→ 指标计数 +1")
		next.ServeHTTP(w, r)
	})
}

func main() {
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// 链式装配：外部先执行，内层后执行（与 Gin 的 Use 顺序一致）。
	h = logging(metrics(h))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/products", nil))
	fmt.Println("状态码:", rec.Code)
}
```

**项目位置**：`cmd/server/main.go` 装配链——`metricRegistry.GinMiddleware() → gin.Recovery() → requestLogger → platformcors.Middleware`；`/api` 组再挂 `requestTimeout`；鉴权 `auth.Middleware`/`auth.RequireAdmin()` 按路由挂载。

## Q2. Bearer Token 解析与鉴权中间件

**答案要点**

- 规范格式 `Authorization: Bearer <token>`，前缀大小写不敏感，需校验"恰好两段、token 非空"。
- 解析失败 → 401；解析成功 → 验签 → 把 claims 塞进请求上下文供业务取用。
- 校验失败不区分"过期/篡改/格式错"细节（防探测）。
- WS 场景无法带自定义头，项目用 `?token=` 查询参数（有日志泄漏取舍，需注明）。

**可运行代码**

```go title="interview/ch03_network/q02_bearer_token/main.go"
package main

import (
	"errors"
	"fmt"
	"strings"
)

func bearerToken(header string) (string, error) {
	// Authorization: Bearer <token>；要求恰好两部分且前缀不区分大小写。
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("缺失或不合法 Authorization 头")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errors.New("token 为空")
	}
	return token, nil
}

func main() {
	for _, h := range []string{"Bearer abc.def.ghi", "bearer token2", "", "Basic xyz", "Bearer "} {
		t, err := bearerToken(h)
		if err != nil {
			fmt.Printf("header %-18q → 401 %v\n", h, err)
			continue
		}
		fmt.Printf("header %-18q → 通过，token=%s\n", h, t)
	}
}
```

**项目位置**：`internal/platform/auth/middleware.go` 的 `bearerToken` 与 `Middleware`（失败 401）；验签 `Verify` 在 `internal/platform/auth/jwt.go`。

## Q3. 请求超时与 context 传播（504）

**答案要点**

- 请求级超时中间件为每个请求重建带 deadline 的 ctx，回写 `c.Request`，全链路传递。
- service/repo 的每个 IO 都要感知 ctx（`ctx.Done()` 或驱动内置支持）。
- `context.DeadlineExceeded` 应映射为 **504**（区别于业务错误 4xx）。
- 超时值要大于下游最坏延迟：请求 5s / MQ 消息 15s / cron 任务 5min / 探活 2s。

**可运行代码**

```go title="interview/ch03_network/q03_timeout_ctx/main.go"
package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 业务函数：ctx 一路传递，链路任何一处超时立即失败。
func loadActivity(ctx context.Context) error {
	select {
	case <-time.After(2 * time.Second): // 模拟慢 DB
		return nil
	case <-ctx.Done():
		return fmt.Errorf("加载活动: %w", ctx.Err())
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := loadActivity(ctx)
	fmt.Printf("耗时 %v\n", time.Since(start).Round(time.Millisecond))
	if errors.Is(err, context.DeadlineExceeded) {
		fmt.Println("→ 超时，HTTP 应返回 504（handler 将 DeadlineExceeded 映射为 504）")
	} else if err != nil {
		fmt.Println("其他错误:", err)
	}
}
```

**项目位置**：`cmd/server/middleware.go` 的 `requestTimeout`；`internal/flashsale/handler/flashsale_handler.go` 的 `writeError` 把 `context.DeadlineExceeded` 映射 504。

## Q4. CORS 预检与白名单

**答案要点**

- 跨源时浏览器先发 **OPTIONS 预检**：`Origin` + `Access-Control-Request-Method/Headers`。
- 白名单校验：`Origin` 不在白名单 → 403；在 → 回 `Access-Control-Allow-*` 头 + 204。
- 预检响应带 `Access-Control-Max-Age` 减少重复预检。
- 项目用 Bearer 头而非 cookie 鉴权，CORS 不需要 `credentials`；白名单收敛自配。

**可运行代码**

```go title="interview/ch03_network/q04_cors/main.go"
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

// 白名单校验 + 预检应答（对应 internal/platform/cors 的简化版）。
func corsMiddleware(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r) // 非跨源直接放行
			return
		}
		if !allowed[origin] {
			w.WriteHeader(http.StatusForbidden) // 非白名单：预检 403
			fmt.Fprintln(w, "origin not allowed")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		if r.Method == http.MethodOptions { // 预检：只回响应头
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent) // 204
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	allowed := map[string]bool{"http://localhost:5173": true}
	h := corsMiddleware(allowed, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	preflight := httptest.NewRequest(http.MethodOptions, "/api/orders", nil)
	preflight.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, preflight)
	fmt.Printf("白名单预检 → %d, Allow-Origin=%s\n", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))

	evil := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	evil.Header.Set("Origin", "http://evil.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, evil)
	fmt.Printf("非白名单跨源 → %d\n", rec2.Code)
}
```

**项目位置**：`internal/platform/cors/cors.go`（白名单 + 预检 403/204 + max-age 86400），白名单来自配置 `server.cors.allow_origins`；nginx 层另有安全头（`deploy/nginx/nginx.conf`）。

## Q5. WebSocket 心跳与读写泵：writePump 模式

**答案要点**

- 读写泵：一个 goroutine 专职写（合并并发写）、一个专职读（防并发读写竞态）。
- 写泵用 select 同时监听：发送队列、**心跳 ticker（Ping）**、退出信号。
- 心跳保活：定期 Ping 探测对端，超时/失败即断开，释放连接与 goroutine。
- 慢消费者：发送缓冲满 → 断开（反压），避免内存膨胀拖垮进程。

**可运行代码**

```go title="interview/ch03_network/q05_websocket_ping/main.go"
package main

import (
	"fmt"
	"time"
)

// 模拟 WS 客户端：send 缓冲 + 退出信号 + 心跳写泵。
type client struct {
	send chan []byte
	done chan struct{}
}

func newClient() *client {
	return &client{send: make(chan []byte, 64), done: make(chan struct{})}
}

// writePump：select 同时等发送队列、退出信号与心跳时钟。
func (c *client) writePump() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case msg := <-c.send:
			fmt.Println("写帧:", string(msg))
		case <-ticker.C:
			fmt.Println("写帧: Ping（保活探测对端存活）")
		case <-c.done:
			fmt.Println("退出写泵")
			return
		}
	}
}

func main() {
	c := newClient()
	go c.writePump()

	c.send <- []byte("hello")
	time.Sleep(250 * time.Millisecond)
	close(c.done) // 触发写泵退出
	time.Sleep(10 * time.Millisecond)
}
```

**项目位置**：`internal/platform/ws/hub.go`——`client.send` 缓冲 64、`writePump` 心跳 Ping、慢消费者缓冲满断开；实时推送链路：chat service → `wsMessageNotifier` → `hub.PushToUser`（`cmd/server/main.go`）。

## Q6. HTTP 服务优雅关闭

**答案要点**

- 监听 `SIGINT`/`SIGTERM`（容器停止就是发 SIGTERM），不直接退出。
- `srv.Shutdown(ctx)`：停止接收新连接、等待在途请求完成、超时强关。
- 关闭顺序：先停调度器（cron）→ 再关 HTTP → 再关长连接资源（WS 不在 http.Server 管辖内）。
- 依赖的连接（DB/Redis/MQ）由进程退出回收；MQ 未确认消息由 broker 自动重投。

**可运行代码**

```go title="interview/ch03_network/q06_graceful_shutdown/main.go"
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	srv := &http.Server{Addr: ":18080"}

	go func() {
		_ = srv.ListenAndServe()
	}()

	// 监听退出信号；Ctrl+C 后走优雅关闭路径。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	fmt.Println("收到退出信号，开始优雅关闭")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Println("关闭超时/出错:", err)
	}
	fmt.Println("在途请求已处理完，服务退出")
}
```

**项目位置**：`cmd/server/main.go`——`cronRegistry.Stop(5s)` → `srv.Shutdown(ctx 10s)` → `defer wsHub.Close()`；MQ 消费者经 ctx 取消退出。

## Q7. REST 状态码语义：202/409/429/504

**答案要点**

- **202 Accepted**：异步受理（秒杀抢购排队），返回排队号供轮询。
- **409 Conflict**：状态冲突（重复提交、已支付再支付、抢光/限购）。
- **429 Too Many Requests**：限流（令牌桶/窗口计数）。
- **504 Gateway Timeout**：链路超时（`context.DeadlineExceeded`）。
- 语义对前端自动化很重要：按状态码分支而非猜字符串。

**可运行代码**

```go title="interview/ch03_network/q07_status_codes/main.go"
package main

import "fmt"

func main() {
	// 秒杀抢购各失败分支 → HTTP 状态码映射。
	type outcome struct {
		phase string
		code  int
		why   string
	}

	table := []outcome{
		{"令牌桶限流", 429, "全局 QPS 超限，让客户端退避重试"},
		{"幂等键冲突", 409, "同用户同活动重复提交"},
		{"预扣成功", 202, "排队中：异步落单，轮询订单号"},
		{"活动抢光", 409, "Lua 返回 0 → ErrSoldOut"},
		{"窗口外/下架", 409, "ErrNotInWindow / ErrOffline"},
		{"超过限购", 409, "ErrLimitReached（每人限购）"},
		{"链路超时", 504, "context.DeadlineExceeded"},
	}
	for _, o := range table {
		fmt.Printf("%-14s → %d（%s）\n", o.phase, o.code, o.why)
	}

	// 下单/支付等其他语义：201 创建成功、404 订单不存在、403 越权访问。
	fmt.Println("202 = 异步受理；204 = 预检/删除无内容；409 = 状态冲突；429 = 限流；504 = 网关超时")
}
```

**项目位置**：`internal/flashsale/handler/flashsale_handler.go` 的 `purchase` 返回 202 `{"status":"queued","order_no":...}`；`writeError` 统一映射（flashsale_handler.go / order_handler.go）；限流 429 在 limiter 中间件。
