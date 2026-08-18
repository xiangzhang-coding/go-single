package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// repoRoot 定位仓库根目录（本文件位于 internal/platform/config/ 下，向上 3 级）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func TestLoad(t *testing.T) {
	root := repoRoot(t)
	cfg, err := LoadFrom(filepath.Join(root, "configs"), root)
	require.NoError(t, err)

	require.Equal(t, 8080, cfg.Server.Port)
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, "./logs/app.log", cfg.Log.File)
	require.Equal(t, "127.0.0.1", cfg.MySQL.Host)
	require.Equal(t, 3306, cfg.MySQL.Port)
	require.Equal(t, "go_shop", cfg.MySQL.Database)
	require.Equal(t, "127.0.0.1:6379", cfg.Redis.Addr)
	require.Contains(t, cfg.MQ.URL, "amqp://")
	require.Equal(t, "127.0.0.1:19000", cfg.MinIO.Endpoint)
	require.Equal(t, "go-shop", cfg.MinIO.Bucket)
	require.False(t, cfg.MinIO.UseSSL)
	require.Equal(t, "./migrations", cfg.Migrations.Path)
	require.Equal(t, int64(1), cfg.Snowflake.WorkerID)
	require.Empty(t, cfg.WS.AllowOrigins)
	require.Empty(t, cfg.CORS.AllowOrigins)
}

func TestMySQLDSN(t *testing.T) {
	m := MySQL{Host: "h", Port: 3306, User: "u", Password: "p", Database: "d"}
	require.Equal(t, "u:p@tcp(h:3306)/d?parseTime=true&charset=utf8mb4&loc=Local", m.DSN())
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("GO_SINGLE_SERVER_PORT", "9090")
	t.Setenv("GO_SINGLE_REDIS_ADDR", "10.0.0.1:6379")
	t.Setenv("GO_SINGLE_SNOWFLAKE_WORKER_ID", "7")

	root := repoRoot(t)
	cfg, err := LoadFrom(filepath.Join(root, "configs"), root)
	require.NoError(t, err)
	require.Equal(t, 9090, cfg.Server.Port)
	require.Equal(t, "10.0.0.1:6379", cfg.Redis.Addr)
	require.Equal(t, int64(7), cfg.Snowflake.WorkerID)
}
