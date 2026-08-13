// Q7 发布流水线：测试 → 构建 → 镜像 → 部署。
// 运行：go run ./interview/ch10_deploy/q07_cicd
package main

import (
	"fmt"
)

type step struct {
	name string
	run  func() error
}

func main() {
	// 流水线 = 有序步骤 + 失败即停（本例模拟执行，真实用 GitHub Actions 等）。
	pipeline := []step{
		{"单元测试与集成测试", func() error { fmt.Println("  go test ./..."); return nil }},
		{"编译", func() error { fmt.Println("  go build ./... && go vet ./..."); return nil }},
		{"构建镜像", func() error { fmt.Println("  docker build -t go-single:${TAG} ."); return nil }},
		{"推送镜像仓库", func() error { fmt.Println("  docker push ..."); return nil }},
		{"滚动发布", func() error { fmt.Println("  docker compose up -d --pull always"); return nil }},
	}
	for i, s := range pipeline {
		fmt.Printf("[%d/5] %s\n", i+1, s.name)
		if err := s.run(); err != nil {
			fmt.Println("流水线中止:", err)
			return
		}
	}
	fmt.Println("发布完成：healthcheck 通过后流量进入新版本")
}

// 项目位置：本仓库暂无 CI 文件（BACKLOG "初始化 git 仓库/部署演进"），
// 本地流程 = docker compose up（依赖）+ go run ./cmd/server（服务）；
// 前端 bun build（web/faire、website/），文档站可接 Cloudflare Pages。
