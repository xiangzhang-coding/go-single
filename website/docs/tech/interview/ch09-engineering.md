---
sidebar_position: 10
---

# 09 工程实践

## Q1. 模块化单体 vs 微服务

**答案要点**

- 模块化单体：单部署单元，模块按业务域切分，**模块间经接口进程内调用**（非 HTTP）。
- 优点：无网络跳转、跨模块事务简单、无分布式一致性难题；微服务化前保留拆分可能。
- 纪律：依赖方向无环（DAG）、跨模块写汇入同一事务、禁止模块间直接操作对方表。
- 本项目刻意 Avoid：微服务、单层 main.go。

**可运行代码**

```go title="interview/ch09_engineering/q01_modular_monolith/main.go"
package main

import "fmt"

// 模块化单体 = 一个进程内按业务域切分的模块集合，模块间经接口调用。
// 对比微服务：无网络跳转（低延迟）、事务跨模块简单、无分布式难题；
// 代价：只能整体扩容，模块间耦合靠纪律（DAG 依赖）维持。

// 依赖方向：flashsale → order（flashsale 声明自己需要的最小订单能力），无环。
type SeckillOrderWriter interface {
	CreateInTx(orderNo string) error
}

type orderModule struct{}

func (orderModule) CreateInTx(orderNo string) error {
	return fmt.Errorf("simulate create order %s", orderNo)
}

type flashsaleModule struct{ orders SeckillOrderWriter }

func (m flashsaleModule) Handle(orderNo string) error {
	// 实际项目在同一事务中继续扣减 flashsale 活动库存。
	return m.orders.CreateInTx(orderNo)
}

func main() {
	// 进程内装配：flashsale 只拿到 order 的最小接口，order 不反向持有 flashsale。
	consumer := flashsaleModule{orders: orderModule{}}
	fmt.Println("flashsale 调用方视角：", consumer.Handle("1001"))

	fmt.Println("跨模块写怎么保证原子性？→ tx 参数汇入同一事务（见 q07）")
}

```

**项目位置**：`internal/flashsale/service` 的消费者、`SeckillCancellation` 声明 order 最小接口并完成应用编排；order 不持有 flashsale；装配在 `cmd/server/main.go`；依赖 DAG 见 `docs/DESIGN.md`。

## Q2. 端口-适配器（Ports & Adapters）

**答案要点**

- **端口在调用方**：consumer 声明自己需要的最小接口，不依赖提供方的具体类型。
- 适配器可换：GORM 仓储 / Redis 缓存 / RabbitMQ 都经接口初始化，替换成本低。
- 测试替身天然落地：fake 实现接口注入单测。
- 编译期断言 `var _ Repository = (*GORMRepo)(nil)` 防接口漂移。

**可运行代码**

```go title="interview/ch09_engineering/q02_di_ports/main.go"
package main

import "fmt"

// 端口（Port）：order 模块需要"减库存"能力，只声明自己用到的子集。
type StockDeduction interface {
	Deduct(skuID int64, qty int) error
}

// 适配器 1：真实 GORM 实现。
type GormStock struct{}

func (GormStock) Deduct(skuID int64, qty int) error {
	return fmt.Errorf("UPDATE skus SET stock=stock-%d WHERE id=%d AND stock>=%d", qty, skuID, qty)
}

// 适配器 2：测试替身（fake）——单测不需要真实 DB。
type FakeStock struct{ Calls int }

func (f *FakeStock) Deduct(skuID int64, qty int) error {
	f.Calls++
	return nil
}

func main() {
	var real StockDeduction = GormStock{}
	fmt.Println("生产:", real.Deduct(1, 1))

	fake := &FakeStock{}
	var svc StockDeduction = fake
	_ = svc.Deduct(1, 1)
	fmt.Println("测试注入 fake，调用次数:", fake.Calls)
}

```

**项目位置**：各模块 `Repository`/`Cache`/`MQ` 均为接口 + 具体实现（`internal/*/repository/*_gorm.go`）；ADR-0003（`docs/adr/0003-port-seams.md`）。

## Q3. 配置管理：默认值 + 文件 + 环境变量覆盖

**答案要点**

- 12-factor：配置外置，代码不含环境差异（本地/测试/生产同构）。
- viper：`setDefaults` 默认值 + 配置文件 + 环境变量覆盖，优先级后者压前者。
- 环境变量命名：前缀 `GO_SINGLE_` + 点号转下划线（`server.request_timeout` → `GO_SINGLE_SERVER_REQUEST_TIMEOUT`）。
- 校验集中：配置加载即校验，失败快速启动失败（fail fast）。

**可运行代码**

