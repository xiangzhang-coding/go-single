---
sidebar_position: 11
---

# 10 部署与运维

## Q1. Docker Compose 编排多服务

**答案要点**

- compose 声明式描述依赖栈：镜像、端口映射、卷、环境变量、健康检查。
- 每个服务应有 **healthcheck**：容器"起来" ≠ "可用"。
- 前端 dist 挂载进 nginx 容器；日志目录挂进 promtail 采集。
- 根目录 compose 只做 include 桥，规范文件放 `deploy/`（发布物集中）。

**可运行代码**

```go title="interview/ch10_deploy/q01_docker_compose/main.go"
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

type composeFile struct {
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Ports       []string          `yaml:"ports"`
		Volumes     []string          `yaml:"volumes"`
		Healthcheck map[string]any    `yaml:"healthcheck"`
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

func main() {
	// 从当前目录向上找 deploy/docker-compose.yml。
	dir, _ := os.Getwd()
	for d := dir; ; d = filepath.Dir(d) {
		p := filepath.Join(d, "deploy", "docker-compose.yml")
		if b, err := os.ReadFile(p); err == nil {
			var cf composeFile
			if err := yaml.Unmarshal(b, &cf); err != nil {
				fmt.Println("解析失败:", err)
				return
			}
			names := make([]string, 0, len(cf.Services))
			for n := range cf.Services {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				s := cf.Services[n]
				hc := "无健康检查"
				if s.Healthcheck != nil {
					hc = "有健康检查"
				}
				fmt.Printf("%-10s %-28s 端口=%v 健康检查: %s\n", n, s.Image, s.Ports, hc)
			}
			return
		} else if d == "/" {
			break
		}
	}
	fmt.Println("未找到 compose 文件")
}

```

**项目位置**：`deploy/docker-compose.yml`（mysql/redis/rabbitmq/minio/nginx/prometheus/grafana/loki/promtail 全带 healthcheck）；根目录 `docker-compose.yml` 是 include 桥。

## Q2. 反向代理与静态托管

**答案要点**

- 前端静态文件由 Nginx 托管，`try_files $uri /index.html` 做 SPA 回退。
- `/api` 反代到后端；`/ws` 需要 `Upgrade` 头转发 + `proxy_read_timeout` 放宽。
- 静态资源（assets）加长缓存头，减少回源。
- 后端 CORS 白名单与 nginx 反代配合：浏览器只看到 nginx 域。

**可运行代码**

```go title="interview/ch10_deploy/q02_reverse_proxy/main.go"
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
)

func main() {
	// 后端（Go 服务）。
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend got: %s", r.URL.Path)
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy := httputil.NewSingleHostReverseProxy(target) // 标准库反向代理

	// 前端入口：静态文件（SPA）与 /api 分流（对应 nginx try_files + location /api）。
	proxyReq := httptest.NewRequest("GET", "/api/products", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, proxyReq)
	fmt.Printf("请求 /api/products → 转发后端 → %q\n", rec.Body.String())

	fmt.Println("nginx 侧：location /api { proxy_pass http://host.docker.internal:8080; }")
	fmt.Println("         location / { try_files $uri /index.html; }（SPA 回退）")
	fmt.Println("         location /ws { proxy_http_version 1.1; Upgrade 头; }")
}

```

**项目位置**：`deploy/nginx/nginx.conf`——静态托管 `web/faire/dist` + `/api` 反代 + `/ws` upgrade（read_timeout 90s）+ try_files 回退；compose nginx 挂载 dist。

## Q3. 优雅重启与信号处理

**答案要点**

- 运维信号约定：SIGTERM 优雅停机（docker stop 默认发）、SIGHUP 重载配置。
- 优雅停机顺序：停调度器 → 停 HTTP → 关长连接；在途请求有宽限期。
- 配置热加载：SIGHUP 重读或 viper WatchConfig；注意重载失败回退旧配置。
- 容器里 PID 1 要正确转发信号（tini/直接前台进程）。

**可运行代码**

