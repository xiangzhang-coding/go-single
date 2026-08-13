---
sidebar_position: 9
---

# 08 认证与安全

## Q1. JWT 结构与 HMAC-SHA256 签名验证

**答案要点**

- 三段：`header.payload.signature`，Base64URL 编码，签名 = HMAC(header.payload, secret)。
- 无状态：服务端验签即可，不需要 session 存储；代价是**吊销难** → TTL 短（2h）。
- 防算法混淆：验签时固定允许的算法（`jwt.WithValidMethods`），不接受头部指定。
- claims 含 user_id/role/exp；勿放敏感信息（可解码）。

**可运行代码**

```go title="interview/ch08_auth/q01_jwt/main.go"
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
```

**项目位置**：`internal/platform/auth/jwt.go`——`NewJWT({Secret,TTL})`、`Issue(userID, role)`、`Verify`（`WithValidMethods` 固定 HS256）；TTL 2h 默认（`configs/config.yaml` auth.ttl）。

## Q2. bcrypt 密码哈希

**答案要点**

- 不存明文：哈希 + 内嵌随机盐，同一密码两次哈希不同。
- **故意慢**（cost=10 约 50ms）：暴力破解单价极高；cost 可调平衡安全与性能。
- 比对用 `CompareHashAndPassword`（内置盐解析 + 慢比较），不要自写字符串比较。
- 延伸：argon2 更现代，bcrypt 仍是主流默认。

**可运行代码**

```go title="interview/ch08_auth/q02_bcrypt/main.go"
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
```

**项目位置**：`internal/user/service/user_service.go`——`Register` 用 `GenerateFromPassword`、`Login` 用 `CompareHashAndPassword`（90、113 行）；users 表只存 `password_hash`（`migrations/000002_users.up.sql`，`json:"-"` 不外泄）。

## Q3. RBAC 角色权限

**答案要点**

- 角色放 JWT claims（`role`），中间件按路由声明要求（admin）。
- 管理面独立路由组：`rg.Group("/admin", auth.Middleware(...), auth.RequireAdmin())`。
- 权限判断要集中（中间件），业务代码不散落 if-role。
- 种子管理员：migrations 里 bcrypt 写死 admin/admin123。

**可运行代码**

```go title="interview/ch08_auth/q03_rbac/main.go"
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
```

**项目位置**：`internal/platform/auth/middleware.go` 的 `RequireAdmin`（43-56）；各模块 handler 管理路由组（flashsale/product/order/coupon）。

## Q4. 安全响应头

**答案要点**

- `X-Content-Type-Options: nosniff`：禁 MIME 嗅探（防类型混淆攻击）。
- `X-Frame-Options: DENY`：防点击劫持（frame 内嵌）。
- `Content-Security-Policy`：限制资源来源（self + 白名单），防 XSS 注入外域脚本。
- `Referrer-Policy`：控制 Referer 泄漏。
- nginx 注意：`add_header` 不继承，server 与 location 各写一遍。

**可运行代码**

```go title="interview/ch08_auth/q04_security_headers/main.go"
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
)

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff") // 禁止 MIME 嗅探
		w.Header().Set("X-Frame-Options", "DENY")           // 防点击劫持（frame 内嵌）
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func main() {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	for k, v := range rec.Header() {
		fmt.Printf("%-28s: %s\n", k, v[0])
	}
	fmt.Println("CSP 注意：内联脚本/外域资源需要放行（项目为 minio 图片与 /ws 加了例外）")
}
```

**项目位置**：`deploy/nginx/nginx.conf`——安全头在 server 与 `/assets` 两块重复声明；CSP 放行 self + minio + ws（T26 修订补充）。

## Q5. 文件上传安全：魔数嗅探

**答案要点**

- 扩展名可伪造，**文件头魔数**才是真实类型证据。
- 白名单 + 魔数嗅探（mimetype.Detect）+ 大小限制（≤5MB）。
- 存储桶私有化：不上公网直接访问，走后端鉴权代理。
- 危险点：可执行文件改名上传、图片内嵌脚本（需要输出转义/隔离域）。

**可运行代码**

```go title="interview/ch08_auth/q05_upload_magic/main.go"
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
```

**项目位置**：`internal/platform/file/file.go` 的 `validate`——mimetype.Detect 魔数嗅探 + 白名单 png/jpeg/webp/gif + ≤5MB；handler `internal/platform/file/handler.go`（POST /api/files）；MinIO 桶私有 `ensurePrivate`（minio.go）。

## Q6. 越权防护（IDOR）

**答案要点**

- IDOR：通过改 URL 中的资源 ID 访问他人资源（`/api/orders/O2`）。
- 所有资源访问必须**归属校验**：`owner_id == claims.user_id`。
- 校验收敛成统一 helper（loadOwned/ensureOwned），不要散落 if 判断。
- 错误语义区分：404（不存在，防探测）vs 403（存在但无权）。

**可运行代码**

```go title="interview/ch08_auth/q06_idor/main.go"
package main

import (
	"errors"
	"fmt"
)

type order struct {
	ownerID int64
	no      string
}

var orders = map[string]order{"O1": {ownerID: 7, no: "O1"}, "O2": {ownerID: 8, no: "O2"}}

// 简化版 loadOwned：按订单号取订单，且必须属于当前用户。
func loadOwned(userID int64, orderNo string) (order, error) {
	o, ok := orders[orderNo]
	if !ok {
		return order{}, errors.New("404 订单不存在")
	}
	if o.ownerID != userID {
		return order{}, errors.New("403 越权：订单不属于你")
	}
	return o, nil
}

func main() {
	// 用户 7 访问自己的订单 OK；尝试访问用户 8 的订单被拒。
	if _, err := loadOwned(7, "O1"); err == nil {
		fmt.Println("访问自己的订单 → 200")
	}
	if _, err := loadOwned(7, "O2"); err != nil {
		fmt.Println("访问他人订单 →", err)
	}
}
```

**项目位置**：`internal/order/service/order_service.go` 的 `loadOwned`（1071-1083）；同模式遍布 user（`ensureOwned`）、cart、friend（`ensurePendingOwned`）、chat（`ensureAccessible`）。

## Q7. SQL 注入与 XSS/CSRF 面

**答案要点**

- 注入根因：字符串拼接 SQL；参数化（`?` 占位）由驱动转义，天然免疫。
- 全量 ORM（GORM）+ 无字符串拼 SQL 的仓储 = 项目实践。
- XSS：前端受控渲染/转义，CSP 兜底（见 Q4）。
- CSRF：本项目用 Authorization 头而非 cookie 携带凭证，CSRF 攻击面小；cookie 方案需 CSRF token。

**可运行代码**

```go title="interview/ch08_auth/q07_sql_injection/main.go"
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
```

**项目位置**：全仓储走 GORM 参数化（`internal/*/repository/*_gorm.go`）；前端统一 Authorization 头 + CORS 白名单（`internal/platform/cors`）；安全头在 nginx（Q4）。
