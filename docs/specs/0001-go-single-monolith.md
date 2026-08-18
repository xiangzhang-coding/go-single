# Go 单体商城 — 项目规格

## Problem Statement

一个正在学习 Go 后端技术栈的开发者，希望通过一个就业向、模块化单体架构的在线商城项目，系统学习：模块化单体架构、商城/秒杀/社交/IM 业务、主流组件选型（Gin/GORM/MySQL/Redis/RabbitMQ）、DDD 风格模块设计、可观测性与服务容错。当前只有设计文档（CONTEXT.md / DESIGN.md / ADR / BACKLOG），缺少一份完整、无歧义、可直接指导实现、可交付给任何实现者（人或 AI）的规格文档，也未建立版本管理。

## Solution

构建一个模块化单体在线商城（go_single）：
- **后端**：单一 Go 部署单元承载 9 个业务模块（user/product/cart/order/flashsale/payment/coupon/social/chat + admin）+ platform 共享基础设施；模块间经 service 接口进程内调用（面向接口，非 HTTP），依赖方向无环；仅秒杀落单走 MQ 异步
- **演示前端**：工程级插拔——每套主题 = `web/` 下独立 Vite 工程（React+TS+Tailwind v4+bun），共享同一后端 REST 接口契约（json tag ↔ 手写 TS 类型 + HTTP 集成测试，ADR-0006）；首套主题 "faire" 的 4 件套设计资产已就位
- **文档站**：Docusaurus（zh-CN）独立部署于 Cloudflare Pages——用户指南（任务导向）+ 技术文档（模块/领域双视图）+ 面试题库（80-100 题随实现产出）
- 就业向选型（Gin/GORM/MySQL8/go-redis/RabbitMQ/zap/viper/JWT），可插拔 seam（仓储/缓存/MQ），Docker Compose 一键起依赖，Prometheus+Grafana+Loki 可观测，全链路容错
- 版本管理：git + GitHub 仓库（本次初始化）

## User Stories

### 用户与认证

1. 作为游客，我想注册账号（bcrypt 加密密码），以便成为用户开始购物
2. 作为用户，我想登录并获取 JWT（有效期 2h），以便访问受保护的接口
3. 作为用户，我想查看与修改我的个人信息（昵称/头像，头像经文件上传），以便维护资料
4. 作为用户，我想管理收货地址簿（新增/编辑/删除/设为默认），以便下单时快速选择
5. 作为管理员，我期望默认 admin 账号（admin/admin123，migration 种入）可直接登录后台
6. 作为用户，我期望 JWT 过期后重新登录即可继续使用（无 refresh，进 backlog）

### 商品与购物车

7. 作为游客，我想按类目浏览商品列表（分页），以便挑选商品
8. 作为游客，我想查看商品详情（SPU 标题/详情/类目 + SKU 规格/价格/库存），以便决定购买
9. 作为用户，我想把商品加入购物车（条目引用 SKU），以便暂存待购
10. 作为用户，我想修改购物车条目数量或删除条目，以便管理待购清单
11. 作为管理员，我想创建/编辑/上架/下架商品与 SKU 并维护库存，以便运营商品
12. 作为管理员，我想管理商品类目，以便组织商品结构

### 订单与交易

13. 作为用户，我想从购物车结算下单（选择地址、可选一张优惠券），以便完成购买
14. 作为用户，我想在商品详情页直接下单（直购），以便快速购买
15. 作为用户，我想查看订单列表（按状态筛选、分页）与订单详情，以便跟踪订单
16. 作为用户，我想取消待支付订单，以便放弃购买
17. 作为用户，我想在订单详情页发起模拟支付（成功/失败两个选项），以便驱动订单流转
18. 作为用户，我想确认收货，以便将订单置为已完成
19. 作为管理员，我想对已支付订单发货（置为已发货），以便推进订单流转
20. 作为系统，我想定时取消超时未支付订单（普通 15min/秒杀 10min）并回补库存、回退优惠券
21. 作为用户，我期望下单接口幂等（`client_request_id` 重复提交只生成一单并返回同一订单号）
22. 作为用户，我期望订单记录下单时的地址快照，后续修改地址不影响历史订单
23. 作为用户，我期望订单金额正确计算（商品总额 − 券额 = 应付金额，支付回调核对）

### 秒杀

