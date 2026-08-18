# 部署指南

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
./deploy/nginx/gen-certs.sh

# 2) 起 Nginx（8081:80 跳转，8443:443 业务）
docker compose up -d nginx

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

安全头清单（本地与云端**头名称一致、值按部署路径调整**，DESIGN「安全设计 → HTTPS 与安全头」）：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Strict-Transport-Security`（HSTS，仅域名生效）、`Content-Security-Policy`（媒体经后端鉴权读取为 `blob:`，浏览器不直连 MinIO；云端 API/WS 放开 `https:` / `wss:`，见 §2）。

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
# 7) 备份与监控：MySQL 定时 dump + Redis/RabbitMQ 卷快照；Prometheus/Grafana/Loki 已预置
```

秒杀等在线业务不在此演示范围（单实例）；多实例负载均衡（upstream 双实例示例已预写在 deploy/nginx/nginx.conf）为 backlog 演进项。

## 4. Redis 与 RabbitMQ 持久化运维

`deploy/docker-compose.yml` 是规范编排，根目录 `docker-compose.yml` 仅作 include 入口。新部署的 Compose project 固定为 `deploy`，所有命令统一从仓库根运行；Redis/RabbitMQ 卷另用全局固定名，避免工作目录改变后选中空卷。旧环境若曾从根目录启动，project 可能是 `go_single`，首次升级必须按下节保留原 project 名，否则 MySQL、MinIO 和可观测组件的 project-scoped 卷会被误认为空卷。

| 服务 | 命名卷与数据目录 | 持久化策略 | 恢复目标 |
|---|---|---|---|
| Redis | `go_single_redis_data:/data` | AOF `appendonly=yes` + `appendfsync=everysec` 为主，RDB `3600/1 300/100 60/10000` 为兜底 | 容器重建后尽量保留订单在途协调键、秒杀预扣和优惠券计数；普通订单持久幂等事实仍以 MySQL 为准；正常故障约 1 秒 RPO |
| RabbitMQ | `go_single_rabbitmq_data:/var/lib/rabbitmq` | 固定 `rabbit@go-single-rabbitmq` 节点名；应用声明 durable 主队列/DLQ，以 delivery mode 2 发布并等待 publisher confirm | 容器重建后 durable 队列和已确认的 persistent 消息仍可消费 |

MySQL 的 `flashsale_pre_deductions` 是 Redis 预扣与 RabbitMQ 消息之间的恢复事实源，必须随订单表一起备份。应用启动恢复和 ordered reservation 修复各有 10s 超时，失败后由每分钟 cron 继续；预扣、重建和回补 Lua 使用专用 Redis 连接并紧跟 `WAITAOF 1 0 2000`，只有本地 AOF 明确 fsync 后才推进 MySQL 终态，覆盖 `appendfsync=everysec` 的 pre-fsync 重启窗口。生产 Redis 必须为 7.2+ 且启用 AOF，否则秒杀生命周期会保持可恢复状态而不冒险落单。监控 `pending_publish`、`pending_order`、`pending_rollback` 的数量和最老 `updated_at`，不能只观察聚合库存或 DLQ 深度。

`docker compose down` 默认保留命名卷；`docker compose down -v`、`docker volume rm go_single_redis_data` 和 `docker volume rm go_single_rabbitmq_data` 会销毁业务状态，日常部署禁止使用。Redis 未配置淘汰上限，因为这些键包含业务状态而不只是可重建缓存；容量不足时应扩容或拆分实例，不能改成 LRU 静默淘汰。

### 首次从旧匿名卷迁移

R14 之前的镜像会为数据目录创建匿名卷。演练脚本检测到匿名卷会直接失败，避免升级时静默切到空命名卷。首次迁移安排维护窗口，先停止后端发布和消费，并在同一个 shell 会话中固定历史 project 名：

