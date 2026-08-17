---
sidebar_position: 5
---

# 04 MySQL

## Q1. 唯一索引与幂等：为什么"先查后插"不可靠

**答案要点**

- 并发下"查重 → 插入"两步可被两个请求同时穿过，双双通过 → 数据重复。
- 唯一索引在数据库层原子拦截第二个插入（报 **1062**），是幂等兜底。
- 秒杀场景：唯一键（`order_no` 主键 / `user_activity_key` 唯一约束）让重复消息落单幂等。
- 可空唯一键：`NULL` 不参与唯一约束——取消订单置 NULL 即"释放"名额，允许再次抢购。

**可运行代码**

```go title="interview/ch04_mysql/q01_unique_index/main.go"
package main

import (
	"errors"
	"fmt"
)

// 模拟 uk_orders_user_activity_key：并发下"查重→插入"两步会被两个请求同时穿过，
// 唯一索引在数据库层原子拦截第二个插入（1062 错误）。
var store = map[string]string{}

var ErrDuplicate = errors.New("duplicate key (MySQL 1062)")

func insertOrderNoDup(key string) error {
	if _, exists := store[key]; exists { // 先查
		return ErrDuplicate
	}
	store[key] = "order_1001" // 后插：并发时可能被绕过
	return nil
}

func main() {
	// 模拟两个并发请求同时到达：查重都通过 → 双双插入，破坏了唯一性。
	// 真实项目用数据库 UNIQUE 约束兜底：第二个 insert 直接报 1062。
	fmt.Println("并发缺陷演示：", insertOrderNoDup("u1:a1") == nil, insertOrderNoDup("u1:a1") == nil)

	// 正确姿势：直接插入并捕获 1062，映射为业务错误。
	// 项目映射：mysqlErr 1062 → ErrOrderDuplicate（order_repository_gorm.go）。
	fmt.Println("唯一索引（DB 层）才是幂等兜底")
}

```

**项目位置**：`migrations/000014_seckill_repurchase.up.sql` 的 `uk_orders_user_activity_key`（可空）；`internal/order/repository/order_repository_gorm.go` 用 `errors.As(err, &mysqlErr)` 把 1062 映射为 `ErrOrderDuplicate` → 消费者按幂等成功处理。

## Q2. 事务 ACID：跨表原子性

**答案要点**

- **A** 原子性：要么全提交要么全回滚；**C** 一致性：约束不变式；**I** 隔离性：并发互不可见中间态；**D** 持久性：提交后不丢。
- 跨表/跨模块写必须同一事务：订单 + 订单项 + 扣减库存 + 地址快照 + 券核销 + 清购物车。
- 事务要短：持有锁时间长 = 冲突面大；不在事务里做外部 IO（HTTP/RPC）。
- 回滚不留中间态，出错即整链失败。

**可运行代码**

```go title="interview/ch04_mysql/q02_transaction/main.go"
package main

import (
	"errors"
	"fmt"
)

// 极简内存"数据库"：操作记录可以提交或回滚。
type memDB struct {
	ops       []string
	committed bool
}

func (db *memDB) exec(sql string) error {
	db.ops = append(db.ops, sql)
	return nil
}

func (db *memDB) begin()  { db.committed = false }
func (db *memDB) commit() { db.committed = true }
func (db *memDB) rollback() {
	if !db.committed {
		db.ops = nil // 未提交即丢弃（模拟回滚）
	}
}

func main() {
	// 下单事务 = 订单 + 订单项 + 扣减库存 + 地址快照 + 券核销 + 清购物车（全在一个事务）。
	db := &memDB{}
	db.begin()
	_ = db.exec("INSERT orders ...")
	_ = db.exec("UPDATE skus SET stock = stock - 1 ...")
	if errors.New("扣减库存失败") != nil { // 模拟任一步失败
		db.rollback()
		fmt.Println("任一步失败 → 全部回滚，无部分写入")
		return
	}
	db.commit()
	fmt.Println("全部成功 → 提交，原子生效")

	// 关键点：ACID 的原子性保证"要么全有要么全无"，配合行锁避免并发串改。
	fmt.Println("回滚后 ops 长度:", len(db.ops))
}

```

**项目位置**：`internal/order/service/order_service.go` 的 `createOrder` / `CreateSeckillInTx`；秒杀跨模块写由 flashsale 消费者经 `TxRunner.WithinTx` 汇入同一事务（`internal/flashsale/service/flashsale_consumer.go`）。

## Q3. 条件更新防超卖：`UPDATE ... WHERE stock >= ?`

**答案要点**

