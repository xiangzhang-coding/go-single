// Q5 结构化日志：JSON 输出 + 字段化打点。
// 运行：go run ./interview/ch09_engineering/q05_logging
package main

import (
	"log/slog"
	"os"
)

func main() {
	// 项目主日志用 zap（JSON 到 stdout，可镜像到文件供 Loki 采集）；
	// 此处用 stdlib slog 演示同款结构化风格（product 模块实际混用了 slog）。
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// 业务日志：机器可读的键值对，而非拼字符串。
	logger.Info("秒杀预扣成功",
		"activity_id", 1001,
		"user_id", 7,
		"order_no", "O20260813001",
		"redis_stock_left", 49,
	)

	// 访问日志（项目 requestLogger 中间件）: method/route/status/duration。
	logger.Info("http request",
		"method", "POST",
		"route", "/api/flashsales/:id/purchase",
		"status", 202,
		"duration_ms", 3.2,
	)
}

// 项目位置：internal/platform/logger/logger.go（zap production JSON + log.file 镜像 +
// 自动建目录）；访问日志 cmd/server/middleware.go 的 requestLogger；
// 降级告警用 slog（internal/product/service/product_service.go）；
// 采集链 promtail → Loki（deploy/monitoring/promtail/config.yml）。