```go title="interview/ch09_engineering/q03_config/main.go"
package main

import (
	"fmt"
	"os"
	"strconv"
)

// 项目用 viper（configs/config.yaml + GO_SINGLE_ 前缀环境变量覆盖），
// 这里用标准库演示同款"默认值 → 环境变量"优先级。
type config struct {
	Server                serverConfig
	RequestTimeoutSeconds int
}

type serverConfig struct {
	Port int
}

func loadConfig() config {
	c := config{
		Server:                serverConfig{Port: 8080},
		RequestTimeoutSeconds: 5, // 默认值（config.yaml server.request_timeout: 5s）
	}
	if v := os.Getenv("GO_SINGLE_SERVER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.Port = n
		}
	}
	return c
}

func main() {
	// 演示环境变量覆盖。
	_ = os.Setenv("GO_SINGLE_SERVER_PORT", "9090")
	c := loadConfig()
	fmt.Printf("port=%d（被 GO_SINGLE_SERVER_PORT 覆盖）\n", c.Server.Port)

	// 项目默认：viper 的 . 号替换成 _ 命名（server.request_timeout → GO_SINGLE_SERVER_REQUEST_TIMEOUT），
	// AutomaticEnv 使未显式读的键也能被环境变量命中。
	fmt.Println("12-factor：配置外置，不写死在代码里")
}

```

**项目位置**：`internal/platform/config/config.go`——`Load`/`LoadFrom`、`setDefaults`、env 前缀 + 点号替换 + `AutomaticEnv`（158-185）；配置文件 `configs/config.yaml`。

## Q4. 分层架构：handler → service → repository → model

**答案要点**

- **handler**：HTTP 出入参、状态码、鉴权上下文提取；不写业务。
- **service**：业务规则、事务边界、跨模块编排、错误翻译。
- **repository**：数据访问、错误映射（1062 → 业务错误）、行锁/条件更新。
- **model**：表映射 + json 契约。
- 依赖只向相邻层：handler 不碰 DB，repo 不做业务判断。

**可运行代码**

```go title="interview/ch09_engineering/q04_layering/main.go"
package main

import (
	"errors"
	"fmt"
)

// model：数据结构（GORM 表映射）。
type CartItem struct {
	ID     int64
	UserID int64
	SKUID  int64
}

// repository：数据访问。
type repo struct{ items []CartItem }

func (r *repo) ListByUser(uid int64) ([]CartItem, error) {
	return r.items, nil
}

// service：业务规则（校验/编排/事务边界）。
type service struct{ repo *repo }

func (s *service) ListCart(uid int64) ([]CartItem, error) {
	if uid <= 0 {
		return nil, errors.New("非法用户")
	}
	return s.repo.ListByUser(uid)
}

// handler：HTTP 出入参、状态码（Gin）。
type handler struct{ svc *service }

func (h *handler) GET(uid int64) {
	items, err := h.svc.ListCart(uid)
	if err != nil {
		fmt.Println("HTTP 400:", err)
		return
	}
	fmt.Println("HTTP 200:", items)
}

func main() {
	h := handler{svc: &service{repo: &repo{items: []CartItem{{ID: 1, UserID: 7, SKUID: 2}}}}}
	h.GET(7)
	h.GET(0)
	fmt.Println("各层只依赖相邻层：handler 不直接碰 DB，repo 不做业务判断")
}

```

**项目位置**：`internal/cart/{handler,service,repository,model}` 即此四层，全项目模块同构；跨模块只能经 service 接口。

## Q5. 结构化日志：JSON + 字段化打点

**答案要点**

- 结构化日志 = 机器可读键值对（JSON），可被日志系统过滤/聚合/告警。
- 字段要"查询友好"：统一命名（activity_id/order_no），一查一个准。
- 访问日志单独中间件：method/route/status/duration。
- 降级/告警路径要单独可观测（可 grep 关键词）。
- 不要拼字符串日志、不要打敏感信息（密码/token）。

**可运行代码**

```go title="interview/ch09_engineering/q05_logging/main.go"
package main

import (
	"log/slog"
	"os"
)

func main() {
	// 项目主日志用 zap（JSON 到 stdout，可镜像到文件供 Loki 采集）；
	// 此处用 stdlib slog 演示同款结构化风格（product 模块实际混用了 slog）。
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// 业务日志：机器可读的键值对，而非拼字符串。
	logger.Info("秒杀预扣成功",
		"activity_id", 1001,
		"user_id", 7,
		"order_no", "O20260813001",
		"redis_stock_left", 49,
	)

	// 访问日志（项目 requestLogger 中间件）: method/route/status/duration。
	logger.Info("http request",
		"method", "POST",
		"route", "/api/flashsales/:id/purchase",
		"status", 202,
		"duration_ms", 3.2,
	)
}

```

