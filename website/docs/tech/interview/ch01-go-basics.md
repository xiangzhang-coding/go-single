---
sidebar_position: 2
---

# 01 Go 基础

## Q1. slice 的扩容机制与底层数组

**答案要点**

- `append` 触发扩容时容量增长策略：`cap < 1024` 约翻倍，`>= 1024` 约 1.25 倍（实现细节，不保证）。
- 子切片与父切片**共享底层数组**：修改子切片元素会影响父切片。
- 扩容分配新数组后**不再共享**；`copy` 可显式复制，避免共享副作用。
- `len` 与 `cap` 是运行时属性；函数传 slice 传的是头（指针/len/cap），共享同一数组。

**可运行代码**

```go title="interview/ch01_go_basics/q01_slice_growth/main.go"
package main

import "fmt"

func main() {
	// append 触发扩容时，cap 增长策略：<1024 翻倍，>=1024 约 1.25 倍（不保证，是实现细节）。
	var s []int
	for i := 0; i < 5; i++ {
		s = append(s, i)
		fmt.Printf("len=%d cap=%d\n", len(s), cap(s))
	}

	// 子切片共享底层数组：对 s2 的修改会影响 s。
	s2 := s[:2]
	s2[0] = 100
	fmt.Println("共享底层数组:", s[0] == 100)

	// 扩容后不再共享：append 超过 cap 会分配新数组。
	s3 := append(s, 99)
	s3[0] = -1
	fmt.Println("扩容后不共享:", s[0] == 100)
}
```

**项目位置**：slice 承载列表型数据（订单项、购物车条目等）；`internal/cart/service` 的条目列表、`internal/order/service` 的 `order_items []model.OrderItem` 均为典型用法。

## Q2. map 的并发安全与正确姿势

**答案要点**

- map 本身**非并发安全**：并发写触发 `fatal error: concurrent map writes`，进程直接崩溃。
- 三种姿势：`sync.Mutex`/`sync.RWMutex` 包裹；`sync.Map`（键集稳定、读多写少、或 per-key 独立更新的场景）；分片 map。
- 只读并发安全：不写就无锁安全；并发读 + 写即使不崩溃也可能读到中间状态。
- 面试延伸：`sync.Map` 底层是"读写分离 + 原子指针 + 摊还锁"，不适合通用场景。

**可运行代码**

```go title="interview/ch01_go_basics/q02_map_concurrency/main.go"
package main

import (
	"fmt"
	"sync"
)

func main() {
	// 错误姿势：并发写 map 会触发 fatal error: concurrent map writes（进程崩溃）。
	// 正确姿势一：互斥锁保护。
	var mu sync.RWMutex
	counts := map[string]int{}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mu.Lock()
				counts["hit"]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Println("互斥锁保护后的计数:", counts["hit"])

	// 正确姿势二：sync.Map（读多写少或键集固定的场景）。
	var hits sync.Map
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := hits.LoadOrStore("hit", 0)
			hits.Store("hit", v.(int)+1)
		}()
	}
	wg.Wait()
	v, _ := hits.Load("hit")
	fmt.Println("sync.Map 计数:", v)
}
```

**项目位置**：`internal/platform/ws/hub.go` 的 `Hub` 用 `sync.RWMutex` 保护 `clients map[int64]map[*client]struct{}`（写少读多）；高并发计数走 Prometheus Counter（自带原子），见 `internal/platform/metrics`。

## Q3. defer 执行时机与命名返回值陷阱

**答案要点**

- defer 在**函数返回时**执行，LIFO（后注册先执行）。
- 参数在注册时求值；闭包捕获变量在返回时取值。
- **命名返回值陷阱**：`return n` 先把 n 赋给返回值，defer 可继续修改该返回值；匿名返回值在 return 语句已确定，defer 修改无效。
- 用途：资源释放（锁/连接/文件）、recover、记录耗时。

**可运行代码**

