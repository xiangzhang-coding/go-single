package cache

import "context"

// Cache 缓存层接口（ADR-0003 seam），隔离 go-redis 客户端。
// Lua 脚本封装在适配器内，业务模块只面向本接口。
type Cache interface {
	// Ping 检查连接可用性。
	Ping(ctx context.Context) error
	// Close 释放底层连接。
	Close() error
}
