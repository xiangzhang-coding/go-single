// Q6 string 与 []byte 转换、UTF-8 与 rune 计数。
// 运行：go run ./interview/ch01_go_basics/q06_string_rune
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

// 项目位置：消息文案校验用 utf8.RuneCountInString（internal/chat/service/message_service.go
// validateMessage）；用户名长度校验、phoneRe 正则校验（internal/user/service/user_service.go）。
