package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// envPrefix 使全部配置项可被环境变量覆盖（GO_SINGLE_MYSQL_HOST 等）。
const envPrefix = "GO_SINGLE"

// Config 全量配置，从 configs/config.yaml 加载，环境变量可覆盖。
type Config struct {
	Server     Server
	Log        Log
	MySQL      MySQL
	Redis      Redis
	MQ         MQ
	MinIO      MinIO
	Migrations Migrations
	Auth       Auth
	Snowflake  Snowflake
	FlashSale  FlashSale
	Retry      Retry
	WS         WS
	CORS       CORS
}

// Retry 幂等操作有限重试配置（T20）：仅幂等操作启用（普通下单/模拟支付回调/
// 秒杀 MQ 消息发布），非幂等操作不重试；与退避共同吸收瞬时基础设施故障。
type Retry struct {
	// Attempts 总尝试次数（含首次）；<=1 表示不重试（单次执行）。
	Attempts int `mapstructure:"attempts"`
	// InitialBackoff 首次重试前等待；每次翻倍。
	InitialBackoff time.Duration `mapstructure:"initial_backoff"`
	// MaxBackoff 退避上限（指数增长封顶）。
	MaxBackoff time.Duration `mapstructure:"max_backoff"`
}

// CORS HTTP 跨源配置（T26 部署双路径）：云端前端与后端不同源时，
// 中间件按该白名单放行（platform/cors）。
type CORS struct {
	// AllowOrigins 允许跨源访问的前端 Origin 白名单；空 = 允许所有
	// （演示取舍，与 ws.allow_origins 语义一致，生产应配置前端域名）。
	AllowOrigins []string `mapstructure:"allow_origins"`
}

// WS WebSocket 实时通道配置（T18）：长连接心跳保活与写超时参数。
type WS struct {
	// HeartbeatInterval 心跳 Ping 间隔（保活；客户端 pong_wait = 2× 间隔内
	// 未收到任何帧即判定断开）。
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
	// WriteWait 单条写（业务消息/Ping）超时。
	WriteWait time.Duration `mapstructure:"write_wait"`
	// AllowOrigins 握手 Origin 白名单；空 = 允许所有（演示取舍，生产应配置前端域名）。
	AllowOrigins []string `mapstructure:"allow_origins"`
}

// FlashSale 秒杀配置：抢购接口限流参数。
// 全局令牌桶为进程内单实例（x/time/rate）；按用户为 Redis 固定窗口计数（跨请求状态）。
type FlashSale struct {
	// QPS 全局令牌桶每秒令牌补充速率。
	QPS float64 `mapstructure:"qps"`
	// Burst 全局令牌桶容量（允许的瞬时突发请求数）。
	Burst int `mapstructure:"burst"`
	// PerUserMax 每用户窗口内最多抢购请求数（<=0 表示不启用按用户限流）。
	PerUserMax int `mapstructure:"per_user_max"`
	// PerUserWindow 按用户限流窗口长度（如 "1s"）。
	PerUserWindow time.Duration `mapstructure:"per_user_window"`
}

type Auth struct {
	// Secret JWT HS256 签名密钥（生产环境用环境变量 GO_SINGLE_AUTH_SECRET 注入）。
	Secret string
	// TTL 令牌有效期，如 "2h"。
	TTL time.Duration
}

// Snowflake 雪花订单号生成器配置；多实例部署时每个实例必须使用不同 worker_id。
type Snowflake struct {
	WorkerID int64 `mapstructure:"worker_id"`
}

type Server struct {
	Port int
	Mode string
	// RequestTimeout 单请求全链路超时（T20）：HTTP handler → service → 存储/MQ
	// 逐层传递同一 ctx，依赖超时时快速失败（504），不挂起连接。
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
	// TrustedProxies 可信反代 IP 白名单（gin SetTrustedProxies）：
	// 命中才采信 X-Forwarded-For/X-Real-IP 得到真实客户端 IP（日志/指标）。
	// 默认信任本机（Nginx 容器经 host-gateway 转发）；云部署时改为反代出口 IP。
	// 空 = 不信任任何代理头（ClientIP 返回直连地址，日志失真但更安全）。
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// Log 日志配置：zap 结构化 JSON 输出。
type Log struct {
	// Level 日志级别（debug/info/warn/error）。
	Level string
	// File 非空时把日志行镜像写入该文件（供 promtail 采集进 Loki）；空 = 仅 stdout。
	File string
}

type MySQL struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

func (m MySQL) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		m.User, m.Password, m.Host, m.Port, m.Database)
}

