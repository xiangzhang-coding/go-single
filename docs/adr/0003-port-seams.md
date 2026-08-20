# 关键依赖层以端口-适配器抽象（仓储/缓存/消息）

仅对三类"可能更换的依赖"定义接口：仓储层（数据访问）、缓存层（Redis）、消息层（MQ）。GORM 之上再包 repository 接口；MQ 接口让 RabbitMQ/Kafka 可换；缓存接口隔离 Redis 客户端。日志、配置、Web 框架不做抽象，避免过度设计的 YAGNI 成本。

好友关系、订单、优惠券等业务数据的访问均为仓储 seam 的具体实例，经各自 repository 接口访问；"双向 + 申请流程"与"互相关注即好友"两种好友实现可切换（后者在 backlog）。

认证校验器（TokenVerifier）为额外轻量 seam，不在上述三类之列（见 DESIGN 认证节）。

私有媒体同样使用调用方定义的最小轻量 seam：user/social/chat 仅依赖 `IsOwned` 校验托管引用，不接触 MinIO；platform/file 的读取处理器通过 `AccessAuthorizer` 聚合头像、好友圈和会话授权。其动机与 TokenVerifier/MessageNotifier 相同，是限制跨模块能力面而非承诺可替换对象存储。

模块间进程内调用同样经接口（面向接口而非 HTTP，见 ADR-0001）：chat 服务的 UserService / SocialService 是业务对业务端口（用户查询、好友关系校验）；MessageNotifier（T18，消息落库后实时推送，实现为 platform/ws Hub）是业务对平台基础设施端口，与 TokenVerifier 同属非可替换依赖的轻量 seam——抽象动机是"调用方只依赖最小能力面"，而非"将来可换实现"。

跨模块数据库事务使用 `internal/platform/transaction.Handle` 作为不透明句柄：service 与 repository 接口只负责传递，GORM bridge 只能在 adapter 中调用，并由 transaction 架构测试强制。这样保留订单/库存/优惠券等单事务原子性，同时不让 ORM 类型穿透业务 seam。
