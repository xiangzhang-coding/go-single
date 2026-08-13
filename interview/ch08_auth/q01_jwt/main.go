// Q1 JWT 结构与 HMAC-SHA256 签名验证。
// 运行：go run ./interview/ch08_auth/q01_jwt
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 用标准库演示 JWT 的核心机制（项目实际用 golang-jwt/v5，语义相同）。
func sign(payload map[string]any, secret string) string {
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	signing := b64(header) + "." + b64(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

func verify(token, secret string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("格式错误")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), mustB64(parts[2])) {
		return nil, fmt.Errorf("签名不匹配")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	_ = json.Unmarshal(body, &claims)
	return claims, nil
}

func mustB64(s string) []byte {
	b, _ := base64.RawURLEncoding.DecodeString(s)
	return b
}

func main() {
	claims := map[string]any{"user_id": 7, "role": "user", "exp": time.Now().Add(2 * time.Hour).Unix()}
	token := sign(claims, "secret")
	fmt.Println("JWT:", token[:40]+"...")

	got, err := verify(token, "secret")
	fmt.Println("正确密钥验证通过, user_id =", int64(got["user_id"].(float64)), "err =", err)

	_, err = verify(token, "wrong-secret")
	fmt.Println("错误密钥验证失败, err =", err)

	// 防算法混淆：项目 jwt.WithValidMethods 固定 HS256（jwt.go）。
	fmt.Println("要点：token 无状态、可被服务端验签；泄漏=冒充，因此 TTL 2h + 妥善保管")
}

// 项目位置：internal/platform/auth/jwt.go——NewJWT({Secret,TTL})、Issue(userID, role)、
// Verify（WithValidMethods 固定 HS256）；TTL 2h 默认（configs/config.yaml auth.ttl）。