```go title="interview/ch01_go_basics/q03_defer_return/main.go"
package main

import "fmt"

func order() int {
	defer fmt.Println("先 defer 的后执行")
	defer fmt.Println("后 defer 的先执行")
	return 1
}

// 命名返回值陷阱：defer 可以修改返回值。
func named() (n int) {
	n = 10
	defer func() { n++ }() // 返回前执行，n 变成 11
	return n
}

func anonymous() int {
	n := 10
	defer func() { n++ }() // 只改局部变量，返回值已在 return 时确定
	return n
}

func main() {
	order()
	fmt.Println("命名返回值 defer 生效:", named())
	fmt.Println("匿名返回值 defer 不生效:", anonymous())
}
```

**项目位置**：`internal/platform/mq/rabbitmq.go` 的 `Publish`/`Consume` 内 `defer ch.Close()`；WS `client.send` 通道关闭由 defer 保证（`internal/platform/ws/hub.go`）。

## Q4. 接口的鸭子类型与动态派发

**答案要点**

- Go 接口是隐式实现（鸭子类型）：类型实现接口的全部方法即满足，无需声明。
- 接口包含两个词（类型 + 值指针）：`nil` 接口判等要小心"非 nil 指针装进接口 ≠ nil"。
- 接口由**使用方定义**（调用方声明最小接口），是模块解耦的核心。
- 空接口 `any` 接收任意值，取值需类型断言 `v.(T)` / `switch v.(type)`。
- 编译期断言 `var _ I = (*T)(nil)` 验证实现。

**可运行代码**

```go title="interview/ch01_go_basics/q04_interface/main.go"
package main

import "fmt"

// 接口由使用方定义（调用方声明最小接口）：消费者只需自己需要的子集。
type StockSource interface {
	Remaining() int
}

type redisStock struct{ remaining int }

func (r redisStock) Remaining() int { return r.remaining }

// 空接口接收任意值，取值时需要类型断言。
func describe(v any) string {
	switch t := v.(type) {
	case int:
		return fmt.Sprintf("int:%d", t)
	case string:
		return fmt.Sprintf("string:%q", t)
	default:
		return "other"
	}
}

func main() {
	var src StockSource = redisStock{remaining: 42}
	fmt.Println("接口动态派发:", src.Remaining())

	var v any = "hello"
	if s, ok := v.(string); ok {
		fmt.Println("类型断言成功:", s)
	}
	fmt.Println(describe(123), describe("hi"), describe(1.5))
}
```

**项目位置**：order 侧声明最小接口 `ActivityStock`/`SeckillRestore`（`internal/order/service/order_service.go:119-133`），由 flashsale 实现；`var _ Repository = (*GORMRepo)(nil)` 断言遍布各模块 repository；ADR-0003 记录该约定。

## Q5. 结构体标签与 JSON 序列化

**答案要点**

- `json` 标签控制字段名/行为：`json:"-"` 忽略、`omitempty` 空值省略、`string` 数字转字符串。
- `json.RawMessage` 保存原始 JSON 字节：不需要解析结构时透传，防重复编解码与精度损失。
- `json.Valid` 校验 JSON 合法性；未知字段默认忽略（`DisallowUnknownFields` 可开启严格模式）。
- `gorm` 标签同时存在：一个结构体承载 DB 与 API 两种契约。

**可运行代码**

```go title="interview/ch01_go_basics/q05_json_tags/main.go"
package main

import (
	"encoding/json"
	"fmt"
)

// specs 用 json.RawMessage 保存原始 JSON（SKU 规格快照，不解析结构只透传）。
type SKU struct {
	ID     int64           `json:"id"`
	Title  string          `json:"title"`
	Specs  json.RawMessage `json:"specs"`
	Hidden string          `json:"-"`
	Note   string          `json:"note,omitempty"` // 空值不输出
}

func main() {
	sku := SKU{ID: 1, Title: "红色 M 码", Specs: json.RawMessage(`{"color":"red"}`), Hidden: "secret"}
	b, _ := json.Marshal(sku)
	fmt.Println("序列化（Hidden 被忽略、omitempty 生效）:", string(b))

	var back SKU
	_ = json.Unmarshal(b, &back)
	fmt.Println("RawMessage 透传:", string(back.Specs))

	var out []byte
	if json.Valid(back.Specs) {
		out = append(out, back.Specs...)
	}
	fmt.Println("json.Valid 校验合法:", string(out))
}
```

