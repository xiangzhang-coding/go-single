package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss 缓存未命中（key 不存在）。
var ErrMiss = errors.New("cache miss")

// Cache 缓存层接口（ADR-0003 seam），隔离 go-redis 客户端。
// Lua 脚本封装在适配器内，业务模块只面向本接口。
type Cache interface {
	// Ping 检查连接可用性。
	Ping(ctx context.Context) error
	// Close 释放底层连接。
	Close() error
	// Get 读取字符串值；未命中返回 ErrMiss。
	Get(ctx context.Context, key string) (string, error)
	// Set 写入字符串值并设置 TTL。
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
	// Del 删除 key（不存在不视为错误）。
	Del(ctx context.Context, key string) error
	// Eval 原子执行 Lua 脚本（Redis EVAL 封装，学习点）。
	// 业务模块持有脚本内容，仅经此方法执行；返回整数结果，脚本约定由调用方定义。
	Eval(ctx context.Context, script string, keys []string, args ...any) (int64, error)
}