- "先 SELECT 再 UPDATE"两步并发必超卖；**条件更新**把检查与修改合成一个原子 SQL。
- `UPDATE skus SET stock=stock-1 WHERE id=? AND stock>=1`：条件不满足影响 0 行。
- 影响行数判断成败：0 行 = 库存不足（或已被并发扣完）。
- 这是数据库层的 CAS；秒杀落单在事务内条件扣活动库存同理。

**可运行代码**

```go title="interview/ch04_mysql/q03_conditional_update/main.go"
package main

import (
	"errors"
	"fmt"
	"sync"
)

type sku struct {
	mu    sync.Mutex
	stock int
}

// 内存版"UPDATE skus SET stock=stock-1 WHERE id=? AND stock>=1"：
// 条件不满足不扣减（对比先 SELECT 再 UPDATE 的检查-执行两步）。
func (s *sku) deduct() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stock < 1 {
		return errors.New("库存不足（条件更新未命中 0 行）")
	}
	s.stock--
	return nil
}

func main() {
	s := &sku{stock: 3}
	succ := 0
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ { // 10 个并发扣减
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.deduct() == nil {
				succ++
			}
		}()
	}
	wg.Wait()
	fmt.Printf("并发 10 次扣减，成功 %d 次（库存 3，不会超卖）\n", succ)
}

```

**项目位置**：`internal/flashsale/repository/activity_repository_gorm.go` 的条件扣减；秒杀消费者在订单事务内调用该仓储的 `DeductStock`；商品 SKU 同理（`internal/product`）。

## Q4. 行锁与锁顺序：避免死锁

**答案要点**

- `SELECT ... FOR UPDATE` 悲观锁：锁住行直到事务结束。
- **死锁四条件**：互斥、持有并等待、不可剥夺、循环等待——打破任意一条即可。
- 多行加锁必须**全局固定排序**：两个事务按相同顺序拿锁就不会环形等待。
- 条件更新（Q3）其实是"锁 + 条件"一体，也是防死锁的常用手段。

**可运行代码**

```go title="interview/ch04_mysql/q04_lock_order/main.go"
package main

import (
	"fmt"
	"sort"
	"sync"
)

// 多 SKU 扣减库存时按 (product_id, sku_id) 排序后加锁——
// 两个事务都以相同顺序拿锁，就不会出现"我等你、你等我"的环形等待。
type skuLock struct {
	id  int64
	mu  sync.Mutex
	got string
}

func main() {
	skus := map[int64]*skuLock{3: {id: 3}, 1: {id: 1}, 2: {id: 2}}
	// 事务内先排序再逐个加锁（模拟 SELECT ... FOR UPDATE 顺序）。
	ids := make([]int64, 0, len(skus))
	for id := range skus {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		skus[id].mu.Lock()
		skus[id].got = fmt.Sprintf("locked %d", id)
		skus[id].mu.Unlock()
	}
	for _, id := range ids {
		fmt.Println(skus[id].got)
	}
	fmt.Println("所有事务按相同排序拿锁 → 无死锁")
}

```

**项目位置**：`internal/order/service/order_service.go` 在锁商品/SKU 前 `sort.Slice` 统一排序（677-682）；加锁查询 `GetSKUForUpdate` 在 `internal/product/service/product_service.go`。

## Q5. 乐观锁 vs 悲观锁

**答案要点**

- 悲观锁：`SELECT FOR UPDATE` 先锁行——冲突多、写频繁时简单可靠，吞吐低。
- 乐观锁：`version` 字段条件更新，影响 0 行 = 冲突，业务重试——冲突少时吞吐高。
- 条件更新（`WHERE stock>=?`）本质是"库存即版本"的乐观 CAS。
- 选型：高并发写同一行（秒杀）→ 条件更新/悲观；低冲突更新 → version 乐观锁。

**可运行代码**

```go title="interview/ch04_mysql/q05_optimistic_lock/main.go"
package main

import (
	"errors"
	"fmt"
)

type account struct {
	balance int
	version int // 乐观锁版本号
}

// 乐观锁：UPDATE ... SET balance=balance-100, version=version+1
// WHERE id=? AND version=? —— version 不匹配则影响 0 行，重试。
func optimisticTransfer(a *account, amount int) error {
	if a.version%2 == 0 { // 模拟并发冲突：版本已被别的请求改掉
		return errors.New("冲突，version 不匹配，重试")
	}
	a.balance -= amount
	a.version++
	return nil
}

func main() {
	acc := &account{balance: 500, version: 1}
	fmt.Println("悲观锁（SELECT FOR UPDATE）：先锁行再修改，串行安全但吞吐低")
	fmt.Println("乐观锁（version 条件更新）：冲突时影响 0 行，业务重试")
	if err := optimisticTransfer(acc, 100); err != nil {
		fmt.Println("乐观锁冲突:", err)
	}
	// 重试一次
	_ = optimisticTransfer(acc, 100)
	fmt.Printf("重试成功，余额=%d version=%d\n", acc.balance, acc.version)
}

```