24. 作为用户，我想在秒杀页查看进行中/即将开始的活动（倒计时、库存余量、每人限购数），以便参与抢购
25. 作为用户，我想抢购秒杀商品，预扣成功后立即返回"排队中"，以便快速获得抢购结果
26. 作为用户，我想轮询订单接口（`GET /api/orders/{order_no}`，1.5s×30 次上限）得知异步落单结果
27. 作为用户，我期望每人限购（`per_user_limit`，后台可配，默认 1）被原子强制
28. 作为用户，我期望同一 `client_request_id` 重试返回原预扣，而新的请求在 `per_user_limit` 内获得独立购买槽位；订单按 `user_id:activity_id:purchase_slot` 去重，取消只释放对应槽位
29. 作为用户，我期望秒杀订单超时未支付取消后，库存与购买机会回补（允许再次抢购）
30. 作为管理员，我想创建/编辑/上架/下架秒杀活动（时间窗口、独立库存、秒杀价、限购数），以便运营促销
31. 作为系统，我期望活动上架时库存预热进 Redis（SETNX，未开始可覆盖、进行中只减不增）
32. 作为系统，我想每小时比对 Redis 活动库存与 MySQL 活动库存与秒杀有效订单数；活动进行中只告警不覆盖，活动结束后以 MySQL 为准对齐 Redis
33. 作为系统，我想在秒杀高峰限流（全局令牌桶 + 按用户 Redis 计数），以便保护服务
34. 作为用户，我期望秒杀订单不使用优惠券（与秒杀互斥）

### 优惠券

35. 作为用户，我想浏览可领取的优惠券（直减/满减）并领取，以便下单抵扣
36. 作为用户，我想查看我的券列表（未用/已用/过期），以便管理
37. 作为用户，我想在下单时使用一张券（满减门槛结算时校验，一单一张），以便享受折扣
38. 作为用户，我期望取消订单后所用券被回退
39. 作为管理员，我想发布券模板（类型/面额/满减门槛/总量/每人限领/有效期），以便开展营销
40. 作为系统，我期望领券防超发（Lua 原子脚本：检查限领与总量后计数）

### 好友与好友圈

41. 作为用户，我想添加好友（发起申请），以便建立关系
42. 作为用户，我想处理好友申请（通过/拒绝），以便控制好友列表
43. 作为用户，我想查看好友列表，以便互动
44. 作为用户，我想在购买成功后分享动态到好友圈（引用已购 SKU + 可选文案 + 可选图片），以便分享购买心得
45. 作为用户，我想浏览好友圈时间线（仅好友可见，拉取式分页），以便查看好友动态
46. 作为用户，我想删除自己的动态，以便管理分享
47. 作为用户，我想按用户名前缀搜索用户（`GET /api/users?username=`，排除自己），以便发起好友申请时找到对方
48. 作为用户，我想查看自己的动态列表（`GET /api/posts/mine`，时间倒序分页），以便回顾个人分享

### 即时通信

49. 作为用户，我想与好友单聊（text/image/file 消息，图片文件经 MinIO），以便沟通
50. 作为用户，我想查看会话列表与消息列表（按会话游标分页），以便回顾聊天
51. 作为用户，我期望在线时实时收到消息（WebSocket 推送），以便即时沟通
52. 作为用户，我期望离线消息在上线后可拉取，以便不错过消息
53. 作为用户，我期望发送消息走 REST（可幂等重试），以便可靠投递
54. 作为用户，我期望 WebSocket 连接携带 JWT 鉴权（query 传 token，注明日志风险为演示取舍）
55. 作为用户，我想标记会话已读并查看未读数（`POST /api/conversations/{key}/read` 推进已读游标 + 会话列表未读计数），以便跟进未读消息

### 后台管理

56. 作为管理员，我想在后台管理商品/订单/秒杀活动/券模板（role 鉴权，业务逻辑复用各模块 service），以便运营整个商城
57. 作为管理员，我期望后台接口与前台接口共用一套认证体系（user.role 区分）

### 可观测性与容错

58. 作为开发者，我想通过 `/metrics` 查看 HTTP QPS/延迟分位/错误率/活跃请求、秒杀预扣与库存、订单与支付、MQ 发布消费、优惠券发放核销、Go runtime 指标，以便监控系统
59. 作为开发者，我想通过 Grafana 大盘查看项目关键指标，以便可视化监控
60. 作为开发者，我想通过 Loki 检索结构化日志（zap JSON → promtail → Loki），以便排障
61. 作为开发者，我期望全链路 context 超时、幂等操作有限重试、MQ 消费者熔断（gobreaker）、缓存兜底降级，以便服务在依赖故障时保持可用

### 演示前端

