---
sidebar_position: 1
---

# 快速开始

三步本地启动：**起依赖（docker compose）→ 起后端（go run）→ 起前端（bun dev）**。

环境要求：Go 1.26+、Docker Desktop、bun（≥1.0）。后端启动时自动执行数据库迁移（golang-migrate），无需手动建表。

## 第 1 步：起依赖

```bash
docker compose up -d mysql redis rabbitmq minio
```

依赖清单（端口）：

| 服务 | 端口 | 用途 |
| --- | --- | --- |
| MySQL 8 | 3306 | 业务数据（账号/商品/订单/秒杀/好友/聊天） |
| Redis 7 | 6379 | 缓存、秒杀预扣与幂等键、领券防超发 |
| RabbitMQ | 5672 / 15672（管理台） | 秒杀异步落单消息队列 |
| MinIO | 19000 / 19001（控制台） | 聊天图片/文件存储 |

等待依赖就绪：

```bash
docker compose ps
# mysql / redis / rabbitmq / minio 均为 healthy 后继续
```

:::tip 可观测组件（可选）
`docker compose up -d` 可一并启动 Prometheus(:9090) / Grafana(:3000，预置 Loki 数据源与服务日志面板) / Loki(:3100) / Promtail / Nginx(:8081，托管前端 dist 并反代 `/api`)。
Nginx 需要先构建前端产物（`cd web/faire && bun install && bun run build`），否则容器健康检查会失败。
:::

## 第 2 步：起后端

```bash
go run ./cmd/server
```

- 自动执行 `migrations/`（含 admin 种子账号），监听 `:8080`
- 健康检查：`curl http://127.0.0.1:8080/healthz`，返回 `{"status":"ok",...}` 即就绪
- 秒杀库存对账 cron（每小时）与订单超时取消 cron（每分钟）随服务启动

## 第 3 步：起前端

```bash
cd web/faire
bun install
bun run dev
```

打开 `http://localhost:5173`，Vite dev server 已把 `/api`、`/ws` 代理到后端 `:8080`，无需额外配置。

## 验收

- 浏览器可注册/登录（演示账号见[演示账号](./demo-accounts)）
- 商品列表/详情可浏览，购物车与下单流程可走通
- 可选：`docker compose up -d` 后访问 `http://localhost:8081` 查看 Nginx 托管形态
