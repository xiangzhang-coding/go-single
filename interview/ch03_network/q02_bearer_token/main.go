// Q2 Bearer Token 解析与鉴权中间件。
// 运行：go run ./interview/ch03_network/q02_bearer_token
package main

import (
	"errors"
	"fmt"
	"strings"
)

func bearerToken(header string) (string, error) {
	// Authorization: Bearer <token>；要求恰好两部分且前缀不区分大小写。
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("缺失或不合法 Authorization 头")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errors.New("token 为空")
	}
	return token, nil
}

func main() {
	for _, h := range []string{"Bearer abc.def.ghi", "bearer token2", "", "Basic xyz", "Bearer "} {
		t, err := bearerToken(h)
		if err != nil {
			fmt.Printf("header %-18q → 401 %v\n", h, err)
			continue
		}
		fmt.Printf("header %-18q → 通过，token=%s\n", h, t)
	}
}

// 项目位置：internal/platform/auth/middleware.go 的 bearerToken 与
// Middleware（解析失败 401）；token 校验 Verify 在 internal/platform/auth/jwt.go。