```bash
set -euo pipefail

# 1) 保留历史 project 名，避免切换 MySQL/MinIO 等已有 project-scoped 卷
existing_project="$(docker inspect go_single_mysql --format '{{index .Config.Labels "com.docker.compose.project"}}')"
case "$existing_project" in
  deploy) ;;
  go_single)
    export COMPOSE_PROJECT_NAME=go_single
    # 后续会话也要在仓库根 .env 中保留 COMPOSE_PROJECT_NAME=go_single；不要覆盖已有 .env 内容。
    ;;
  *) printf '未知 Compose project: %s\n' "$existing_project" >&2; exit 1 ;;
esac

# 2) 绑定旧卷名，供归档和 Redis 数据复制使用
export OLD_REDIS_VOLUME="$(docker inspect go_single_redis --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')"
export OLD_RABBITMQ_VOLUME="$(docker inspect go_single_rabbitmq --format '{{range .Mounts}}{{if eq .Destination "/var/lib/rabbitmq"}}{{.Name}}{{end}}{{end}}')"
test -n "$OLD_REDIS_VOLUME"
test -n "$OLD_RABBITMQ_VOLUME"

# 3) RabbitMQ 必须先排空 ready/unacked 消息；DLQ 先处理或另行归档
docker exec go_single_rabbitmq rabbitmqctl list_queues name durable messages_ready messages_unacknowledged
docker exec go_single_rabbitmq rabbitmqctl export_definitions /tmp/definitions.json
docker cp go_single_rabbitmq:/tmp/definitions.json "$HOME/go-single-rabbitmq-definitions.json"

# 4) 停服务并归档旧卷；不要删除归档或旧匿名卷，直到恢复验收完成
docker stop go_single_redis go_single_rabbitmq
export MIGRATION_BACKUP_DIR="$HOME/go-single-backups/pre-r14-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$MIGRATION_BACKUP_DIR" && chmod 700 "$MIGRATION_BACKUP_DIR"
docker run --rm -v "$OLD_REDIS_VOLUME:/source:ro" -v "$MIGRATION_BACKUP_DIR:/backup" \
  alpine:3.21 tar -czf /backup/redis-anonymous-volume.tgz -C /source .
docker run --rm -v "$OLD_RABBITMQ_VOLUME:/source:ro" -v "$MIGRATION_BACKUP_DIR:/backup" \
  alpine:3.21 tar -czf /backup/rabbitmq-anonymous-volume.tgz -C /source .
tar -tzf "$MIGRATION_BACKUP_DIR/redis-anonymous-volume.tgz" >/dev/null
tar -tzf "$MIGRATION_BACKUP_DIR/rabbitmq-anonymous-volume.tgz" >/dev/null
```

Redis 匿名卷可按下一节的卷归档方法复制到 `go_single_redis_data`，保留全部 DB 和 TTL。RabbitMQ definitions 不含排队消息，且旧容器的随机节点名不能直接当作新固定节点的数据目录；必须先排空消息，再在新节点导入 definitions。若无法排空，不要继续迁移，应先建立临时消费者/转发流程并验证消息数归零。

```bash
# 移除旧容器但保留其匿名卷，由 Compose 创建正确归属的命名卷容器
docker rm go_single_redis go_single_rabbitmq
docker compose create redis rabbitmq

# 新 Redis 容器尚未启动，可安全复制旧卷的全部 DB、RDB 和 TTL
docker run --rm \
  -v "$OLD_REDIS_VOLUME:/source:ro" \
  -v go_single_redis_data:/target \
  alpine:3.21 sh -c 'cp -a /source/. /target/'

# 启动固定节点名的 RabbitMQ，导入已排空消息后的 definitions
docker compose up -d --wait redis rabbitmq
docker cp "$HOME/go-single-rabbitmq-definitions.json" go_single_rabbitmq:/tmp/definitions.json
docker exec go_single_rabbitmq rabbitmqctl import_definitions /tmp/definitions.json
```

完成下方持久化演练并检查业务后再启动后端。至少保留旧匿名卷和 definitions 归档一个备份周期，确认无需回滚后再人工清理。

### 备份与恢复

基线方案是维护窗口内停止两个服务后对卷做文件级归档，可同时覆盖 Redis AOF/RDB、RabbitMQ definitions 和排队消息。备份包含业务数据、RabbitMQ cookie 等敏感内容，应存入权限受控且加密的异机/对象存储。

```bash
set -euo pipefail
backup_dir="$HOME/go-single-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$backup_dir" && chmod 700 "$backup_dir"

docker compose stop redis rabbitmq
docker run --rm -v go_single_redis_data:/source:ro -v "$backup_dir:/backup" \
  alpine:3.21 tar -czf /backup/redis-volume.tgz -C /source .
docker run --rm -v go_single_rabbitmq_data:/source:ro -v "$backup_dir:/backup" \
  alpine:3.21 tar -czf /backup/rabbitmq-volume.tgz -C /source .
docker compose up -d --wait redis rabbitmq
```

