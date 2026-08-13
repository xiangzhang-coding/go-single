// Q3 配置管理：默认值 + 文件 + 环境变量覆盖。
// 运行：go run ./interview/ch09_engineering/q03_config
package main

import (
	"fmt"
	"os"
	"strconv"
)

// 项目用 viper（configs/config.yaml + GO_SINGLE_ 前缀环境变量覆盖），
// 这里用标准库演示同款"默认值 → 环境变量"优先级。
type config struct {
	Server   serverConfig
	RequestTimeoutSeconds int
}

type serverConfig struct {
	Port int
}

func loadConfig() config {
	c := config{
		Server:                serverConfig{Port: 8080},
		RequestTimeoutSeconds: 5, // 默认值（config.yaml server.request_timeout: 5s）
	}
	if v := os.Getenv("GO_SINGLE_SERVER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.Port = n
		}
	}
	return c
}

func main() {
	// 演示环境变量覆盖。
	_ = os.Setenv("GO_SINGLE_SERVER_PORT", "9090")
	c := loadConfig()
	fmt.Printf("port=%d（被 GO_SINGLE_SERVER_PORT 覆盖）\n", c.Server.Port)

	// 项目默认：viper 的 . 号替换成 _ 命名（server.request_timeout → GO_SINGLE_SERVER_REQUEST_TIMEOUT），
	// AutomaticEnv 使未显式读的键也能被环境变量命中。
	fmt.Println("12-factor：配置外置，不写死在代码里")
}

// 项目位置：internal/platform/config/config.go——Load/LoadFrom、setDefaults、
// env 前缀 GO_SINGLE + 点号替换 + AutomaticEnv（158-185）；配置文件 configs/config.yaml。
