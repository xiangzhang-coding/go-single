# 部署指南（T28 部署收尾）

部署三件套：**本地 HTTPS 演示**（Nginx SSL 终止 + 安全头）、**云端静态资源**（Cloudflare Pages 接线 web/ 主题 + website/ 文档站）、**后端云平台选型**（VPS + Docker Compose，见 ADR-0005）。

> 后端后台演示：`go run ./cmd/server` 建议**前台运行**（另开终端）。如需后台化，请用
> `go build -o bin/server ./cmd/server && nohup ./bin/server &`——直接 `nohup go run ... &` 时
> 杀掉 go 命令进程不会终止其编译产物子进程（孤儿进程仍监听 :8080 并消费 MQ 队列，
> 会污染本地集成测试）。

## 5. 上线前必改清单（安全收尾）

| # | 项 | 本地默认 | 上线要求 |
|---|---|---|---|
| 1 | `auth.secret`（JWT 签名密钥） | `dev-secret-change-me` | **强随机**（≥32 字节）；泄露可伪造任意角色 token |
| 2 | MySQL / RabbitMQ / MinIO 凭据 | `shop123` / `guest:guest` / `minioadmin` | 全部改强密码（compose 与 configs 同步） |
| 3 | `cors.allow_origins` / `ws.allow_origins` | 空（允许所有 Origin） | 配置前端域名白名单 |
| 4 | `server.trusted_proxies` | 127.0.0.1/::1（本地 Nginx） | 改为反代出口 IP（client_ip 日志/指标才真实） |
| 5 | compose 端口暴露（MySQL 3306 / RabbitMQ 5672 / Redis 6379 / MinIO 19000） | 0.0.0.0 | ufw 只放 22/80/443 |
| 6 | `server.mode` | `debug` | `release` |
| 7 | Grafana/Prometheus 访问 | 无认证 | 加认证或内网隔离 |
| 8 | 秒杀 `worker_id` | 1 | 多实例时每实例唯一（0-1023） |

依赖版本安全：`go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...` 与
`bun run scripts/frontend-audit.ts`（CI 已内置，版本均固定）。
已知项：`golang.org/x/crypto/openpgp` 为上游弃用包（无修复版，代码未调用，仅经依赖链引入）；
website 构建链 `@docusaurus/mdx-loader@3.10.2 → image-size@2.0.2` 的
`GHSA-5p2g-fcmc-qvqq` / `CVE-2025-71329` 与 `GHSA-w3rx-r6r6-pgpr` / `CVE-2025-71330`
上游无修复版，仅处理仓库内受控图片且不进入浏览器产物。例外按精确版本、依赖路径和复查期限记录在
`security/frontend-audit-allowlist.json`；新增漏洞、路径变化、例外消失或过期均使 CI 失败。

## 1. 本地 HTTPS 与安全头演示

拓扑：`浏览器 → Nginx（443 SSL 终止 + 安全头 + 静态托管）→ 后端 :8080（宿主机进程）`；80 端口仅 301 跳转 HTTPS。

前置：依赖容器起好（`docker compose up -d mysql redis rabbitmq minio`）、后端在跑（`go run ./cmd/server`）、前端已构建（`cd web/faire && bun install && bun run build`）。

```bash
# 1) 生成自签证书（certs/ 不入库，有效 365 天）
cd deploy/nginx && ./gen-certs.sh

# 2) 起 Nginx（8081:80 跳转，8443:443 业务）
cd ../ && docker compose up -d nginx

# 3) 演示 80 → 301 跳转
curl -sI http://127.0.0.1:8081/ | head -2
# HTTP/1.1 301 Moved Permanently
# Location: https://127.0.0.1:8443/

# 4) 演示 HTTPS + 安全头（-k：自签证书）
curl -skI https://127.0.0.1:8443/ | grep -iE "HTTP/|content-type|frame-options|strict-transport|content-security|referrer-policy"
# HTTP/2 200
# content-type: text/html
# strict-transport-security: max-age=31536000; includeSubDomains
# x-frame-options: DENY
# content-security-policy: ...
# referrer-policy: no-referrer

# 5) 演示反代联通（后端 502 时 nginx 健康检查转 unhealthy）
curl -sk https://127.0.0.1:8443/api/products
```

浏览器访问 `https://127.0.0.1:8443`（自签证书需手动信任；IP 地址访问不受 HSTS 影响）。生产证书：Nginx 终止 SSL 不变，仅把自签证书换成 Let's Encrypt（certbot 或云厂商证书），见 §4。

