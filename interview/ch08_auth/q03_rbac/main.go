// Q3 RBAC 角色权限：普通用户 vs 管理员。
// 运行：go run ./interview/ch08_auth/q03_rbac
package main

import (
	"errors"
	"fmt"
)

const (
	roleUser  = "user"
	roleAdmin = "admin"
)

// 简化版 RequireAdmin 中间件逻辑。
func requireAdmin(role string) error {
	if role != roleAdmin {
		return errors.New("403 Forbidden：非管理员")
	}
	return nil
}

func main() {
	for _, role := range []string{roleUser, roleAdmin} {
		canPublish := requireAdmin(role) == nil
		fmt.Printf("角色 %-5s 可上架秒杀活动: %v\n", role, canPublish)
	}

	fmt.Println()
	fmt.Println("管理面路由（Bearer + RequireAdmin）:")
	fmt.Println("  /api/admin/flashsales  /api/admin/products  /api/admin/orders  /api/admin/coupons")
	fmt.Println("管理端 token 来自种子账号 admin/admin123（migrations/000002_users）")
}

// 项目位置：internal/platform/auth/middleware.go 的 RequireAdmin（43-56）；
// 各模块 handler 以 rg.Group("/admin", auth.Middleware(...), auth.RequireAdmin())
// 装配管理路由（flashsale/product/order/coupon handler）。
