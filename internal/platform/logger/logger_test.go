package logger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// newTestLogger 创建写往临时文件镜像的 logger，返回日志文件路径。
func newTestLogger(t *testing.T, level string) (*zap.Logger, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "app.log")
	log, err := New(level, file)
	require.NoError(t, err)
	return log, file
}

// syncLog 触发 flush。测试环境 stdout 非真实终端，Sync 报 bad file descriptor
// 属预期；zap 的 MultiWriteSyncer 仍会同步其余 writer，此处只需触发 flush。
func syncLog(log *zap.Logger) {
	_ = log.Sync()
}

func readFile(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	return string(raw)
}

func TestNewOutputsJSONLines(t *testing.T) {
	log, file := newTestLogger(t, "info")

	log.Info("hello", zap.String("path", "/api/v1/products"))
	syncLog(log)

	line := strings.TrimSpace(readFile(t, file))
	// zap 生产编码：单行 JSON，含 ts/level/msg 与结构化字段。
	for _, want := range []string{`"level":"info"`, `"msg":"hello"`, `"path":"/api/v1/products"`} {
		require.Contains(t, line, want)
	}
	require.NotContains(t, line, "\n")
}

func TestNewLevelFilter(t *testing.T) {
	t.Run("info 级别过滤 debug", func(t *testing.T) {
		log, file := newTestLogger(t, "info")

		log.Debug("should-not-appear")
		log.Info("should-appear")
		syncLog(log)

		got := readFile(t, file)
		require.NotContains(t, got, "should-not-appear")
		require.Contains(t, got, "should-appear")
	})

	t.Run("debug 级别全部输出", func(t *testing.T) {
		log, file := newTestLogger(t, "debug")

		log.Debug("debug-line")
		log.Info("info-line")
		syncLog(log)

		got := readFile(t, file)
		require.Contains(t, got, "debug-line")
		require.Contains(t, got, "info-line")
	})
}

func TestNewCreatesParentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	log, err := New("info", filepath.Join(dir, "app.log"))
	require.NoError(t, err)
	syncLog(log)

	_, err = os.Stat(filepath.Join(dir, "app.log"))
	require.NoError(t, err, "父目录不存在时应自动创建日志文件")
}

func TestNewInvalidLevel(t *testing.T) {
	_, err := New("not-a-level", "")
	require.Error(t, err)
}

func TestNewWithoutFileOnlyStdout(t *testing.T) {
	// 仅 stdout 模式：New 不创建任何文件，写日志不报错即可。
	log, err := New("info", "")
	require.NoError(t, err)
	log.Info("hello")
	syncLog(log)
}

func TestGORMLoggerRedactsParametersFromErrorsAndSlowQueries(t *testing.T) {
	log, file := newTestLogger(t, "debug")
	dbLog := NewGORM(log, gormlogger.Warn, time.Millisecond)
	filter, ok := dbLog.(gorm.ParamsFilter)
	require.True(t, ok)

	const (
		message    = "private-message-sentinel"
		address    = "private-address-sentinel"
		credential = "private-credential-sentinel"
	)
	sqlTemplate, params := filter.ParamsFilter(context.Background(),
		"INSERT INTO sensitive_table (message, address, credential) VALUES (?, ?, ?)",
		message, address, credential)
	require.Empty(t, params)

	dbLog.Trace(context.Background(), time.Now(), func() (string, int64) {
		return sqlTemplate, 0
	}, errors.New("database rejected "+credential))
	dbLog.Trace(context.Background(), time.Now().Add(-time.Second), func() (string, int64) {
		return sqlTemplate, 1
	}, nil)
	syncLog(log)

	got := readFile(t, file)
	for _, secret := range []string{message, address, credential} {
		require.NotContains(t, got, secret)
	}
	require.Contains(t, got, "数据库查询失败")
	require.Contains(t, got, "数据库慢查询")
	require.Contains(t, got, "VALUES (?, ?, ?)")
}
