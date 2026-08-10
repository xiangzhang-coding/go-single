# 关键依赖层以端口-适配器抽象（仓储/缓存/消息）

仅对三类"可能更换的依赖"定义接口：仓储层（数据访问）、缓存层（Redis）、消息层（MQ）。GORM 之上再包 repository 接口；MQ 接口让 RabbitMQ/Kafka 可换；缓存接口隔离 Redis 客户端。日志、配置、Web 框架不做抽象，避免过度设计的 YAGNI 成本。

好友关系、订单、优惠券等业务数据的访问均为仓储 seam 的具体实例，经各自 repository 接口访问；"双向 + 申请流程"与"互相关注即好友"两种好友实现可切换（后者在 backlog）。

认证校验器（TokenVerifier）为额外轻量 seam，不在上述三类之列（见 DESIGN 认证节）。
