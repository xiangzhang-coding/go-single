---
sidebar_position: 11
---

# platform — 共享基础设施

**定位**：config（viper）/ logger（zap）/ metrics（Prometheus）/ auth（JWT）/ limiter / cors / cron / mq（RabbitMQ）/ cache（Redis）/ ws / file（MinIO 上传）/ snowflake / retry / health。可替换组件集中于本层，替换成本低（见 [BACKLOG](https://github.com/xiangzhang-coding/go-single/blob/main/docs/BACKLOG.md)）。

实现：`internal/platform/`。

## 组件清单

| 组件 | 职责 | 关键点 |
| --- | --- | --- |
| config | viper 加载 `configs/config.yaml` + 环境变量覆盖（`GO_SINGLE_*`，如 `GO_SINGLE_AUTH_SECRET`） | 含默认值（port 8080、request_timeout 5s、JWT 2h、秒杀限流 50 QPS/100 burst 等） |
| logger | zap 结构化 JSON 输出（stdout） | `log.file` 非空时同内容镜像写入文件（promtail 采集进 Loki）；父目录自动创建 |
| metrics | Prometheus 指标注册器（独立 registry，内置 go_*/process_* 采集器） | HTTP 三件套（`http_requests_total` QPS / `http_request_duration_seconds` 延迟直方图 50/90/99 / `http_errors_total` 4xx/5xx）+ `http_requests_active` gauge；**业务指标**（T19c）见下；`/metrics` 端点（中间件不计自身抓取流量） |
| auth | JWT 自签（HS256）+ bcrypt | `TokenVerifier` 接口（轻量 seam，非 ADR-0003 三类）；`Middleware`（Bearer 解析 → 401）+ `RequireAdmin`（→ 403）；JWT 有效期 2h，无 refresh |
| limiter | 令牌桶 + Redis 固定窗口计数 | 全局令牌桶中间件（`golang.org/x/time/rate`，单实例）；`RedisCounter` 经 `IncrementFixedWindow` 类型化缓存能力完成 INCR+TTL（跨请求状态，fail-closed） |
| cors | Origin 白名单跨源中间件（T26） | 空白名单 = 允许所有（演示取舍）；预检请求非白名单 403；无 Origin（同源/非浏览器）不处理 |
| cron | 定时任务注册表（robfig/cron 薄封装） | `SkipIfStillRunning` 防重叠 + panic 兜底；单次执行超时可配（5min）；优雅停止等待执行中任务 |
| mq | RabbitMQ 消息层（ADR-0003 seam） | 发布确认（publisher confirm）+ 持久化消息；队列自动声明（幂等）+ **死信队列 `<queue>.dlq`**；消费端 QoS 预取 1、单条消息超时 15s；Ack / Nack 重投（瞬时）/ Nack 拒收进死信（`ErrPermanent`）；**消费者熔断**（gobreaker，连续失败打开→半开探活，仅包消费） |
| cache | Redis 缓存层（ADR-0003 seam） | 接口隔离 go-redis；订单幂等、领券/计数重建、秒杀预热/预扣/回补、固定窗口计数均以类型化能力暴露，Lua 文本与返回码协议仅存在于适配器内 |
| ws | WebSocket 实时通道 | Hub 管理在线连接（userID → 连接集合）；`PushToUser` 单向推送（缓冲满 = 慢消费者，关闭连接）；心跳保活（Ping 30s / pong_wait 2× / 写超时 10s）；`GET /ws?token=` 握手鉴权 |
| file | MinIO 私有桶上传代理 | `POST /api/files`（multipart 字段 "file"，Bearer）；**魔数嗅探**类型白名单（png/jpeg/webp/gif）+ ≤5MB；桶私有（匿名探测防暴露存在性）；前端不直连 MinIO；调用方：图片/文件消息、动态配图、用户头像（URL 由 `PATCH /api/users/me` 写入） |
| snowflake | 手写雪花 ID（学习点） | 41bit 毫秒时间戳（纪元 2024-01-01）+ 10bit worker + 12bit 序列号 = 63bit int64；单实例单调递增；**时钟回拨拒绝生成**；同毫秒序列号耗尽自旋 |
| retry | 有限重试 + 指数退避（T20） | **仅幂等操作可重试**（普通下单 / 支付回调 / 秒杀消息发布）；`retry.Stop` 标记业务拒绝不重试；退避可被 ctx 取消；默认 3 次、100ms 起、上限 1s |
| health | 依赖连通性检查 | `/healthz`：并发探测 MySQL / Redis / MQ（2s 超时），全部正常返回 200 `ok`；任一失败返回 503 `degraded`（body 含各依赖明细） |
| pagination | 统一分页参数解析（T05） | `pagination.FromQuery(c)` 解析 `page`/`page_size`：默认 1/20，非法或 `<1` 回退，page_size `>50` 钳制（page 不设上限）；4 模块 7 处 handler 复用；service 层钳制保留为非 HTTP 调用方的幂等防线 |

## 平台级端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /metrics | Prometheus 抓取（HTTP 三件套 + Go runtime + 业务指标） |
| GET | /healthz | 依赖健康检查（mysql/redis/mq） |
| GET | /ws | WebSocket 握手（query 携带 JWT，见 [chat](./chat) 实时通道） |
| POST | /api/files | 文件上传代理（类型白名单 + ≤5MB → MinIO 私有桶 → 返回 URL） |

## 业务指标（platform/metrics Business 集合，T19c）

| 指标 | 类型 | 维度 |
| --- | --- | --- |
| seckill_prededuct_total | CounterVec | result（success/fail）——秒杀原子预扣 |
| seckill_stock_remaining | GaugeVec | activity_id——活动库存余量（随预扣/回补/秒杀页浏览/上架/下架刷新） |
| orders_created_total | CounterVec | order_type（normal/seckill）——订单创建（幂等命中不重复计数） |
| orders_status_total | CounterVec | status——订单进入各状态累计次数 |
| orders_payment_total | CounterVec | result——支付回调处理数（流水落库后计数） |
| mq_published_total | CounterVec | queue, result——MQ 发布 |
| mq_consumed_total | CounterVec | queue——MQ 消费 |
| mq_consume_failed_total | CounterVec | queue, reason（permanent/transient）——消费失败 |
| coupon_issued_total / coupon_redeemed_total | Counter | ———优惠券发放 / 核销 |

Grafana 大盘（`deploy/monitoring/grafana/dashboards/business.json`）逐项对应。

权威源：[docs/DESIGN.md 技术栈 / 可观测性 / 容错与降级 / 定时任务](https://github.com/xiangzhang-coding/go-single/blob/main/docs/DESIGN.md)、[ADR-0003 关键依赖层抽象](https://github.com/xiangzhang-coding/go-single/blob/main/docs/adr/0003-port-seams.md)、[ADR-0004 RabbitMQ](https://github.com/xiangzhang-coding/go-single/blob/main/docs/adr/0004-rabbitmq-over-kafka.md)。