type Redis struct {
	Addr     string
	Password string
	DB       int
}

type MQ struct {
	URL string
	// Circuit MQ 消费者熔断（T20，gobreaker）：连续失败打开 → 快速失败，
	// 冷却期后半开探活，恢复即闭合。仅包消费者，发布/进程内调用不包。
	Circuit Circuit `mapstructure:"circuit"`
}

// Circuit 熔断器参数（gobreaker.Settings 映射）。
type Circuit struct {
	// MaxConsecutiveFailures 连续失败阈值：达到后熔断打开（ReadyToTrip）。
	MaxConsecutiveFailures int `mapstructure:"max_consecutive_failures"`
	// Interval 关闭态失败计数清零周期；<=0 表示不按时间清零。
	Interval time.Duration `mapstructure:"interval"`
	// Timeout 熔断打开后的冷却时长，到点进入半开（放行一次探活）。
	Timeout time.Duration `mapstructure:"timeout"`
}

// MinIO 对象存储配置：私有桶 + 后端代理上传（前端不直连，presigned 不做）。
type MinIO struct {
	// Endpoint 服务地址（本地 compose 为 127.0.0.1:19000）。
	Endpoint string `mapstructure:"endpoint"`
	// AccessKey / SecretKey 管理员凭据（演示环境固定，生产用环境变量注入）。
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	// Bucket 私有桶名，上传与读取均经后端业务接口代理。
	Bucket string `mapstructure:"bucket"`
	// UseSSL 是否启用 TLS（本地 compose 为 false）。
	UseSSL bool `mapstructure:"use_ssl"`
}

type Migrations struct {
	Path string
}

// Load 加载配置：yaml 为基座，环境变量（GO_SINGLE_*）优先。
func Load() (*Config, error) {
	return LoadFrom("./configs", ".")
}

// LoadFrom 同 Load，但指定配置搜索路径（测试可指向仓库根）。
func LoadFrom(paths ...string) (*Config, error) {
	v := viper.New()
	v.SetConfigName("config.yaml")
	v.SetConfigType("yaml")
	for _, p := range paths {
		v.AddConfigPath(p)
	}
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	setDefaults(v)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.mode", "debug")
	v.SetDefault("server.request_timeout", "5s")
	v.SetDefault("log.level", "info")
	v.SetDefault("mysql.host", "127.0.0.1")
	v.SetDefault("mysql.port", 3306)
	v.SetDefault("mysql.user", "root")
	v.SetDefault("mysql.password", "")
	v.SetDefault("mysql.database", "go_shop")
	v.SetDefault("redis.addr", "127.0.0.1:6379")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("mq.url", "amqp://guest:guest@127.0.0.1:5672/")
	v.SetDefault("mq.circuit.max_consecutive_failures", 3)
	v.SetDefault("mq.circuit.interval", "30s")
	v.SetDefault("mq.circuit.timeout", "10s")
	v.SetDefault("minio.endpoint", "127.0.0.1:19000")
	v.SetDefault("minio.access_key", "minioadmin")
	v.SetDefault("minio.secret_key", "minioadmin")
	v.SetDefault("minio.bucket", "go-shop")
	v.SetDefault("minio.use_ssl", false)
	v.SetDefault("migrations.path", "./migrations")
	v.SetDefault("auth.secret", "dev-secret-change-me")
	v.SetDefault("auth.ttl", "2h")
	v.SetDefault("snowflake.worker_id", 1)
	v.SetDefault("flashsale.qps", 50)
	v.SetDefault("flashsale.burst", 100)
	v.SetDefault("flashsale.per_user_max", 5)
	v.SetDefault("flashsale.per_user_window", "1s")
	v.SetDefault("retry.attempts", 3)
	v.SetDefault("retry.initial_backoff", "100ms")
	v.SetDefault("retry.max_backoff", "1s")
	v.SetDefault("ws.heartbeat_interval", "30s")
	v.SetDefault("ws.write_wait", "10s")
	v.SetDefault("ws.allow_origins", []string{})
	v.SetDefault("cors.allow_origins", []string{})
}
