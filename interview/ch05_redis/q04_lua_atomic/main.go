// Q4 Lua 脚本原子性：一次网络往返 + 不可分割执行。
// 运行：go run ./interview/ch05_redis/q04_lua_atomic
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

// 项目位置：internal/platform/cache/atomic.go。业务模块只调用类型化原子能力，
// Lua 文本与返回码协议不离开 Redis 适配器。
