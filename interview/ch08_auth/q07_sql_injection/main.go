// Q7 SQL 注入：拼接 vs 参数化。
// 运行：go run ./interview/ch08_auth/q07_sql_injection
package main

import (
	"fmt"
	"strings"
)

// 反例：用户输入直接拼 SQL —— ' OR '1'='1 让 WHERE 恒真。
func vulnerable(userID string) string {
	return "SELECT * FROM orders WHERE user_id = '" + userID + "'"
}

// 正例：参数化占位符，值经驱动转义（GORM/ database/sql 均如此）。
func safe(userID string) string {
	return "SELECT * FROM orders WHERE user_id = ?" // 参数 ? 绑定，而非拼接
}

func main() {
	input := "' OR '1'='1"
	q1 := vulnerable(input)
	fmt.Println("拼接 SQL:", q1)
	fmt.Println("→ 所有订单被拖走:", strings.Contains(q1, "'1'='1"))

	q2 := safe(input)
	fmt.Println("参数化 SQL:", q2, "（值作为参数传给驱动，天然免疫）")

	// 项目实践：全量 GORM（参数化） + 无任何字符串拼 SQL 的仓储。
	fmt.Println("补充面：XSS（前端转义/受控 React 渲染）、CSRF（Bearer 头不在 cookie，风险低）")
}

// 项目位置：所有仓储走 GORM 参数化查询（internal/*/repository/order_repository_gorm.go 等）；
// 无原生 SQL 拼接；前端统一请求带 Authorization 头而非 cookie（CORS 白名单收紧）。
