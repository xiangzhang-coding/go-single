// Q2 bcrypt 密码哈希：不存明文、慢哈希抗暴力。
// 运行：go run ./interview/ch08_auth/q02_bcrypt
package main

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	password := "admin123"

	// 注册：哈希后落库（cost 默认 10，故意慢 ~50ms，暴力破解代价高）。
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bcrypt 哈希（含盐，长度 60）: %s\n", string(hash))

	// 登录：比对哈希（内置盐解析 + 慢比较）。
	err = bcrypt.CompareHashAndPassword(hash, []byte(password))
	fmt.Println("正确密码比对:", err == nil)

	err = bcrypt.CompareHashAndPassword(hash, []byte("wrong"))
	fmt.Println("错误密码比对:", err != nil)
}

// 项目位置：internal/user/service/user_service.go——Register 用
// GenerateFromPassword、Login 用 CompareHashAndPassword（90、113 行）；
// users 表只存 password_hash 字段（migrations/000002_users，json:"-" 不外泄）。