恢复会覆盖当前数据，必须先停后端并保存当前卷快照。RabbitMQ 应先用备份时相同的镜像版本和固定节点名恢复，确认消息后再升级；不要用较低版本 RabbitMQ 打开高版本数据目录。

```bash
set -euo pipefail
: "${BACKUP_DIR:?先 export BACKUP_DIR=/path/to/verified-backup}"
backup_dir="$BACKUP_DIR"
test -f "$backup_dir/redis-volume.tgz"
test -f "$backup_dir/rabbitmq-volume.tgz"
tar -tzf "$backup_dir/redis-volume.tgz" >/dev/null
tar -tzf "$backup_dir/rabbitmq-volume.tgz" >/dev/null

docker compose down
docker volume rm go_single_redis_data go_single_rabbitmq_data
docker compose create redis rabbitmq

docker run --rm -v go_single_redis_data:/target -v "$backup_dir:/backup:ro" \
  alpine:3.21 tar -xzf /backup/redis-volume.tgz -C /target
docker run --rm -v go_single_rabbitmq_data:/target -v "$backup_dir:/backup:ro" \
  alpine:3.21 tar -xzf /backup/rabbitmq-volume.tgz -C /target
docker compose up -d
docker compose up -d --wait redis rabbitmq
```

恢复后运行 `redis-cli INFO persistence`、`rabbitmq-diagnostics -q ping`、队列消息数检查和本节演练。仅有备份文件不算完成，至少每季度在隔离 VPS 做一次恢复演练并记录耗时。

### 容量与升级

```bash
# Redis：内存、AOF/RDB 状态和重写失败
docker compose exec -T redis redis-cli INFO memory
docker compose exec -T redis redis-cli INFO persistence

# RabbitMQ：持久消息积压、未确认消息和磁盘/内存告警
docker compose exec -T rabbitmq rabbitmqctl list_queues \
  name durable messages_ready messages_unacknowledged message_bytes_persistent
docker compose exec -T rabbitmq rabbitmq-diagnostics alarms

# Docker 卷与宿主机总体空间
docker system df -v
df -h
```

Redis 至少预留当前 AOF 两倍的可用磁盘供重写，并监控 `aof_last_write_status`、`aof_last_bgrewrite_status` 和内存增长。RabbitMQ 对持续增长的 `messages_ready`/DLQ、任何 resource alarm 告警；VPS 数据盘达到 70% 时预警，80% 前扩容或处理积压。备份容量不与生产卷共盘计算。

升级时先停后端写入与消费者，运行演练并备份，再一次只改一个服务的固定 tag/digest。Redis 升级前检查 AOF/RDB 格式兼容性；RabbitMQ 按官方支持路径逐小版本/大版本升级，先恢复到原版本验证后再前进，禁止直接降级复用数据目录。每次只启动对应服务，健康检查、数据读取和队列深度都通过后再继续，最后重新运行演练。

### 容器重建演练

脚本写入代表性的订单幂等、秒杀预扣/计数/reservation marker 和优惠券计数状态，声明 durable 测试队列并发布 delivery mode 2 消息，然后删除并重建 Redis/RabbitMQ 容器，验证 TTL、值、队列和消息后清理测试数据。脚本会中断已有连接，只在本地或维护窗口运行，且绝不使用 `down -v`。

```bash
bash scripts/persistence-drill.sh

# 上线后修改了 Redis/RabbitMQ 凭据时
PERSISTENCE_DRILL_REDIS_PASSWORD='redis-password' \
PERSISTENCE_DRILL_RABBITMQ_USER=shop \
PERSISTENCE_DRILL_RABBITMQ_PASSWORD='rabbitmq-password' \
  bash scripts/persistence-drill.sh
```

成功终态为：`[persistence-drill] 通过：Redis 关键状态和 RabbitMQ 持久消息均跨容器重建保留`。

## 5. 验收对照

- [x] 本地 HTTPS 与安全头可演示：§1 命令可复现（301 跳转 + 安全头齐全 + /api 反代联通）
- [x] 前端与文档站可部署到 Cloudflare Pages：§2 接线 + workflow，构建产物可直接部署
- [x] 后端云部署方案选型确定并文档化：ADR-0005 + §3
- [x] Redis/RabbitMQ 使用固定命名卷与明确持久化配置：§4 + `deploy/docker-compose.yml`
- [x] durable 队列、persistent 消息和 Redis 关键状态跨容器重建保留：`scripts/persistence-drill.sh`
- [x] 备份、恢复、容量、首次迁移和升级注意事项已记录：§4