安全头清单（本地与云端**头名称一致、值按部署路径调整**，DESIGN「安全设计 → HTTPS 与安全头」）：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Strict-Transport-Security`（HSTS，仅域名生效）、`Content-Security-Policy`（本地：同源脚本/样式 + MinIO 图片直连 + WS 通道；云端：放开 `https:` / `wss:`，见 §2）。

## 2. Cloudflare Pages 接线（web/ 主题 + website/ 文档站）

两个独立 Pages 项目，纯静态部署，与后端运行完全解耦。

| 项目 | 目录 | 构建 | 输出目录 | 接线文件 |
|---|---|---|---|---|
| `go-single-web-faire` | `web/faire` | `bun install && bun run build` | `dist/` | `public/_redirects`（SPA fallback `/* /index.html 200`）、`public/_headers`（安全头） |
| `go-single-website` | `website` | `bun install && bun run build` | `build/` | `static/_headers`（安全头）、`static/_redirects`（占位，Docusaurus 自带 404 处理） |

`_redirects` / `_headers` 置于 `public/` / `static/`，构建时随产物拷入输出目录，由 Cloudflare Pages 自动识别。

### 构建环境变量

- **web/faire**：Cloudflare 构建必须提供 `VITE_API_BASE`（后端 HTTPS 绝对地址，如 `https://api.example.com`）和 `VITE_WS_BASE`（后端 WSS 绝对地址，如 `wss://api.example.com/ws`），缺失或协议错误时 workflow 在安装依赖前失败。本地 Nginx 构建仍可省略并使用 `/api`、`/ws` 同源回退。跨源时后端 `platform/cors` 按白名单放行（见 configs/config.yaml `cors.allowed_origins`）。
- **website**：无环境变量。

### 域名 / HTTPS

- Pages 项目自带 `*.pages.dev` 域名与自动签发/续期的 HTTPS 证书，零配置。
- 绑定自定义域名：Pages 项目 → Custom domains → 添加域名；若域名托管在 Cloudflare，一条 CNAME 记录即可（自动代理 + SSL/TLS 全自动）。
- HTTPS 强制：Pages 默认全站 HTTPS；自定义域名可在规则（Rules）中加 HTTPS 重写，或由 CF 代理的 CNAME 默认生效。
- 本地 CSP 里的 `http://127.0.0.1:19000`（MinIO 直连）仅为本地演示，云端 `_headers` 的 CSP 已放开 `https:` / `wss:`。

### 自动部署（GitHub Actions）

`.github/workflows/pages-deploy.yml`：push main（web/ 或 website/ 变更）或手动触发，自动构建并 `wrangler pages deploy`。首次配置：

```bash
# 1) 仓库 Secrets：CLOUDFLARE_API_TOKEN（Cloudflare 令牌，Pages:Edit 权限）、CLOUDFLARE_ACCOUNT_ID
# 2) 仓库 Variables：VITE_API_BASE / VITE_WS_BASE（后端地址）
# 3) 预创建两个 Pages 项目（或让首次部署自动创建）
npx wrangler pages project create go-single-web-faire
npx wrangler pages project create go-single-website
```

## 3. 后端云部署选型（结论）

**VPS + Docker Compose + Nginx 反代**为主选（ADR-0005）；Fly.io / Railway 等 PaaS 仅作演练备选，Kubernetes 明确不做。

| 方案 | 结论 | 理由 |
|---|---|---|
| VPS（Hetzner/腾讯云轻量 2C4G） | **主选** | compose 即部署单、本地云端同构；systemd/ufw/Nginx+Let's Encrypt/备份是就业向学习目标；成本低、全可控 |
| Fly.io / Railway / Render | 备选（演练） | 对有状态服务 + 多容器网络（RabbitMQ/MinIO/端口暴露、卷挂载）限制或收费偏贵；抽象掉运维概念 |
| Kubernetes | 不做 | 单一部署单元的模块化单体过度设计（与"拆微服务才引网关"同判据） |

### VPS 部署步骤（概要）

```bash
# 1) 基础环境：安装 Docker Engine + compose 插件、ufw（只放 22/80/443）、clone 仓库
# 2) 配置：configs/config.yaml 改 MySQL/Redis/RabbitMQ/MinIO 主机为云上地址与强密码
# 3) 证书：Let's Encrypt（certbot --nginx 或云厂商证书），替换 deploy/nginx/certs/ 自签证书
# 4) 起服务：docker compose up -d（mysql/redis/rabbitmq/minio/nginx + 可观测全家桶）
# 5) 后端守护：systemd unit 跑 go 构建产物（bin/server），开机自启 + 崩溃重启
# 6) 域名解析：api.example.com → VPS；Pages 前端 VITE_API_BASE 指向它
# 7) 备份与监控：MySQL 定时 dump（cron）+ 对象存储；Prometheus/Grafana/Loki 已在 compose 预置
```

秒杀等在线业务不在此演示范围（单实例）；多实例负载均衡（upstream 双实例示例已预写在 deploy/nginx/nginx.conf）为 backlog 演进项。

## 4. 验收对照

- [x] 本地 HTTPS 与安全头可演示：§1 命令可复现（301 跳转 + 安全头齐全 + /api 反代联通）
- [x] 前端与文档站可部署到 Cloudflare Pages：§2 接线 + workflow，构建产物可直接部署
- [x] 后端云部署方案选型确定并文档化：ADR-0005 + §3
