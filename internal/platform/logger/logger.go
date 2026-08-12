package logger

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New 创建 zap 结构化 JSON 日志（stdout），级别可配；
// file 非空时把同内容 JSON 行镜像写入该文件（供 promtail 采集进 Loki），
// 父目录不存在则自动创建。
func New(level, file string) (*zap.Logger, error) {
	lv, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("无效日志级别 %q: %w", level, err)
	}

	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.Level = zap.NewAtomicLevelAt(lv)

	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, fmt.Errorf("创建日志目录: %w", err)
		}
		cfg.OutputPaths = append(cfg.OutputPaths, file)
		cfg.ErrorOutputPaths = append(cfg.ErrorOutputPaths, file)
	}

	return cfg.Build()
}