```go title="interview/ch10_deploy/q03_graceful_reload/main.go"
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func loadConfig() map[string]string {
	return map[string]string{"flashsale.token_bucket.qps": "20"}
}

func main() {
	// 运维惯例：SIGTERM 优雅停机，SIGHUP 重载配置/日志文件。
	// 本项目只实现 SIGTERM 优雅关闭（main.go signal.Notify）；
	// SIGHUP 重载留作扩展，viper 的 WatchConfig 是备选。
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM)

	cfg := loadConfig()
	fmt.Println("当前配置:", cfg)

	go func() {
		for sig := range ch {
			switch sig {
			case syscall.SIGHUP:
				cfg = loadConfig() // 重读配置
				fmt.Println("SIGHUP: 配置已重载", cfg)
			case syscall.SIGTERM:
				fmt.Println("SIGTERM: 优雅退出（处理在途请求）")
				os.Exit(0)
			}
		}
	}()
	select {} // 用 Ctrl+C 发 SIGINT 结束演示
}

```

**项目位置**：`cmd/server/main.go`——SIGINT/SIGTERM → `cronRegistry.Stop(5s)` → `srv.Shutdown(10s)` → `wsHub.Close()`；compose healthcheck 探 `/healthz`。

## Q4. 就绪探针：服务真的能干活才算健康

**答案要点**

- 存活探针（Liveness）：进程活着但僵死 → 重启；就绪探针（Readiness）：能接流量 → 摘除。
- 依赖探活要**并发 + 超时**：串行探测拖慢健康检查。
- 聚合状态：任一依赖 down → 503，调度器摘流量，恢复自动回归。
- 健康检查端点不带鉴权、无业务副作用。

**可运行代码**

```go title="interview/ch10_deploy/q04_healthcheck/main.go"
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type dependency struct {
	name string
	err  error
}

// 并发探测各依赖（与 platform/health 同构：goroutine + buffered channel + 超时）。
func check(ctx context.Context) (map[string]bool, bool) {
	deps := []dependency{{name: "mysql"}, {name: "redis"}, {name: "rabbitmq"}}
	results := make(chan struct {
		string
		bool
	}, len(deps))

	var wg sync.WaitGroup
	for _, d := range deps {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			time.Sleep(20 * time.Millisecond) // 模拟探测
			results <- struct {
				string
				bool
			}{n, d.err == nil}
		}(d.name)
	}
	go func() { wg.Wait(); close(results) }()

	ok := true
	state := map[string]bool{}
	select {
	case <-ctx.Done():
		return nil, false
	default:
	}
	for r := range results {
		state[r.string] = r.bool
		if !r.bool {
			ok = false
		}
	}
	return state, ok
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	state, ok := check(ctx)
	fmt.Printf("状态: %v → 探针返回 %v（任一依赖挂 → 503 从负载均衡摘除）\n", state, ok)
	_ = errors.New("unused")
}

```

**项目位置**：`internal/platform/health/health.go` 的 `Check` + GET `/healthz`（main.go 395-404）；compose 各服务 healthcheck。

## Q5. 数据持久化与备份

**答案要点**

- 容器无状态、数据进卷：删容器不删数据（mysql_data/redis_data/minio_data 命名卷）。
- 日志另管：落盘文件 → promtail 采集，不占容器层。
- 备份要"备份 + 恢复演练"闭环；异地 + 定时 + 版本化。
- 开发/生产卷分离，避免误删生产数据。

**可运行代码**

```go title="interview/ch10_deploy/q05_persistence/main.go"
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	// 容器重建即失数据 → 数据目录必须挂卷（mysql_data/redis_data 命名卷）。
	fmt.Println("compose 挂载约定：")
	fmt.Println("  mysql:    mysql_data:/var/lib/mysql（无状态进程 + 有状态数据卷）")
	fmt.Println("  redis:    redis_data:/data（AOF/RDB 落盘路径）")
	fmt.Println("  minio:    minio_data:/data（对象存储本体）")
	fmt.Println("  promtail: ../logs:/logs（采集宿主机后端日志文件）")

	// 备份演练：拷贝数据文件到带时间戳的备份目录。
	dataDir := os.TempDir()
	backup := filepath.Join(dataDir, "backup-"+time.Now().Format("20060102-150405"))
	_ = os.MkdirAll(backup, 0o755)
	fmt.Printf("备份目录: %s（生产应异地 + 定期验证恢复）\n", backup)
}

```