62. 作为演示者，我想用 Faire 风格主题的前端演示全部功能（13 个页面清单），以便展示项目
63. 作为开发者，我想按主题工程级插拔前端（每套主题独立 Vite 工程，共享同一后端，部署时选一套构建），以便复用与换肤
64. 作为开发者，我期望前端与后端以 REST 接口契约对接（手写 TS 类型对齐后端 json tag + HTTP 集成测试固定响应形状，axios + 拦截器统一 JWT/401 处理），以便联调
65. 作为演示者，我期望前端可本地 Nginx 部署（同源反代）或云端 Cloudflare Pages 独立部署（`_redirects` + `VITE_API_BASE` 跨源）

### 文档站

66. 作为用户，我想阅读任务导向的用户指南（快速开始三步启动、演示账号、功能向导），以便快速上手演示
67. 作为开发者，我想阅读按模块组织的技术文档（tech/modules 镜像 internal/，每模块数据模型/接口/时序），以便改代码时导航
68. 作为开发者，我想阅读按领域分组的设计文档（tech/domains 薄视图，映射模块与术语），以便理解 DDD 结构
69. 作为学习者，我想刷面试题库（80-100 题，分章，每题答案要点+可运行 Go 代码+关联本项目位置），以便面试准备
70. 作为维护者，我期望 `docs/`（ADR/DESIGN/BACKLOG）为工程决策权威源，文档站只放摘要与链接（需仓库公开），避免双份失同步

## Implementation Decisions

### 架构

- **模块化单体**（ADR-0001）：单一 Go 部署单元承载全部业务模块；模块 = 限界上下文，内部自治，模块间经 service 接口进程内调用（面向接口，非 HTTP）；依赖方向画成 DAG 并禁止循环（depguard lint 进 backlog）；仅秒杀落单走 MQ 异步
- **演示前端与文档站不在单体单元内**（ADR-0001）：前端以 REST 契约为界独立构建，文档站为独立静态站点
- **DDD 风格垂直模块**：9 个业务模块 + admin 管理入口（只做入口与 role 鉴权，业务逻辑在各模块 service）+ platform 共享基础设施（config/logger/metrics/auth/limiter/cors/cron/mq/cache/ws/file）
- **可插拔 seam**（ADR-0003）：仅三类依赖定义接口——仓储层（每模块 repository 接口，GORM 之上再包一层）、缓存层（隔离 go-redis，Lua 封装在适配器）、消息层（RabbitMQ 实现，Kafka 可换）；好友关系等业务数据访问均为仓储 seam 实例；TokenVerifier 为额外轻量 seam（不在三类之列）
- **选型**（ADR-0002/0004 + 技术栈表）：Gin/GORM/MySQL 8/go-redis v9/RabbitMQ/JWT 自签 HS256+bcrypt/zap/viper/robfig-cron/x-time-rate/golang-migrate/testing+testify/手写 DI；API 契约由手写 TS 类型 + HTTP 集成测试承担（ADR-0006，swaggo 进 backlog）；现代实践（sqlc/PostgreSQL/slog/session/env/asynq/Kafka/Redis 分布式限流/wire/stdlib 路由/OIDC）全部进 backlog

### 订单与交易

- 普通/秒杀共用状态机：待支付 → 已支付（支付回调）→ 已发货（后台发货）→ 已完成（用户确认收货）；含用户取消与超时取消；非法跃迁直接拒绝
- 订单表含 `order_type`（normal/seckill）；秒杀订单不使用优惠券，持久化 `purchase_slot`，可空 `user_activity_key=user_id:activity_id:purchase_slot` 唯一约束只拦同槽重投；取消置 NULL 释放该槽位
- 普通订单：购物车/直购 → 单事务（订单+订单项+库存条件更新 `stock>=N`+地址快照+券核销+删除购物车条目）→ 待支付；超时取消回补库存+回退券
- 幂等：`client_request_id`（Redis SETNX + TTL 15min），重复请求返回同一订单号
- 订单号：雪花 ID（手写实现，学习点）；超时默认 普通 15min / 秒杀 10min

### 秒杀

- 活动独立库存模型：`flashsale.stock`（秒杀专属）+ `price`（秒杀价），与 `sku.stock` 互不干扰；落单扣活动库存
- Lua 原子脚本：校验时间窗口 + `status` 下架标志 + `per_user_limit` + DECR 库存 + INCR 用户计数（limit 作为 ARGV 传入）
- 幂等两段式：MySQL `(user, activity, client_request_id)` 区分请求重试和新购买；Redis 槽位键（TTL 30min）保护单次预扣；DB `user_activity_key` 唯一约束挡同槽重复落单
- MQ 异步落单：失败重投/死信 + 对账兜底；取消回补 Redis 库存 + MySQL 库存 + 用户计数（允许再次抢购）
- 预扣事实固化 SKU、成交价、数量和购买槽位，消费者不读取活动当前价格/SKU改写订单；进行中仅标题和库存减少可编辑，编辑经 Redis pause 栅栏 + MySQL 行锁重算差额，不能减到已接受预扣以下，同步失败活动自动下架
- key 约定 `flashsale:stock:{id}` / `flashsale:count:{id}:{user}` / `flashsale:idem:{id}:{user}:{purchase_slot}`
- 对账分场景：活动进行中只比对告警不自动回写（Redis 预扣领先属正常，仅识别"有扣减无订单"作补单信号）；活动结束收尾对账以 MySQL 为准对齐 Redis
- 限流：全局单机令牌桶（x/time/rate，QPS 可配）+ 秒杀接口按用户 Redis 计数（INCR+TTL）

