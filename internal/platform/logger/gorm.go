package logger

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// NewGORM creates a GORM logger that keeps SQL placeholders and never records
// bound parameters or raw database errors, both of which may contain secrets.
func NewGORM(log *zap.Logger, level gormlogger.LogLevel, slowThreshold time.Duration) gormlogger.Interface {
	return &gormZapLogger{log: log, level: level, slowThreshold: slowThreshold}
}

type gormZapLogger struct {
	log           *zap.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

func (l *gormZapLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	clone := *l
	clone.level = level
	return &clone
}

func (l *gormZapLogger) Info(_ context.Context, message string, _ ...interface{}) {
	if l.level >= gormlogger.Info {
		l.log.Debug("GORM 信息", zap.String("message_template", message))
	}
}

func (l *gormZapLogger) Warn(_ context.Context, message string, _ ...interface{}) {
	if l.level >= gormlogger.Warn {
		l.log.Warn("GORM 警告", zap.String("message_template", message))
	}
}

func (l *gormZapLogger) Error(_ context.Context, message string, _ ...interface{}) {
	if l.level >= gormlogger.Error {
		l.log.Error("GORM 错误", zap.String("message_template", message))
	}
}

func (l *gormZapLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)
	switch {
	case err != nil && l.level >= gormlogger.Error && !errors.Is(err, gorm.ErrRecordNotFound):
		sql, rows := fc()
		l.log.Error("数据库查询失败",
			zap.String("error_kind", "database_error"),
			zap.Duration("elapsed", elapsed),
			zap.Int64("rows", rows),
			zap.String("sql", sql))
	case l.slowThreshold > 0 && elapsed > l.slowThreshold && l.level >= gormlogger.Warn:
		sql, rows := fc()
		l.log.Warn("数据库慢查询",
			zap.Duration("threshold", l.slowThreshold),
			zap.Duration("elapsed", elapsed),
			zap.Int64("rows", rows),
			zap.String("sql", sql))
	case l.level == gormlogger.Info:
		sql, rows := fc()
		l.log.Debug("数据库查询",
			zap.Duration("elapsed", elapsed),
			zap.Int64("rows", rows),
			zap.String("sql", sql))
	}
}

// ParamsFilter makes GORM pass SQL templates, not interpolated values, to Trace.
func (l *gormZapLogger) ParamsFilter(_ context.Context, sql string, _ ...interface{}) (string, []interface{}) {
	return sql, nil
}

var _ gormlogger.Interface = (*gormZapLogger)(nil)
var _ gorm.ParamsFilter = (*gormZapLogger)(nil)