**项目位置**：`internal/platform/logger/logger.go`（zap JSON + `log.file` 镜像 + 自动建目录）；访问日志 `cmd/server/middleware.go` 的 `requestLogger`；product 模块降级告警用 slog（`product_service.go`）；采集链 promtail → Loki。

## Q6. 表驱动测试与假对象注入

**答案要点**

- 表驱动：输入 + 期望一张表，逐行 `t.Run` 子测试，失败定位清晰。
- 假对象注入：fake 实现接口替换仓储/缓存，单测不连真实中间件。
- 集成测试连真实 MySQL/Redis（compose 起依赖、Redis 分库隔离）。
- 断言库 testify/require；错误用 `errors.Is` 断言语义而非字符串。

**可运行代码**

```go title="interview/ch09_engineering/q06_testing/main.go"
package main

import (
	"errors"
	"fmt"
)

// 被测函数：把仓储错误翻译成业务错误（对应 translateProductError 的简化版）。
func placeOrder(engine Engine) error {
	if err := engine.CreateSeckill(); err != nil {
		if errors.Is(err, ErrSoldOut) {
			return fmt.Errorf("下单失败: %w", ErrSoldOut)
		}
		return fmt.Errorf("下单失败: %w", err)
	}
	return nil
}

var ErrSoldOut = errors.New("已抢光")

type Engine interface{ CreateSeckill() error }

type fakeEngine struct{ err error }

func (f fakeEngine) CreateSeckill() error { return f.err }

// 表驱动用例：输入 + 期望输出一张表。
var cases = []struct {
	name    string
	engine  Engine
	wantErr bool
	wantIs  error
}{
	{"成功", fakeEngine{nil}, false, nil},
	{"库存不足", fakeEngine{ErrSoldOut}, true, ErrSoldOut},
	{"DB 故障", fakeEngine{errors.New("conn refused")}, true, nil},
}

func main() {
	for _, c := range cases {
		err := placeOrder(c.engine)
		fmt.Printf("%-10s → err=%v wantErr=%v\n", c.name, err, c.wantErr)
	}
}

```

配套测试 `main_test.go`（同目录，`go test ./interview/ch09_engineering/q06_testing -v` 运行）。

**项目位置**：手写 fake 替换仓储/缓存（`internal/order/service/order_service_test.go`、`flashsale_consumer_test.go`）；集成测试 `*_integration_test.go`（compose Redis 21 库做包隔离）。

## Q7. 跨模块事务：tx 参数汇入同一事务

**答案要点**

- 跨模块写不能各开各的事务（部分成功 = 数据不一致）。
- 约定：服务接口只传不透明的 `transaction.Handle`，调用方的事务句柄汇入下游；GORM 仅由 adapter 创建和解包。
- 事务内只做 DB 操作：外部 IO（MQ 发布、通知）移出事务，提交后再发。
- 失败即回滚整链：订单 + 库存 + 券 + 购物车原子生效。

**可运行代码**

```go title="interview/ch09_engineering/q07_tx_boundary/main.go"
package main

import (
	"errors"
	"fmt"
)

// 不透明事务句柄（示例实现；业务模块只传递，不执行驱动 API）。
type Tx struct {
	ops []string
	ok  bool
}

func (t *Tx) begin() *Tx { return &Tx{ok: true} }
func (t *Tx) commit()    { t.ok = true }
func (t *Tx) rollback()  { t.ok = false; t.ops = nil }

// 跨模块写都接收 tx：order 调 coupon.UseCoupon(tx)、product.DeductStock(tx)……
type couponModule struct{}

func (couponModule) UseCoupon(tx *Tx) error {
	if tx == nil {
		return errors.New("必须汇入调用方事务！")
	}
	tx.ops = append(tx.ops, "UPDATE user_coupons SET status=used")
	return nil
}

type productModule struct{}

func (productModule) DeductStock(tx *Tx) error {
	tx.ops = append(tx.ops, "UPDATE skus SET stock=stock-1 WHERE stock>=1")
	return nil
}

func main() {
	// 下单事务：订单 + 订单项 + 扣减库存 + 地址快照 + 券核销 + 清购物车。
	tx := (&Tx{}).begin()
	coupon := couponModule{}
	product := productModule{}
	_ = coupon.UseCoupon(tx)
	_ = product.DeductStock(tx)
	tx.ops = append(tx.ops, "INSERT orders ...")
	tx.commit()
	fmt.Printf("事务提交，ops=%d：跨模块写全部原子生效\n", len(tx.ops))
}

```

**项目位置**：`internal/order/repository/order_repository.go` 的 `TxRunner.WithinTx`；服务接口带 tx 参数（`GetSKUForUpdate`/`DeductStock`/`UseCoupon`/`RollbackCoupon`），见 `order_service.go` createOrder 事务体。
