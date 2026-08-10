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
	Migrations Migrations
	Auth       Auth
}

type Auth struct {
	// Secret JWT HS256 签名密钥（生产环境用环境变量 GO_SINGLE_AUTH_SECRET 注入）。
	Secret string
	// TTL 令牌有效期，如 "2h"。
	TTL time.Duration
}

type Server struct {
	Port int
	Mode string
}

type Log struct {
	Level string
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
	v.SetDefault("migrations.path", "./migrations")
	v.SetDefault("auth.secret", "dev-secret-change-me")
	v.SetDefault("auth.ttl", "2h")
}