### 支付 / 优惠券

- 模拟支付：外部 API 端点 `POST /api/payments/mock {order_id, result}` → payment service → 进程内调用 order service；成功需状态机校验 + 支付流水唯一约束 + 金额核对；失败留待支付可重付
- 优惠券：券模板（直减/满减/面额/门槛/总量/限领/有效期）；Redis Lua 快速计数并从 MySQL 事实重建，MySQL 模板行锁事务作为防超发最终约束；一单一张、结算校验门槛、事务内核销、取消回退；与秒杀互斥、全场券；订单记 `discount_amount`，应付 = 总额 − 券额

### 认证与安全

- JWT 自签 HS256（2h，无 refresh）+ bcrypt；TokenVerifier 接口（自签实现，OIDC 换实现进 backlog）
- `user.role`（user/admin）+ admin 中间件；admin 种子账号 admin/admin123（migration 种入）
- 对象级授权：订单/购物车/地址簿/聊天/好友操作强制校验 `owner_id`，防 IDOR
- HTTPS 与安全头（Nginx 终止 SSL + X-Content-Type-Options/X-Frame-Options/CSP）；上传类型白名单 png/jpeg/webp/gif 且 ≤5MB；MinIO 桶私有；日志不记录密码与 token

### 社交 / 即时通信

- 好友：申请/通过流程（关系仓储接口化，免申请实现可切换进 backlog）
- 动态：仅好友可见；购买成功后分享（引用已购 SKU + 文案 + 图片）；拉取式时间线分页
- IM：单聊；`conversation_key = min(uidA,uidB):max(uidA,uidB)`；消息三通道——发送 REST（可幂等重试）、实时接收 WS 推送、离线 REST 按会话游标分页拉取；WS 握手 JWT（query 传 token，注明日志风险取舍）；已读回执为会话粒度已读游标（`POST /api/conversations/{key}/read` 推进，未读数 = 我收到且 id > 游标的消息数）

### 可观测性与容错

- 指标：`platform/metrics`（client_golang，自动含 Go runtime 指标）；指标点：HTTP QPS/延迟直方图 50/90/99 分位/4xx/5xx/活跃请求、秒杀预扣成功失败/库存余量、订单创建/状态/支付、MQ 发布/消费/消费失败、优惠券发放/核销；指标注册器支持扩展
- 日志：zap 结构化 JSON → promtail → Loki → Grafana；Grafana 预配置 datasource + 项目大盘；prometheus/grafana/loki/promtail 默认进 docker-compose
- 容错：全链路 context 超时；仅幂等操作有限重试 + 退避；gobreaker 熔断仅包 MQ 消费者；缓存兜底降级（商品详情 key `product:detail:{id}` TTL 5min，缓存挂直查 DB）

### 演示前端

- 工程级插拔：每套主题 = `web/<theme>/` 独立 Vite 工程（bun），共享后端 REST 接口契约；换主题 = 部署时选一套构建；第二套主题克隆骨架；`web/shared/` 出现第二套再抽
- 主题资产：四件套 `web/<theme>/design/`（DESIGN.md 规范参考 / CSS_Variables.css / Design_Tokens.json W3C DTCG / Tailwind_V4.css @theme）；组件一律语义化 Tailwind 类
- 工程选型：react-router v7（admin 按角色分组 + role 守卫）、TanStack Query（服务端状态）+ zustand（客户端状态）、axios 拦截器（JWT 头 + 401 跳登录）、JWT 存 localStorage、手写组件、TS 类型手写对齐后端 json tag（ADR-0006）
- 对接：`VITE_API_BASE` / `VITE_WS_BASE`（dev 用 /api、/ws 代理到 :8080）；文件上传 `POST /api/files` 后端代理（presigned 明确不做）
- 页面清单（13 页）：登录/注册、首页、商品详情、购物车、结算、订单列表/详情、秒杀页、优惠券中心、好友列表/申请、好友圈、聊天、个人中心（地址簿）、后台管理
- 交互约定：秒杀排队中轮询订单接口 1.5s×30；倒计时由轮询接口带服务端时间；401 跳登录；支付为订单详情页内动作（不设独立页）
- 部署双路径：本地 Nginx（托管选定主题 dist + /api 反代 + try_files + upstream 双实例示例）；云端 Cloudflare Pages（`public/_redirects` + `VITE_API_BASE` 跨源 + 后端 CORS）
- 桌面优先，不做移动端适配

