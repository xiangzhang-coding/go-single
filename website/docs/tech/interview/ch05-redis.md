---
sidebar_position: 6
---

# 05 Redis

## Q1. 缓存击穿/穿透/雪崩 与缓存回填

**答案要点**

- **穿透**：查不存在的 key，缓存永远 miss，DB 被打——布隆过滤器/空值缓存。
- **击穿**：热点 key 过期瞬间大量请求直击 DB——单飞（singleflight）/互斥回填。
- **雪崩**：大量 key 同时过期，DB 压力陡增——过期时间加随机抖动。
- 回填时机：miss 后查 DB 写回缓存并设 TTL；并发 miss 应只让一个请求回填。

**可运行代码**

```go title="interview/ch05_redis/q01_cache_breakdown/main.go"
package main

import (
	"fmt"
	"sync"
)

// 缓存 + 回填：击穿是"热点 key 过期瞬间大量请求直击 DB"。
// 本 demo 用"单飞（singleflight）"思想：同 key 只放一个请求去 DB 回填。
type detailCache struct {
	mu       sync.Mutex
	data     map[string]string
	inflight map[string]*sync.WaitGroup
}

func (c *detailCache) getOrLoad(key string, load func() string) string {
	c.mu.Lock()
	if v, ok := c.data[key]; ok {
		c.mu.Unlock()
		return v // 命中缓存
	}
	// 击穿防护：同 key 只有一个回填者，其余等待。
	if wg, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		wg.Wait()
		c.mu.Lock()
		v := c.data[key]
		c.mu.Unlock()
		return v
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.inflight[key] = wg
	c.mu.Unlock()

	v := load() // DB 查询
	c.mu.Lock()
	c.data[key] = v
	delete(c.inflight, key)
	wg.Done()
	c.mu.Unlock()
	return v
}

func main() {
	c := &detailCache{data: map[string]string{}, inflight: map[string]*sync.WaitGroup{}}
	loads := 0
	load := func() string {
		loads++
		return "product-detail"
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ { // 10 并发读同一未命中 key
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.getOrLoad("p1", load) }()
	}
	wg.Wait()
	fmt.Printf("10 个并发请求，DB 只回填 %d 次\n", loads)
	fmt.Println("应对雪崩：过期时间加随机抖动，避免同一时刻集体过期")
}

```

**项目位置**：`internal/product/service/product_service.go` 的 `GetDetail`——缓存 miss 后查 DB 回填（`product:detail:{id}`，TTL 5min）；单飞留作演进（BACKLOG）。

## Q2. 缓存一致性：Cache-Aside 与"删缓存 vs 更新缓存"

**答案要点**

- Cache-Aside（旁路缓存）：读 miss 回填；写 DB 后**删缓存**而非更新缓存。
- 为什么不更新缓存：并发写顺序与回填顺序竞争，旧值可能覆盖新值；删缓存让下次读自然取新值。
- 一致性窗口 = TTL：短 TTL 自愈，长 TTL 更一致但命中率低。
- 秒杀库存特殊：Redis 本身就是事实源（预扣），不适用本模式。

**可运行代码**

```go title="interview/ch05_redis/q02_cache_consistency/main.go"
package main

import (
	"fmt"
	"sync"
)

type store struct {
	mu    sync.Mutex
	db    map[string]string
	cache map[string]string
	log   []string
}

func (s *store) writeThrough(id, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db[id] = v
	// 写库后【删除】缓存而非"更新"缓存：
	// 更新会引入并发写顺序问题，删除则让下一次读自然回填新值。
	delete(s.cache, id)
	s.log = append(s.log, fmt.Sprintf("DB 写入 %s=%s，缓存删除", id, v))
}

func (s *store) read(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cache[id]; ok {
		return v
	}
	v := s.db[id]
	s.cache[id] = v // 回填
	return v
}

func main() {
	s := &store{db: map[string]string{"p1": "旧价"}, cache: map[string]string{"p1": "旧价"}}
	s.writeThrough("p1", "新价") // 管理端改价：先 DB 后删缓存
	fmt.Println("读:", s.read("p1"), "| 日志:", s.log)
	fmt.Println("要点：Cache-Aside 下写路径 = 更新 DB + 删除缓存（而非更新缓存）")
}

```

**项目位置**：product 详情 Cache-Aside（`product_service.go` GetDetail）；秒杀库存 Redis 是事实源；对账回写见 `reconciliation.go`。

## Q3. SETNX 幂等键：防重复提交

**答案要点**

- `SET key v NX EX ttl` 原子完成"存在即拒绝 + 设置过期"。
- 幂等键先于业务执行抢占：并发重复提交只放行第一个。
- **释放时机**：业务成功 → 保留（直到处理完成）；业务拒绝 → 删除（允许重试）；基础设施失败 → 保留（防重复扣）。
- TTL 兜底：进程崩溃也自动过期，不留死键。

**可运行代码**

