# 面试题库配套代码（可运行）

配套文档站 [tech/interview](https://github.com/xiangzhang-coding/go-single/tree/main/website/docs/tech/interview) 的 12 章共 84 题。每题一个独立程序，纯标准库（少量复用项目既有依赖），可直接运行：

```bash
# 任意一题（示例）
go run ./interview/ch07_flashsale/q03_lua_prededuct

# 全部编译验证
go build ./...

# 含测试的题目
go test ./interview/ch09_engineering/q06_testing -v
```

- 每个程序头部注释注明题目编号与对应项目真实位置（`internal/`、`migrations/`、`deploy/` 等）。
- 章节目录：`ch01_go_basics` ~ `ch12_resilience`，对应文档站 12 个分章。
- 代码演示的是语义，不是生产实现本身；生产实现请直接看 `internal/`。
