package logger

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New 创建 zap 结构化 JSON 日志（stdout），级别可配。
func New(level string) (*zap.Logger, error) {
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

	return cfg.Build()
}
