# API 契约机制：手写 TS 类型 + HTTP 集成测试（swaggo 不引入）

## 背景

spec 与 DESIGN.md 早期将 swaggo/swag 列为选型、前端边界表述为 "Swagger REST 契约"，但仓库从未落地：
全仓无 swagger 注解、无 `/swagger` 路由、go.mod 无 swaggo 依赖。实际承担契约职责的机制一直是——

- **后端**：HTTP API 层黑盒集成测试（各模块 `*_integration_test.go`，httptest 起完整路由 + 真实 MySQL/Redis/MinIO），
  以可执行断言固定每个端点的请求/响应形状与状态码，即事实上的契约测试；
- **前端**：`web/faire/src/api/types.ts` 手写 TS 类型对齐后端 JSON 序列化字段（json tag），
  `tsc -b` 在构建期强制类型检查。

## 决策

维持现状为正式契约机制（方向 B），不引入 swaggo：

1. **契约由两层承担**：集成测试固定后端行为（含字段形状），手写 TS 类型镜像 json tag 并经 tsc 构建期校验；
   漂移由 CI 双重暴露——后端改 json tag 会使集成测试断言失败，前端对接变化会使 faire 构建失败。
2. **swaggo 的收益对本项目不成立**：注解是随每个 handler 持续维护的样板负担（9 模块 70+ 端点）；
   演示前端仅一套主题且与后端同仓演进，无多消费方需要机器可读契约；无外部团队联调场景。
3. **替换路径保留**：swaggo（注解 + `/swagger` 路由）与 openapi-typescript（OpenAPI → TS 自动生成）
   均移入 BACKLOG 组件替换清单——若未来出现第二套主题/外部消费方，可按"模块分批试点注解 →
   openapi-typescript 替换手写类型"路径升级。
4. **错误响应统一**：业务 API 失败响应使用 `{ "error": string }`，由后端共享响应包与前端
   `ErrorResponse` 共同固定。校验失败、未认证、无权限、未找到、冲突、限流和超时分别使用
   400、401、403、404、409、429 和 504；未知内部错误使用 500 且不返回依赖错误内容。
   各模块仍在自己的 handler 内声明业务 sentinel 到状态码的映射，platform 层不依赖领域模块。

## 后果

- DESIGN.md 选型表 "API 文档" 行如实标注现状（swaggo 移出选型、进 backlog）；
- 前后端边界表述统一为 "REST 接口契约（json tag ↔ 手写 TS 类型 + 集成测试）"，不再声称 Swagger；
- 新端点的交付清单含"types.ts 对齐 + 集成测试断言响应形状"两项（已为各模块既有实践）。