**项目位置**：`deploy/docker-compose.yml` 的 volumes；日志持久化走 `log.file` 镜像（`configs/config.yaml`）+ promtail。

## Q6. 版本化迁移（golang-migrate）

**答案要点**

- schema 变更版本化：up/down 成对 SQL，按版本号顺序应用，可回退。
- 迁移只前移不修改旧文件：改旧迁移 = 破坏已部署环境的版本历史。
- 启动时自动执行（幂等）；失败标记 dirty，需人工修复再继续。
- 业务数据变更（如 000014 回填）也在迁移里做，保证环境一致。

**可运行代码**

```go title="interview/ch10_deploy/q06_migrations/main.go"
package main

import (
	"fmt"
	"sort"
)

type migration struct {
	version int
	name    string
}

// 简化迁移执行器：按版本号升序应用，逐版本记录 dirty 状态。
func runUp(current int, ms []migration) (int, error) {
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for _, m := range ms {
		if m.version > current {
			fmt.Printf("执行 %03d_%s ... OK\n", m.version, m.name)
			current = m.version
		}
	}
	return current, nil
}

func main() {
	ms := []migration{
		{2, "users"}, {1, "init"}, {9, "orders"}, {14, "seckill_repurchase"},
	}
	cur, err := runUp(0, ms)
	if err != nil {
		fmt.Println("迁移失败（dirty 版本需人工修复）:", err)
		return
	}
	fmt.Printf("schema 前进到版本 %d（当前 000014）\n", cur)
	fmt.Println("要点：迁移只前移不修改旧文件；升级走 migrate up，回退走 migrate down")
}

```

**项目位置**：`migrations/000001_init.up.sql` ~ `000014_seckill_repurchase.up.sql`（up/down 成对）；启动执行 `runMigrations`（`cmd/server/main.go` 197-209）。

## Q7. 发布流水线（CI/CD）

**答案要点**

- 流水线 = 有序步骤 + 失败即停：测试 → 编译 → 镜像 → 推送 → 部署 → 健康检查放行。
- 门禁：测试不过不构建，镜像不可复现不部署（tag 唯一）。
- 部署策略演进：重建/滚动/蓝绿；healthcheck 通过才切流量。
- 本项目为学习仓库暂无 CI 配置（BACKLOG 列部署演进），本地流程明确。

**可运行代码**

```go title="interview/ch10_deploy/q07_cicd/main.go"
package main

import (
	"fmt"
)

type step struct {
	name string
	run  func() error
}

func main() {
	// 流水线 = 有序步骤 + 失败即停（本例模拟执行，真实用 GitHub Actions 等）。
	pipeline := []step{
		{"单元测试与集成测试", func() error { fmt.Println("  go test ./..."); return nil }},
		{"编译", func() error { fmt.Println("  go build ./... && go vet ./..."); return nil }},
		{"构建镜像", func() error { fmt.Println("  docker build -t go-single:${TAG} ."); return nil }},
		{"推送镜像仓库", func() error { fmt.Println("  docker push ..."); return nil }},
		{"滚动发布", func() error { fmt.Println("  docker compose up -d --pull always"); return nil }},
	}
	for i, s := range pipeline {
		fmt.Printf("[%d/5] %s\n", i+1, s.name)
		if err := s.run(); err != nil {
			fmt.Println("流水线中止:", err)
			return
		}
	}
	fmt.Println("发布完成：healthcheck 通过后流量进入新版本")
}

```

**项目位置**：本仓库暂无 CI 文件（BACKLOG）；本地流程 = `docker compose up`（依赖）+ `go run ./cmd/server`；前端 `bun build`（web/faire、website/）；文档站可接 Cloudflare Pages。