**项目位置**：`internal/product/model/product.go` 的 `SKU.Specs` 与 `internal/order/model/order.go` 订单项快照均用 `json.RawMessage`；`internal/product/service/product_service.go` 的 `validateSKU` 用 `json.Valid` 校验规格。

## Q6. string 与 []byte、UTF-8 与 rune

**答案要点**

- `len(string)` 是**字节数**不是字符数；`utf8.RuneCountInString` 数字符。
- `for range` 按 rune 迭代自动解码 UTF-8；越界截断用 `[]rune(s)[:n]` 转换（有拷贝）。
- `string` ↔ `[]byte` 互转会复制；高频场景可考虑 `unsafe`（不推荐业务用）。
- 中文三字节/emoji 四字节：按字节切片可能切坏多字节字符。

**可运行代码**

```go title="interview/ch01_go_basics/q06_string_rune/main.go"
package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

func main() {
	s := "秒杀商品FlashSale"

	// len() 是字节数，不是字符数。
	fmt.Println("len 字节数:", len(s))
	fmt.Println("rune 字符数:", utf8.RuneCountInString(s))

	// 按 rune 遍历。
	var chars []string
	for _, r := range s {
		chars = append(chars, string(r))
	}
	fmt.Println("逐字遍历:", strings.Join(chars, " "))

	// 字节切片与 string 互相转换会复制（小字符串开销可忽略）。
	b := []byte(s)
	back := string(b)
	fmt.Println("[]byte 往返一致:", back == s)

	// 截断字符串按字符而不是字节（避免切坏 UTF-8）。
	truncated := string([]rune(s)[:4])
	fmt.Println("按字符截断:", truncated)
}
```

**项目位置**：`internal/chat/service/message_service.go` 的 `validateMessage` 用 `utf8.RuneCountInString`；`internal/user/service/user_service.go` 的用户名长度校验、`phoneRe` 正则。

## Q7. 错误处理：哨兵错误、%w 与 errors.Is/As

**答案要点**

- 哨兵错误 `var ErrXxx = errors.New(...)` 做错误契约；服务返回业务错误，handler 映射状态码。
- `fmt.Errorf("%w: ...", err)` 包装保留错误链；不用 `%w` 则链断裂。
- `errors.Is` 沿链匹配哨兵；`errors.As` 提取链上特定类型。
- 跨模块翻译错误（`translateXxxError`）保持依赖方向与错误语义清晰。
- 不可重试错误可用 `retry.Stop` 标记（见容错章节）。

**可运行代码**

```go title="interview/ch01_go_basics/q07_error_handling/main.go"
package main

import (
	"errors"
	"fmt"
)

var ErrSoldOut = errors.New("活动已抢光")
var ErrInvalidInput = errors.New("参数不合法")

// 包装错误：%w 保留原错误链。
func buy() error {
	return fmt.Errorf("%w: 剩余库存 0", ErrSoldOut)
}

// 业务拒绝可加上下文后继续包装。
func placeOrder() error {
	if err := buy(); err != nil {
		return fmt.Errorf("下单失败: %w", err)
	}
	return nil
}

func main() {
	err := placeOrder()

	fmt.Println("errors.Is 命中哨兵:", errors.Is(err, ErrSoldOut))
	fmt.Println("errors.Is 不命中:", errors.Is(err, ErrInvalidInput))

	// errors.As 取链上特定类型。
	var target interface{ Error() string }
	fmt.Println("errors.As 取出:", errors.As(err, &target))

	// 对比：不用 %w 则链条断裂，Is 返回 false。
	broken := fmt.Errorf("下单失败: %v", ErrSoldOut)
	fmt.Println("未包装无法 Is:", errors.Is(broken, ErrSoldOut))
}
```

**项目位置**：`internal/flashsale/service/flashsale_service.go` 顶部哨兵错误族（`ErrActivityNotFound`/`ErrSoldOut`/`ErrLimitReached`/`ErrDuplicateRequest` 等）；`translateCouponError`/`translateProductError`（`internal/order/service/order_service.go`）；`retry.Stop` 在 `internal/platform/retry/retry.go`。