```go title="interview/ch05_redis/q03_setnx_idem/main.go"
package main

import (
	"errors"
	"fmt"
	"sync"
)

// 内存版 SETNX + EXPIRE（对应项目 cache.AcquireIdempotency 类型化能力）。
type idemStore struct {
	mu   sync.Mutex
	keys map[string]bool
	ttl  map[string]int
}

func (s *idemStore) setnx(key string, ttl int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys[key] {
		return false // 已存在：重复请求
	}
	s.keys[key] = true
	s.ttl[key] = ttl
	return true
}

func (s *idemStore) del(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, key)
	delete(s.ttl, key)
}

func main() {
	idem := &idemStore{keys: map[string]bool{}, ttl: map[string]int{}}
	user, activity := int64(7), int64(1001)
	key := fmt.Sprintf("flashsale:idem:%d:%d", activity, user)

	// 第一次抢占成功；重复提交被挡（409）。
	got := idem.setnx(key, 1800)
	fmt.Println("首次抢占:", got)
	if !idem.setnx(key, 1800) {
		fmt.Println("重复提交 → 409 重复请求（ErrDuplicateRequest）")
	}
	// 业务拒绝（抢光/限购）后释放幂等键，允许窗口内重试。
	idem.del(key)
	fmt.Println("业务拒绝释放后再次抢占:", idem.setnx(key, 1800))
	_ = errors.New("unused")
}

```

**项目位置**：`internal/platform/cache/atomic.go` 的 `AcquireIdempotency`（适配器内部 SETNX+EX）；`internal/flashsale/service/flashsale_service.go` 的 `isBusinessReject` 管理键生命周期；订单幂等键 `order:idem:{user}:{client_request_id}` TTL 15min。

## Q4. Lua 脚本原子性：一次往返 + 不可分割执行

**答案要点**

- Lua 脚本在 Redis **单线程执行**，期间无其他命令插入 → 原子性。
- 一次 `EVAL` 网络往返替代多次 round-trip（预扣 = 校验状态/窗口/库存/限购 + 扣减 + 计数）。
- 返回码协议化：业务方按码映射错误（1/-3/-1/0/-2）。
- 注意事项：脚本只依赖 KEYS/ARGV 参数，不能依赖外部状态（如服务器时间需传入）。

**可运行代码**

```go title="interview/ch05_redis/q04_lua_atomic/main.go"
package main

import "fmt"

// 项目 Redis 适配器脚本（internal/platform/cache/atomic.go）简化。
// Redis 保证脚本整体原子执行：期间不会有其他命令插入。
const preDeductScript = `
-- KEYS[1]=库存key KEYS[2]=用户计数key
-- ARGV[1]=活动状态 ARGV[2]=now ARGV[3]=start ARGV[4]=end
-- ARGV[5]=每人限购 ARGV[6]=数量
if ARGV[1] ~= 'on_sale' then return -3 end
if tonumber(ARGV[2]) < tonumber(ARGV[3]) or tonumber(ARGV[2]) > tonumber(ARGV[4]) then return -1 end
if tonumber(redis.call('get', KEYS[1]) or 0) < tonumber(ARGV[6]) then return 0 end
if tonumber(redis.call('get', KEYS[2]) or 0) + tonumber(ARGV[6]) > tonumber(ARGV[5]) then return -2 end
redis.call('decrby', KEYS[1], ARGV[6])
redis.call('incrby', KEYS[2], ARGV[6])
return 1
`

// 内存"Redis"执行器：演示脚本的语义（真实为 redis.call）。
func eval(stock, count *int64, status string, now, start, end, limit, qty int64) int64 {
	if status != "on_sale" {
		return -3 // 下架
	}
	if now < start || now > end {
		return -1 // 窗口外
	}
	if *stock < qty {
		return 0 // 抢光
	}
	if *count+qty > limit {
		return -2 // 超限购
	}
	*stock -= qty
	*count += qty
	return 1
}

func main() {
	fmt.Println("Lua 脚本（摘要）:")
	fmt.Print(preDeductScript)
	stock, count := int64(5), int64(0)
	code := eval(&stock, &count, "on_sale", 100, 0, 200, 1, 1)
	fmt.Printf("执行结果=%d stock=%d count=%d（校验→扣减→计数全在一个原子步骤内）\n", code, stock, count)
}

```

**项目位置**：脚本文本与整数返回码协议统一封装在 `internal/platform/cache/atomic.go`；业务模块只调用 `AcquireIdempotency` / `ClaimCoupon` / `WarmFlashSaleStock` / `PreDeductFlashSale` / `RestoreFlashSale` / `IncrementFixedWindow` 类型化能力，不能直接 `Eval`。

## Q5. 固定窗口限流（INCR + TTL）

**答案要点**

- 窗口计数：key 不存在 `SET 1 EX`，存在 `INCR`，超限拒绝。
- Lua 原子完成"SET/INCR + 过期"，避免两步竞态。
- 局限：窗口边界突发（第 59s 与第 61s 各放满一批）——滑动窗口/令牌桶补足。
- 单机令牌桶（x/time/rate）+ 分布式计数可组合：先全局后按用户。

**可运行代码**