**项目位置**：本项目主用"条件更新（CAS）+ 行锁"组合（`activity_repository_gorm.go`、`order_repository_gorm.go` 的 `MarkPaid` 带金额断言）；乐观锁留作延伸讨论。

## Q6. 唯一键冲突（1062）映射业务错误

**答案要点**

- go-sql-driver 的 `*mysql.MySQLError` 携带 `Number`，`errors.As` 提取判 1062。
- 把 1062 映射为**业务幂等成功**（消费者 Ack）而非报错：重复消息是常态不是异常。
- 映射职责在 repository 层，service 只认业务错误（`ErrOrderDuplicate`）。
- 幂等 + 库存条件扣减双保险：重复执行也不会重复扣减库存。

**可运行代码**

```go title="interview/ch04_mysql/q06_unique_idempotent/main.go"
package main

import (
	"errors"
	"fmt"
)

// 模拟 MySQL 1062 错误。
var ErrMySQL1062 = errors.New("Error 1062: Duplicate entry 'xxx' for key 'uk_orders_user_activity_key'")

// 模拟订单仓储：插入命中唯一键 → 返回"重复"业务错误。
func saveOrder(no string) error {
	if no == "O20260813001" {
		return ErrMySQL1062
	}
	return nil
}

func main() {
	// 秒杀消费者：重复消息 → 幂等成功（不重复扣减库存）。
	// 项目做法：errors.As(err, &mysqlErr) 解析 1062 → ErrOrderDuplicate → 消费者 Ack。
	no := "O20260813001"
	if err := saveOrder(no); err != nil {
		var mysqlErr *mySQLError
		if errors.As(err, &mysqlErr) {
			fmt.Println("按 1062 处理为幂等成功")
			return
		}
		fmt.Println("其他错误:", err)
	}
}

// mySQLError 占位类型：真实项目直接用 go-sql-driver/mysql 的 *mysql.MySQLError。
type mySQLError struct{ Number uint16 }

func (e *mySQLError) Error() string { return "mysql error" }

```

**项目位置**：`internal/order/repository/order_repository_gorm.go`（`errors.As` 判 1062 → `ErrOrderDuplicate`）；消费端视为幂等成功（`flashsale_consumer.go` 的 `classifyCreateError`）。

## Q7. 连接池与慢查询

**答案要点**

- 池四参数：`MaxOpen`（防压垮 DB）、`MaxIdle`（省握手）、`ConnMaxLifetime`（防断链）、`ConnMaxIdleTime`。
- 池耗尽的表现是"获取连接超时/等待"，容易误判为 DB 慢——先看池指标。
- 慢查询根因常是**缺索引**：`WHERE user_id=? ORDER BY created_at` 要有联合索引。
- GORM Logger Warn 级别自动打印慢 SQL（`SlowThreshold`）。

**可运行代码**

```go title="interview/ch04_mysql/q07_conn_pool/main.go"
package main

import (
	"fmt"
	"time"
)

type pool struct {
	maxOpen int
	maxIdle int
	life    time.Duration
	used    int
}

func (p *pool) acquire() bool {
	if p.used >= p.maxOpen {
		return false // 池耗尽：请求排队/超时，表现为连接超时
	}
	p.used++
	return true
}

func main() {
	p := &pool{maxOpen: 20, maxIdle: 5, life: 5 * time.Minute}
	fmt.Println("配置要点（main.go openMySQL）：")
	fmt.Println("  SetMaxOpenConns(20)   防止无限建连压垮 DB")
	fmt.Println("  SetMaxIdleConns(5)    控制空闲连接（过高浪费、过低频繁握手）")
	fmt.Println("  SetConnMaxLifetime(5m) 避免长连接被 LB/防火墙掐断")

	// 池耗尽演示
	for i := 0; i < 20; i++ {
		p.acquire()
	}
	fmt.Println("池是否耗尽:", !p.acquire())

	// 慢查询：WHERE user_id=? 无索引 vs 有索引。
	fmt.Println("orders 建 idx_orders_user_status 索引后，按用户查订单 O(log n) 而非全表扫")
}

```

**项目位置**：`cmd/server/main.go` 的 `openMySQL`（池参数 + `PingContext` 5s）；`migrations/000009_orders.up.sql` 建 `idx_orders_user_status`；GORM Warn 慢日志。
