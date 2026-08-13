// Q5 文件上传安全：扩展名不可信，用魔数嗅探真实类型。
// 运行：go run ./interview/ch08_auth/q05_upload_magic
package main

import (
	"bytes"
	"fmt"
)

// 常见图片魔数：文件头字节序列（项目用 mimetype.Detect，语义相同）。
func sniff(buf []byte) string {
	switch {
	case len(buf) >= 8 && bytes.Equal(buf[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(buf) >= 3 && bytes.Equal(buf[:3], []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case len(buf) >= 6 && bytes.Equal(buf[:6], []byte("GIF87a")) || len(buf) >= 6 && bytes.Equal(buf[:6], []byte("GIF89a")):
		return "image/gif"
	default:
		return ""
	}
}

func main() {
	// 攻击者把 exe 改名成 .png 上传 → 魔数嗅探识破。
	evil := append([]byte("MZ..."), 0) // exe 文件头
	fakePNG := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00}
	real := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	for name, b := range map[string][]byte{"改名的 exe": evil, "伪 PNG": fakePNG, "真 JPEG": real} {
		if sniff(b) == "" {
			fmt.Printf("%-10s → 拒绝（非白名单类型）\n", name)
		} else {
			fmt.Printf("%-10s → 允许（%s）\n", name, sniff(b))
		}
	}
}

// 项目位置：internal/platform/file/file.go 的 validate——mimetype.Detect 魔数嗅探 +
// 白名单 png/jpeg/webp/gif + ≤5MB 限制；handler 在 internal/platform/file/handler.go
//（POST /api/files）；MinIO 桶私有化 ensurePrivate（minio.go）。
