// Q6 迁移工具：版本化 schema 变更（golang-migrate）。
// 运行：go run ./interview/ch10_deploy/q06_migrations
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

// 项目位置：migrations/000001_... ~ 000014_seckill_repurchase（SQL 文件成对 up/down）；
// 启动执行 runMigrations（cmd/server/main.go 197-209）；000014 是"取消后可再次抢购"
// 的 nullable 唯一键改造（uk_orders_user_activity_key）。
