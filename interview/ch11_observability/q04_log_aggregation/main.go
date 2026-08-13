// Q4 结构化日志 + 日志聚合：从 JSON 行到 Loki 检索。
// 运行：go run ./interview/ch11_observability/q04_log_aggregation
package main

import (
	"encoding/json"
	"fmt"
)

type logLine struct {
	Ts          string `json:"ts"`
	Level       string `json:"level"`
	Msg         string `json:"msg"`
	ActivityID  int64  `json:"activity_id,omitempty"`
	OrderNo     string `json:"order_no,omitempty"`
	Elapsed     string `json:"elapsed"`
}

func main() {
	// 一条 zap JSON 日志（logger.go 输出格式，ISO8601 ts）。
	line := logLine{Ts: "2026-08-13T21:30:00.123+08:00", Level: "info",
		Msg: "秒杀预扣成功", ActivityID: 1001, OrderNo: "O20260813001", Elapsed: "1.2ms"}
	b, _ := json.Marshal(line)
	fmt.Println("日志行:", string(b))

	fmt.Println()
	fmt.Println("采集链：zap 输出 stdout / 镜像 log.file → promtail（docker 容器 + 文件作业）")
	fmt.Println("        → Loki（labels: job=go-single）→ Grafana 日志面板按 label + 关键词检索")
	fmt.Println("检索示例：{job=\"go-single\"} |~ \"秒杀预扣成功\"")
}

// 项目位置：internal/platform/logger/logger.go（JSON + log.file 镜像 + 自动建目录）；
// deploy/monitoring/promtail/config.yml（两个作业）、loki/config.yml、
// grafana/dashboards/logs.json（Loki datasource 面板）。