```go title="interview/ch05_redis/q05_fixed_window/main.go"
package main

import (
	"fmt"
	"sync"
)

// 内存版固定窗口脚本：key 不存在 SET 1 + EXPIRE，存在则 INCR。
// 固定窗口的局限：窗口边界突发（第 59s 与第 61s 各放满一批）。
type windowCounter struct {
	mu     sync.Mutex
	counts map[string]int
	ttl    map[string]int
}

func (w *windowCounter) allow(key string, limit, window int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.counts[key]; !ok {
		w.counts[key] = 1
		w.ttl[key] = window
		return 1 <= limit
	}
	w.counts[key]++
	return w.counts[key] <= limit
}

func main() {
	w := &windowCounter{counts: map[string]int{}, ttl: map[string]int{}}
	key := "flashsale:rl:7" // 用户 7
	for i := 1; i <= 6; i++ {
		fmt.Printf("第 %d 次请求 → 允许=%v\n", i, w.allow(key, 5, 60))
	}
	fmt.Println("第 6 次被拒：固定窗口 60s 内最多 5 次")
}

```

**项目位置**：`internal/platform/cache/atomic.go` 的 `IncrementFixedWindow` 与 `internal/platform/limiter/limiter.go` 的 `RedisCounter.Allow`；调用点 `flashsale_service.go` 的 `Seckill`；演进"Redis 分布式限流"见 BACKLOG。

## Q6. TTL 与过期策略

**答案要点**

- 惰性删除：读时发现过期即删（省资源）；定期删除兜底（active expire cycle）。
- 内存淘汰（LRU/LFU 等）在内存超限时触发，与过期无关——注意别把淘汰当过期。
- 业务给 key 设计 TTL = 自清理 + 一致性窗口：幂等键、库存 key、缓存各有不同 TTL。
- 秒杀库存 TTL = 活动结束时间 + 1h，活动结束自动消失，无需人工清理。

**可运行代码**

```go title="interview/ch05_redis/q06_ttl/main.go"
package main

import (
	"fmt"
	"time"
)

type redisKey struct {
	value   string
	expires time.Time
}

// 惰性删除：读取时发现过期即删（访问才淘汰，省资源）。
func get(k *redisKey) (string, bool) {
	if k == nil {
		return "", false
	}
	if time.Now().After(k.expires) {
		return "", false // 惰性删除 + 返回 miss
	}
	return k.value, true
}

func main() {
	stock := &redisKey{value: "10", expires: time.Now().Add(150 * time.Millisecond)}
	if v, ok := get(stock); ok {
		fmt.Println("未过期:", v)
	}
	time.Sleep(200 * time.Millisecond)
	_, ok := get(stock)
	fmt.Println("过期后读取 → miss（调用方降级到 DB/配置值）:", ok)

	fmt.Println("项目 TTL 设计：")
	fmt.Println("  flashsale:stock:{id}      TTL = 活动结束时间 + 1h（自清理）")
	fmt.Println("  flashsale:idem:{id}:{user} TTL 30min（幂等键）")
	fmt.Println("  order:idem:{user}:{req}    TTL 15min（下单幂等）")
	fmt.Println("  product:detail:{id}        TTL 5min（详情缓存）")
}

```

**项目位置**：`internal/flashsale/service/flashsale_service.go` 的 `remainingTTL`；幂等键 TTL 见 Seckill 流程；`product:detail` TTL 在 `internal/product/service`。

## Q7. 缓存降级：缓存故障不影响主流程

**答案要点**

- 缓存是优化不是依赖：读失败/miss 必须能回落到真实数据源。
- 降级设计：命中返回缓存；miss → DB；DB 也挂 → 默认值/空值 + 告警。
- 降级要**打日志/打点**（可见性），否则故障静默。
- 秒杀页剩余库存读 Redis 降级配置库存：页面可用，只是数字不实时。

**可运行代码**

```go title="interview/ch05_redis/q07_degrade/main.go"
package main

import (
	"errors"
	"fmt"
)

var ErrCacheMiss = errors.New("cache miss")

type cache interface {
	Get(key string) (int, error)
}

type redisCache struct {
	down bool // 模拟 Redis 故障
}

func (r redisCache) Get(key string) (int, error) {
	if r.down {
		return 0, errors.New("connection refused") // 基础设施故障
	}
	return 0, ErrCacheMiss
}

// 秒杀页剩余库存：缓存读失败/缺失 → 降级返回配置库存，页面照常展示。
func listActivityStock(c cache, configured int) int {
	left, err := c.Get("flashsale:stock:1001")
	if err != nil {
		fmt.Println("降级日志：", err, "→ 使用配置库存")
		return configured
	}
	return left
}

func main() {
	// 正常：缓存有数据。
	normal := listActivityStock(redisCache{down: false}, 100)
	fmt.Println("正常路径剩余库存:", normal)

	// Redis 挂：不报错，返回配置库存（秒杀页仍可浏览，只是数字不实时）。
	degraded := listActivityStock(redisCache{down: true}, 100)
	fmt.Println("降级路径剩余库存:", degraded)
}

```

**项目位置**：`internal/flashsale/service/flashsale_service.go` 的 `ListUserActivities`（降级配置库存 379-385）；product `GetDetail` 缓存 miss 回填、读失败直查 DB（`product_service.go`，slog 降级日志）。
