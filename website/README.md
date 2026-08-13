# website/ — 文档站

Docusaurus（zh-CN）文档站，与后端/前端完全独立构建；构建产物部署到 Cloudflare Pages（部署接线在实现后期完成）。

```bash
bun install
bun run start    # 本地开发：http://localhost:3000
bun run build    # 生产构建 → build/
bun run serve    # 本地预览构建产物
```

## 内容结构

- `docs/user-guide/` — 用户文档（任务导向）：快速开始 / 演示账号 / 功能向导
- `docs/tech/modules/` — 镜像 `internal/` 的模块视图（数据模型 / 接口 / 时序，随模块实现同步产出）
- `docs/tech/domains/` — 镜像 `CONTEXT.md` 的领域分组（薄视图：模块 ↔ 领域映射）
- `docs/tech/interview/` — 面试题库（80-100 题，计划中，随模块实现同步产出）

工程决策权威源为仓库 `docs/`（ADR / DESIGN / BACKLOG），本站只放摘要与链接。
