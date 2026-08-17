# Backlog

现代最佳实践与二期能力（组件替换选项 + 演进方向）。可替换组件集中于 `platform/` 或经接口初始化，替换成本低；功能项按二期排期。

## 范围裁决记录

- 已读回执（会话粒度已读游标 + 未读数）原列二期，实现与前端均已落地——T07 裁决转入正式范围（spec 故事 55），从二期清单移除。

## 组件替换（集中于 platform/ 或经接口初始化，替换成本低）

- sqlc（替代 GORM）
- PostgreSQL（替代 MySQL）
- slog（替代 zap）
- session 认证（替代 JWT）
- env 配置（替代 viper）
- asynq（替代 robfig/cron）
- Kafka（替代 RabbitMQ）
- Redis 分布式限流（替代单机令牌桶）
- wire（替代手写 DI）
- swaggo/swag（Swagger 注解 + /swagger 路由，按模块分批试点——ADR-0006 维持手写 TS 类型 + 集成测试为契约机制）
- stdlib net/http 路由实验
- depguard（模块依赖无环 lint，强制 DESIGN.md 的 DAG）
- 第三方认证服务器（Keycloak/OIDC，经 TokenVerifier 接口替换自签 JWT）

## 更优解标注

- RabbitMQ 延迟队列（TTL + 死信）：订单超时取消的秒级精确方案，替代 cron 轮询扫描

## 二期功能

- 免申请双向好友（互相关注即互为好友，替换申请/通过流程——好友关系仓储经接口抽象，可低成本切换）
- 退款/售后状态流转
- 群聊
- JWT refresh token（访问令牌过期自动续期）
- 多实例负载均衡实操（Nginx upstream 权重 + header/cookie 灰度分流）
- 链路追踪：OpenTelemetry + Jaeger（单体阶段价值低，微服务化前的铺垫）
- 指标点扩展：秒杀接口成功率、优惠券过期数、WS 连接数、聊天消息速率等（经 platform/metrics 注册器即插即用）
- 舱壁隔离（semaphore 并发池，限制关键路径并发）
- Sentinel-golang（流量控制/熔断演进，替代 gobreaker 的备选）
- 数据库读写分离（MySQL 主从）
- 认证增强：登出黑名单、密码复杂度策略、双因素认证
- shadcn/ui（组件库参考，按主题定制——当前手写组件）
- openapi-typescript（OpenAPI → TS 类型自动生成——当前手写对齐 json tag，见 ADR-0006）
- JWT 存储方案讨论：cookie + CSRF 防护（当前 localStorage 取舍）

## 明确不做（单体阶段）

- 微服务拆分
- API 网关（Kong/APISIX）
- 注册中心 / 服务网格
- presigned 直传（前端直连 MinIO 上传——统一走后端代理）