### 文档站与工程文档

- Docusaurus（zh-CN，bun 构建 → build/）部署 Cloudflare Pages；user-guide（快速开始三步、演示账号、功能向导）+ tech/modules（镜像 internal/，每模块详细规格）+ tech/domains（镜像 CONTEXT.md 领域分组，薄视图）+ tech/interview（80-100 题：Go 基础/并发/网络/MySQL/Redis/MQ/秒杀架构/认证安全/工程实践/部署运维/可观测性/容错与降级，随实现同步产出）
- `docs/`（ADR/DESIGN/BACKLOG）为工程决策权威源，文档站只放摘要与链接（需仓库公开）

### 部署

- docker-compose：mysql/redis/rabbitmq/minio/nginx/prometheus/grafana/loki/promtail（默认开启；不含前端构建——宿主 bun build 后挂载 dist）
- Nginx 即网关：路由/SSL 终止/静态托管/入口限流/upstream 双实例示例；拆微服务时才引入独立 API 网关
- Cloudflare Pages 部署（web/ 主题 + website/ 文档站）：`_redirects` / `_headers` 接线、构建环境变量（VITE_API_BASE / VITE_WS_BASE）、域名与 HTTPS、GitHub Actions 自动部署（T28，详见 docs/DEPLOYMENT.md）
- 后端云平台选型：VPS + Docker Compose 为主选，PaaS 备选（T28，ADR-0005）

## Testing Decisions

- **主 seam（最高点）：HTTP API 层黑盒集成测试**——`httptest` 起完整路由 + 真实 MySQL/Redis（docker compose 或 testcontainers）；覆盖完整业务流程（注册→登录→加购→下单→支付→发货→确认收货）、秒杀全链路（限流→Lua 预扣→MQ 落单→轮询→超时回补）、幂等（client_request_id、秒杀幂等键+唯一约束）、状态机非法跃迁拒绝、金额计算与支付核对、对象级授权（跨用户访问拒绝）
- **中间 seam：service 层单元测试**——各模块 service + fake repository（复用 ADR-0003 仓储接口 seam，测试替身成本低）；覆盖状态机流转、限购/门槛校验、券核销与回退、好友申请流转
- **底层 seam：Lua 脚本测试**——Redis 内直接执行脚本验证原子性、超卖防护（并发抢空）、限购边界
- 只测外部行为（接口输入输出与状态迁移），不测实现细节；工具 testing + testify
- 前端不做自动化测试（演示项目）；文档站无测试；测试先例：各模块 service 测试与 API 集成测试同构，随模块实现逐步建立

## Out of Scope

- 所有 backlog 项：群聊、退款/售后、免申请双向好友、JWT refresh、多实例 LB+灰度（upstream 权重+header/cookie）、OpenTelemetry 链路、指标点扩展、舱壁、Sentinel-golang、数据库读写分离、认证增强（登出黑名单/密码复杂度/双因素）、shadcn/ui、openapi-typescript、JWT cookie+CSRF 方案、RabbitMQ 延迟队列
- 明确不做：微服务拆分、API 网关（Kong/APISIX）、注册中心/服务网格、presigned 直传（前端直连 MinIO）、ELK 技术栈
- 移动端适配、真实支付渠道、多语言站点

## Further Notes

- 学习点（实现时刻意练习）：goroutine/channel/context、面向接口编程、错误处理、手写雪花 ID、Lua 原子脚本、模块边界、testify 测试
- 面试题库随模块实现同步产出（每题：答案要点 + 可运行 Go 代码 + 关联本项目位置）
- 建议实现顺序（tracer bullet）：骨架（compose/config/platform）→ user 认证 → product → cart → order → payment → flashsale → coupon → social → chat → 可观测与容错 → web/faire 前端 → website 文档站 → 部署收尾
- 版本管理已初始化（git + GitHub，仓库需公开以便文档站链接）
- 设计权威源：CONTEXT.md（术语表）/ docs/DESIGN.md（总览）/ docs/adr/0001~0004（决策）/ docs/BACKLOG.md（演进）
